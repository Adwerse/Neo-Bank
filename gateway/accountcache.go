package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"neobank/pkg/tracing"
)

// accountLookupTimeout bounds the fallback HTTP call to accounts-svc on a
// cache miss, so a hung accounts-svc can't stall a Kafka consumer's
// processing loop indefinitely.
const accountLookupTimeout = 3 * time.Second

type accountCacheEntry struct {
	userID    string
	expiresAt time.Time
}

// accountCache is the gateway's local, in-memory map from account_id to
// the user_id that owns it — events carry account ids, but the WS
// registry is keyed by user_id, so something has to bridge the two.
//
// TTL here is not about staleness: account ownership doesn't change once
// an account exists. It exists purely to bound memory over a long-lived
// process — an evicted entry costs one extra fallback HTTP call the next
// time that account_id shows up in an event, nothing more.
type accountCache struct {
	mu           sync.RWMutex
	entries      map[string]accountCacheEntry
	ttl          time.Duration
	accountsAddr string
	httpClient   *http.Client
}

func newAccountCache(ttl time.Duration, accountsAddr string) *accountCache {
	return &accountCache{
		entries:      make(map[string]accountCacheEntry),
		ttl:          ttl,
		accountsAddr: accountsAddr,
		// Traced transport so a cache-miss lookup shows up as a child
		// span of whatever triggered it, rather than as unexplained
		// latency inside a Kafka consumer. Note this call originates from
		// a consumer goroutine, not an inbound HTTP request, so today it
		// usually has no parent span to attach to — the async side is the
		// next sprint's problem. Instrumenting it now costs nothing and
		// means it joins the trace automatically once the consumer
		// propagates context.
		httpClient: &http.Client{
			Timeout:   accountLookupTimeout,
			Transport: tracing.Transport(nil),
		},
	}
}

func (c *accountCache) set(accountID, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[accountID] = accountCacheEntry{userID: userID, expiresAt: time.Now().Add(c.ttl)}
}

func (c *accountCache) get(accountID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[accountID]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.userID, true
}

// resolve returns the user_id owning accountID, consulting the cache
// first and falling back to a direct call to accounts-svc's GET /{id} on
// a miss (populating the cache on success). That endpoint
// (services/accounts-svc/http.go) performs no auth or ownership check at
// all — same as every other internal service-to-service call in this
// repo, none of which are authenticated. This reuses that existing gap
// rather than introducing a new one.
func (c *accountCache) resolve(ctx context.Context, accountID string) (string, bool) {
	if userID, ok := c.get(accountID); ok {
		return userID, true
	}

	userID, ok := c.fetchFromAccountsSvc(ctx, accountID)
	if !ok {
		return "", false
	}
	c.set(accountID, userID)
	return userID, true
}

func (c *accountCache) fetchFromAccountsSvc(ctx context.Context, accountID string) (string, bool) {
	reqURL := "http://" + c.accountsAddr + "/" + url.PathEscape(accountID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		log.Printf("gateway: account lookup: build request for %s: %v", accountID, err)
		return "", false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("gateway: account lookup: request for %s: %v", accountID, err)
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("gateway: account lookup: %s returned status %d", accountID, resp.StatusCode)
		return "", false
	}

	var body struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("gateway: account lookup: decode response for %s: %v", accountID, err)
		return "", false
	}
	if body.UserID == "" {
		return "", false
	}
	return body.UserID, true
}
