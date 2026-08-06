package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	defaultWSAuthTimeout       = 5 * time.Second
	defaultWSHeartbeatInterval = 30 * time.Second
	defaultWSPingTimeout       = 10 * time.Second
	defaultWSMaxConnsPerUser   = 5

	// wsCloseTooManyConns is an application-defined close code (RFC 6455
	// reserves 4000-4999 for private use) for the one rejection reason
	// that isn't really a protocol violation — the client did everything
	// right, there's just no room for another connection.
	wsCloseTooManyConns = websocket.StatusCode(4001)

	// wsSendTimeout bounds a single push write in wsRegistry.send, so one
	// slow or half-dead client (not yet reaped by the heartbeat) can't
	// hold up delivery to anyone else.
	wsSendTimeout = 5 * time.Second
)

type wsAuthMessage struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// wsRegistry is the in-memory map of user_id -> active connections. A
// plain mutex over a nested map, not sync.Map: add() needs an atomic
// "check count, then insert" that sync.Map doesn't give for free. This is
// also the shape the Kafka consumer (kafka.go) reads from to fan events
// out to a user's local sockets via send, below.
type wsRegistry struct {
	mu     sync.Mutex
	byUser map[string]map[*websocket.Conn]struct{}
}

func newWSRegistry() *wsRegistry {
	return &wsRegistry{byUser: make(map[string]map[*websocket.Conn]struct{})}
}

// add registers conn under userID and reports whether it fit under max. On
// false, conn is NOT registered — the caller is responsible for closing it.
func (r *wsRegistry) add(userID string, conn *websocket.Conn, max int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	conns := r.byUser[userID]
	if len(conns) >= max {
		return false
	}
	if conns == nil {
		conns = make(map[*websocket.Conn]struct{})
		r.byUser[userID] = conns
	}
	conns[conn] = struct{}{}
	return true
}

func (r *wsRegistry) remove(userID string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	conns := r.byUser[userID]
	delete(conns, conn)
	if len(conns) == 0 {
		delete(r.byUser, userID)
	}
}

// send delivers msg to every connection currently registered for userID
// on this instance. A user with no local connections — the common case,
// since most events on most instances belong to a user connected
// (if at all) to some other instance — is a silent no-op, not logged:
// logging it would spam the log on every instance for every event that
// isn't locally relevant. The connection snapshot is taken under the
// lock and the actual writes happen after releasing it, so one slow
// client's write can't block registry operations (new connections
// registering, the heartbeat) for everyone else.
func (r *wsRegistry) send(ctx context.Context, userID string, msg any) {
	r.mu.Lock()
	conns := r.byUser[userID]
	snapshot := make([]*websocket.Conn, 0, len(conns))
	for c := range conns {
		snapshot = append(snapshot, c)
	}
	r.mu.Unlock()

	for _, c := range snapshot {
		writeCtx, cancel := context.WithTimeout(ctx, wsSendTimeout)
		err := wsjson.Write(writeCtx, c, msg)
		cancel()
		if err != nil {
			log.Printf("gateway: ws push failed user=%s: %v", userID, err)
		}
	}
}

// wsServer holds GET /ws's config and the connection registry it shares
// across every upgraded connection.
type wsServer struct {
	shutdownCtx     context.Context
	secret          string
	registry        *wsRegistry
	authTimeout     time.Duration
	heartbeat       time.Duration
	pingTimeout     time.Duration
	maxConnsPerUser int
}

func newWSServer(ctx context.Context, secret string) *wsServer {
	return &wsServer{
		shutdownCtx:     ctx,
		secret:          secret,
		registry:        newWSRegistry(),
		authTimeout:     envDurationOr("WS_AUTH_TIMEOUT", defaultWSAuthTimeout),
		heartbeat:       envDurationOr("WS_HEARTBEAT_INTERVAL", defaultWSHeartbeatInterval),
		pingTimeout:     envDurationOr("WS_PING_TIMEOUT", defaultWSPingTimeout),
		maxConnsPerUser: envIntOr("WS_MAX_CONNS_PER_USER", defaultWSMaxConnsPerUser),
	}
}

// handleWS implements GET /ws end to end: upgrade, an unauthenticated
// window bounded by authTimeout during which nothing is sent to the
// client, registry-backed auth + a per-user connection cap, a heartbeat
// that reaps dead connections, and a clean close on server shutdown.
func (s *wsServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("gateway: ws accept failed: %v", err)
		return
	}

	// Unauthenticated phase: read exactly one message, bounded by
	// authTimeout. Nothing is written to conn before auth succeeds — an
	// unauthenticated socket held open for free (no timeout) would be a
	// way to exhaust memory with connections that never prove who they
	// are.
	authCtx, cancel := context.WithTimeout(r.Context(), s.authTimeout)
	var msg wsAuthMessage
	err = wsjson.Read(authCtx, conn, &msg)
	cancel()
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "no valid auth message received in time")
		log.Printf("gateway: ws closed — no auth message: %v", err)
		return
	}
	if msg.Type != "auth" || msg.Token == "" {
		conn.Close(websocket.StatusPolicyViolation, "expected an auth message first")
		log.Printf("gateway: ws closed — first message was not an auth message")
		return
	}
	claims, err := parseAccessToken(msg.Token, s.secret)
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "invalid or expired token")
		log.Printf("gateway: ws closed — auth failed: %v", err)
		return
	}
	userID := claims.UserID

	if !s.registry.add(userID, conn, s.maxConnsPerUser) {
		conn.Close(wsCloseTooManyConns, "too many connections for this user")
		log.Printf("gateway: ws rejected — user %s already at the %d connection limit", userID, s.maxConnsPerUser)
		return
	}
	log.Printf("gateway: ws connection registered user=%s", userID)
	defer func() {
		s.registry.remove(userID, conn)
		log.Printf("gateway: ws connection removed user=%s", userID)
	}()

	if err := wsjson.Write(r.Context(), conn, map[string]string{"type": "connected"}); err != nil {
		log.Printf("gateway: ws ack write failed user=%s: %v", userID, err)
		return
	}

	// From here on this is a push-only channel: no more data messages are
	// expected from the client. CloseRead hands control-frame handling
	// (ping/pong/close) to the library and closes the connection itself
	// if a data frame does arrive.
	readCtx := conn.CloseRead(r.Context())

	heartbeatCtx, cancelHeartbeat := context.WithCancel(readCtx)
	defer cancelHeartbeat()
	go s.runHeartbeat(heartbeatCtx, conn, userID)

	select {
	case <-readCtx.Done():
	case <-s.shutdownCtx.Done():
		conn.Close(websocket.StatusServiceRestart, "server shutting down")
	}
}

// runHeartbeat pings conn every s.heartbeat and closes it if a pong
// doesn't arrive within s.pingTimeout (task 4) — without this, a client
// that vanished without sending a close frame (the normal case for a lost
// network) would sit in the registry forever, leaking memory.
func (s *wsServer) runHeartbeat(ctx context.Context, conn *websocket.Conn, userID string) {
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, s.pingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Printf("gateway: ws heartbeat failed user=%s, closing: %v", userID, err)
				conn.Close(websocket.StatusPolicyViolation, "ping timeout")
				return
			}
		}
	}
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("gateway: invalid %s %q: %v", key, v, err)
	}
	return d
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("gateway: invalid %s %q: %v", key, v, err)
	}
	return n
}
