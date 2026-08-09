// Package pgha holds the small amount of application-side code that a
// Postgres cluster with automatic failover needs and a single Postgres
// instance did not.
//
// The premise: with Patroni in front of the nodes and HAProxy in front of
// Patroni (see infra/patroni/, infra/haproxy/haproxy.cfg), a failover is
// not a special event the application handles — it is a few seconds
// during which connections break and queries fail, followed by normal
// service against a different node at the same address. Nothing here
// tries to hide that window. What it does is make sure that when the
// window closes, every part of the process recovers on its own:
//
//   - pools drop connections to the departed leader promptly and refill
//     against the new one, instead of handing out dead ones (NewPool);
//   - startup work that must talk to a leader waits for one instead of
//     crash-looping on its absence (Retry, WaitForWritable);
//   - long-lived loops can tell "the database is briefly unavailable"
//     apart from "this message is poison", so a failover does not get
//     mistaken for a permanent failure (IsUnavailable, in errors.go).
package pgha

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// connectTimeout bounds a single dial. Without it pgx inherits the
	// OS TCP timeout, which on a node that is gone rather than refusing
	// can be minutes — long enough that a "brief" failover looks to the
	// application like a total outage.
	connectTimeout = 5 * time.Second

	// healthCheckPeriod is how often pgxpool's background goroutine
	// inspects idle connections and destroys the broken ones. pgx's
	// default is a minute; that is fine when connections only ever break
	// because a network blipped, and much too slow when a node can be
	// deposed. Note this only reaches IDLE connections — a connection
	// checked out and in use fails on use, which is why HAProxy's
	// on-marked-down shutdown-sessions matters as much as this does.
	healthCheckPeriod = 10 * time.Second

	// maxConnLifetime/Jitter recycle connections steadily so that after a
	// failover the pool does not stay permanently pinned to whichever
	// nodes it happened to reach first — relevant once the old leader
	// returns as a standby and the read pool should start using it again.
	// The jitter keeps every connection in every service from expiring in
	// the same instant and stampeding the new leader.
	maxConnLifetime       = 30 * time.Minute
	maxConnLifetimeJitter = 5 * time.Minute

	// maxConnIdleTime keeps the pool from holding a large number of idle
	// connections open across a failover, each of which would have to be
	// discovered broken separately.
	maxConnIdleTime = 5 * time.Minute
)

// NewPool builds a connection pool tuned for a cluster whose leader can
// change underneath it.
//
// It deliberately does NOT verify the database is reachable: like
// pgxpool.New, it establishes nothing here and dials on first use. That
// laziness is a feature during failover — a service restarted in the
// middle of one gets a working pool that starts succeeding the moment a
// leader exists, rather than failing to construct. Callers that genuinely
// need a live leader before proceeding (migrations, mainly) should follow
// this with WaitForWritable.
//
// Settings in the DSN win: ParseConfig reads the URL first and the
// adjustments below only fill in values the URL did not specify, so
// pool_max_conns and friends remain usable from a connection string.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	if config.ConnConfig.ConnectTimeout == 0 {
		config.ConnConfig.ConnectTimeout = connectTimeout
	}
	if config.HealthCheckPeriod == 0 || config.HealthCheckPeriod > healthCheckPeriod {
		config.HealthCheckPeriod = healthCheckPeriod
	}
	if config.MaxConnLifetime == 0 || config.MaxConnLifetime > maxConnLifetime {
		config.MaxConnLifetime = maxConnLifetime
	}
	if config.MaxConnLifetimeJitter == 0 {
		config.MaxConnLifetimeJitter = maxConnLifetimeJitter
	}
	if config.MaxConnIdleTime == 0 || config.MaxConnIdleTime > maxConnIdleTime {
		config.MaxConnIdleTime = maxConnIdleTime
	}

	// MinConns is left at zero on purpose. A non-zero MinConns makes the
	// health-check goroutine keep re-dialling to top the pool back up,
	// which during a failover is a steady stream of failing connection
	// attempts and log noise for no benefit — there is no work waiting on
	// those connections, and the pool opens what it needs on demand
	// anyway.

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	return pool, nil
}

// WaitForWritable blocks until pool answers a query on a node that is
// actually the leader, or until ctx is cancelled.
//
// "Answers a query" is not enough on its own: HAProxy routes port 5432 to
// whichever node Patroni calls leader, but a node can be demoted between
// HAProxy's health check and this query landing, and a demoted node still
// accepts connections — it just refuses to write. So the probe asks
// pg_is_in_recovery() rather than SELECT 1, and treats "in recovery" as
// not-ready. That turns "I can reach a database" into "I can reach the
// database that accepts writes", which is what the caller actually needs
// before running migrations against it.
func WaitForWritable(ctx context.Context, pool *pgxpool.Pool, logf func(string, ...any)) error {
	return Retry(ctx, "wait for writable primary", logf, func(attemptCtx context.Context) error {
		var inRecovery bool
		if err := pool.QueryRow(attemptCtx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
			return err
		}
		if inRecovery {
			return errStandby
		}
		return nil
	})
}
