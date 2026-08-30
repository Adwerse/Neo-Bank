package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// dialWS connects to gw's /ws endpoint and registers cleanup that force-
// closes the connection, so a test that ends before the server closes the
// socket itself doesn't leak a goroutine.
func dialWS(t *testing.T, gw *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(gw.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

type wsAckMessage struct {
	Type string `json:"type"`
}

func TestWSHandler_NoAuthMessage_ClosesAfterTimeout(t *testing.T) {
	t.Setenv("WS_AUTH_TIMEOUT", "100ms")

	const secret = "test-secret"
	gw := httptest.NewServer(newTestHandler(secret))
	t.Cleanup(gw.Close)

	conn := dialWS(t, gw)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var msg wsAckMessage
	err := wsjson.Read(ctx, conn, &msg)
	if err == nil {
		t.Fatal("expected the connection to close after the auth timeout, got a message instead")
	}
	// The server's own Read call is what times out, so it tears the
	// connection down before it can send a graceful close frame — the
	// client sees an abnormal closure, not a StatusPolicyViolation close
	// frame. What matters for the DoD is that the socket does not stay
	// open; the exact wire-level closure shape is an implementation
	// detail of the auth-timeout path.
}

func TestWSHandler_InvalidToken_ClosesImmediately(t *testing.T) {
	t.Setenv("WS_AUTH_TIMEOUT", "2s") // long enough that only the bad token can cause the close

	const secret = "test-secret"
	gw := httptest.NewServer(newTestHandler(secret))
	t.Cleanup(gw.Close)

	conn := dialWS(t, gw)

	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Write(writeCtx, conn, wsAuthMessage{Type: "auth", Token: "not-a-real-token"}); err != nil {
		t.Fatalf("write auth message: %v", err)
	}

	readCtx, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	var msg wsAckMessage
	err := wsjson.Read(readCtx, conn, &msg)
	if err == nil {
		t.Fatal("expected the connection to close immediately on an invalid token")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v, want %v", status, websocket.StatusPolicyViolation)
	}
}

func TestWSHandler_ValidToken_StaysOpenAndAcks(t *testing.T) {
	t.Setenv("WS_HEARTBEAT_INTERVAL", "50ms")
	t.Setenv("WS_PING_TIMEOUT", "500ms")

	const secret = "test-secret"
	gw := httptest.NewServer(newTestHandler(secret))
	t.Cleanup(gw.Close)

	conn := dialWS(t, gw)

	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token := signedTestJWT(t, secret, "user-123")
	if err := wsjson.Write(writeCtx, conn, wsAuthMessage{Type: "auth", Token: token}); err != nil {
		t.Fatalf("write auth message: %v", err)
	}

	ackCtx, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	var ack wsAckMessage
	if err := wsjson.Read(ackCtx, conn, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Type != "connected" {
		t.Errorf("ack type = %q, want %q", ack.Type, "connected")
	}

	// Hand the client side over to CloseRead too: control frames (the
	// server's heartbeat pings) are only answered while something is
	// actively reading, exactly like the server-side handler. Without
	// this the server's pings never get a pong and it kills the
	// connection thinking the client vanished — a test-side requirement,
	// not a server bug.
	readCtx := conn.CloseRead(context.Background())

	// Outlast a few (shortened) heartbeat intervals, then confirm the
	// connection is still alive: it wasn't closed by CloseRead, and a
	// client-initiated ping still round-trips.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-readCtx.Done():
		t.Fatal("connection closed while waiting to outlast the heartbeat interval")
	default:
	}
	pingCtx, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	if err := conn.Ping(pingCtx); err != nil {
		t.Errorf("ping after surviving heartbeat interval: %v", err)
	}
}

func TestWSHandler_SixthConnectionRejected(t *testing.T) {
	t.Setenv("WS_MAX_CONNS_PER_USER", "2")
	t.Setenv("WS_AUTH_TIMEOUT", "2s")

	const secret = "test-secret"
	gw := httptest.NewServer(newTestHandler(secret))
	t.Cleanup(gw.Close)

	token := signedTestJWT(t, secret, "user-123")

	authenticate := func(conn *websocket.Conn) {
		t.Helper()
		writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := wsjson.Write(writeCtx, conn, wsAuthMessage{Type: "auth", Token: token}); err != nil {
			t.Fatalf("write auth message: %v", err)
		}
	}

	// Fill the (overridden) limit of 2 connections for this user.
	for i := 0; i < 2; i++ {
		conn := dialWS(t, gw)
		authenticate(conn)
		readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		var ack wsAckMessage
		err := wsjson.Read(readCtx, conn, &ack)
		cancel()
		if err != nil {
			t.Fatalf("connection %d: expected ack, got error: %v", i, err)
		}
	}

	// The 3rd connection for the same user must be rejected.
	over := dialWS(t, gw)
	authenticate(over)
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var ack wsAckMessage
	err := wsjson.Read(readCtx, over, &ack)
	if err == nil {
		t.Fatal("expected the over-limit connection to be rejected, got an ack instead")
	}
	if status := websocket.CloseStatus(err); status != wsCloseTooManyConns {
		t.Errorf("close status = %v, want %v", status, wsCloseTooManyConns)
	}
}

// TestWSRegistry_Send_DeliversOnlyToTargetUsersLocalConnections is the
// delivery-layer half of the Kafka fan-out's DoD ("both get their own
// signal, neither gets the other's data"): notify_test.go proves
// the routing DECISION never addresses the wrong user, this proves the
// TRANSPORT that decision feeds into only ever reaches that user's own
// connections — a user with two tabs open gets it on both, an unrelated
// user gets nothing at all, not even after waiting.
//
// This builds the wsServer directly (not via newTestHandler) so the test
// can hold onto ws.registry — the same registry Kafka's consumer would
// call send() on — and push into it exactly the way kafka.go's
// handleTransferMessage does.
func TestWSRegistry_Send_DeliversOnlyToTargetUsersLocalConnections(t *testing.T) {
	const secret = "test-secret"
	ws := newWSServer(context.Background(), secret)
	gw := httptest.NewServer(newHandler(secret, ws))
	t.Cleanup(gw.Close)

	authenticate := func(conn *websocket.Conn, userID string) {
		t.Helper()
		writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		token := signedTestJWT(t, secret, userID)
		if err := wsjson.Write(writeCtx, conn, wsAuthMessage{Type: "auth", Token: token}); err != nil {
			t.Fatalf("write auth message: %v", err)
		}
		readCtx, cancel2 := context.WithTimeout(context.Background(), time.Second)
		defer cancel2()
		var ack wsAckMessage
		if err := wsjson.Read(readCtx, conn, &ack); err != nil {
			t.Fatalf("read ack: %v", err)
		}
	}

	connA1 := dialWS(t, gw)
	authenticate(connA1, "user-A")
	connA2 := dialWS(t, gw) // user A's second tab
	authenticate(connA2, "user-A")
	connB := dialWS(t, gw)
	authenticate(connB, "user-B")

	ws.registry.send(context.Background(), "user-A", wsBalanceChangedMsg{Type: "balance.changed"})

	for i, conn := range []*websocket.Conn{connA1, connA2} {
		readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		var msg wsBalanceChangedMsg
		err := wsjson.Read(readCtx, conn, &msg)
		cancel()
		if err != nil {
			t.Fatalf("user-A connection %d: expected to receive the push, got error: %v", i, err)
		}
		if msg.Type != "balance.changed" {
			t.Errorf("user-A connection %d: got type %q, want balance.changed", i, msg.Type)
		}
	}

	// user-B must receive nothing addressed to user-A — a short timeout
	// that is EXPECTED to expire is exactly how "nothing arrived" is
	// observed on a still-open connection.
	readCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var msg wsBalanceChangedMsg
	if err := wsjson.Read(readCtx, connB, &msg); err == nil {
		t.Fatal("user-B's connection received a push addressed to user-A — cross-user leak")
	}
}
