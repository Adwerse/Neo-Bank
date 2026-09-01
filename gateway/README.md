# gateway

The single entry point. Terminates JWTs, reverse-proxies every route to its
owning service by path prefix, and holds the WebSocket connection registry.

**Owns:** nothing persistent — no database, no domain data. JWT validation
(`jwtMiddleware`), the reverse-proxy routing table, the WebSocket connection
registry (`wsRegistry`), and the per-instance Kafka consumer group that turns
`transfer.events`/`account.events` into WS pushes.

**API:** proxies every other service's HTTP surface unchanged
(`/auth/*`, `/accounts/*`, `/transfers/*`, `/deposits`, `/profile*`,
`/webhooks/stripe`) plus its own `GET /ws` WebSocket endpoint (auth by first
message, not the handshake).

**Consumes:** `transfer.events`, `account.events` — read with a fresh,
unique consumer group per instance (`LastOffset`), turned into
`{"type":"balance.changed"}`-style signals and pushed to the WS connections
of the one user each event names, never broadcast.

Full request-routing and WebSocket rationale: root [README](../README.md),
"Engineering highlights" and the Gateway sections further down.
