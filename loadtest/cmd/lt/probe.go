package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// probeQuery is one round trip that answers every question a "where did the
// latency come from?" post-mortem starts with. It is a single statement of
// scalar subqueries rather than seven separate queries because the sample
// has to be a coherent snapshot: connection count and lock waiters read a
// second apart describe two different moments, and the interesting cases
// are exactly the ones where a second is a long time.
//
// What each column is for:
//
//   - conns_* — pool exhaustion, seen from the only side that can see it.
//     Postgres cannot observe a goroutine blocked waiting for a connection
//     from pgxpool; what it can show is the pool pinned at its ceiling
//     while latency climbs, which is the same finding from the other end.
//   - lock_waiters / locks_ungranted — row-lock contention, i.e. the hot
//     account profile's entire thesis, measured rather than asserted.
//   - outbox_backlog — whether the relay publishes as fast as handlers
//     write. A backlog that grows monotonically through a run and drains
//     afterwards is relay lag; one that never drains is a stuck relay.
//   - transfers_pending — transfers that reached neither completed nor
//     failed. Under load this is the settlement-uncertain path, and it is
//     what the reconciliation worker later has to resolve.
//   - replica_lag_* — write pressure on the primary showing up as standby
//     lag. With synchronous_mode on (infra/patroni/patroni.yml), the sync
//     standby's flush lag is not just an observability metric: it is
//     directly inside every commit's latency.
const probeQuery = `
SELECT
  (SELECT count(*) FROM pg_stat_activity WHERE datname = 'neobank')                                        AS conns_total,
  (SELECT count(*) FROM pg_stat_activity WHERE datname = 'neobank' AND state = 'active')                   AS conns_active,
  (SELECT count(*) FROM pg_stat_activity WHERE datname = 'neobank' AND state = 'idle in transaction')      AS conns_idle_in_txn,
  (SELECT count(*) FROM pg_stat_activity WHERE datname = 'neobank' AND wait_event_type = 'Lock')           AS lock_waiters,
  (SELECT count(*) FROM pg_locks WHERE NOT granted)                                                        AS locks_ungranted,
  (SELECT count(*) FROM outbox WHERE published_at IS NULL)                                                 AS outbox_backlog,
  (SELECT count(*) FROM transfers WHERE status = 'pending')                                                AS transfers_pending,
  (SELECT COALESCE(max(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)), 0)::bigint
     FROM pg_stat_replication)                                                                             AS replica_lag_bytes,
  (SELECT COALESCE(max(extract(epoch FROM replay_lag)), 0)::float8 FROM pg_stat_replication)               AS replica_lag_seconds,
  (SELECT COALESCE(max(extract(epoch FROM flush_lag)), 0)::float8
     FROM pg_stat_replication WHERE sync_state = 'sync')                                                   AS sync_flush_lag_seconds,
  (SELECT COALESCE(max(extract(epoch FROM now() - xact_start)), 0)::float8
     FROM pg_stat_activity WHERE datname = 'neobank' AND xact_start IS NOT NULL)                           AS longest_txn_seconds,
  (SELECT count(*) FROM pg_stat_activity WHERE datname = 'neobank' AND state = 'active' AND now() - query_start > interval '1 second') AS queries_over_1s
`

type probeSample struct {
	At                  time.Time
	ConnsTotal          int64
	ConnsActive         int64
	ConnsIdleInTxn      int64
	LockWaiters         int64
	LocksUngranted      int64
	OutboxBacklog       int64
	TransfersPending    int64
	ReplicaLagBytes     int64
	ReplicaLagSeconds   float64
	SyncFlushLagSeconds float64
	LongestTxnSeconds   float64
	QueriesOver1s       int64
}

var probeColumns = []string{
	"elapsed_s", "conns_total", "conns_active", "conns_idle_in_txn",
	"lock_waiters", "locks_ungranted", "outbox_backlog", "transfers_pending",
	"replica_lag_bytes", "replica_lag_seconds", "sync_flush_lag_seconds",
	"longest_txn_seconds", "queries_over_1s",
}

func (s probeSample) row(start time.Time) []string {
	return []string{
		strconv.FormatFloat(s.At.Sub(start).Seconds(), 'f', 2, 64),
		strconv.FormatInt(s.ConnsTotal, 10),
		strconv.FormatInt(s.ConnsActive, 10),
		strconv.FormatInt(s.ConnsIdleInTxn, 10),
		strconv.FormatInt(s.LockWaiters, 10),
		strconv.FormatInt(s.LocksUngranted, 10),
		strconv.FormatInt(s.OutboxBacklog, 10),
		strconv.FormatInt(s.TransfersPending, 10),
		strconv.FormatInt(s.ReplicaLagBytes, 10),
		strconv.FormatFloat(s.ReplicaLagSeconds, 'f', 3, 64),
		strconv.FormatFloat(s.SyncFlushLagSeconds, 'f', 4, 64),
		strconv.FormatFloat(s.LongestTxnSeconds, 'f', 3, 64),
		strconv.FormatInt(s.QueriesOver1s, 10),
	}
}

func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	duration := fs.Duration("duration", 0, "how long to sample; 0 means until interrupted")
	interval := fs.Duration("interval", time.Second, "sampling interval")
	databaseURL := fs.String("database-url", envOr("DATABASE_URL", defaultDatabaseURL), "Postgres URL of the current leader")
	out := fs.String("out", "loadtest/results/probe.csv", "CSV output path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The probe opens exactly one connection and holds it. Anything more
	// would be an observer that changes what it observes: a probe that
	// itself competes for pool slots during a connection-exhaustion test
	// is worse than no probe.
	conn, err := pgx.Connect(ctx, *databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("create results directory: %w", err)
	}
	f, err := os.Create(*out)
	if err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(probeColumns); err != nil {
		return err
	}

	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	start := time.Now()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	var samples []probeSample
sampling:
	for {
		s, err := sampleOnce(ctx, conn)
		if err != nil {
			// A failed sample is itself information (the database was
			// unreachable for a moment) and must not abort the run —
			// otherwise a probe is only useful when nothing is wrong.
			if ctx.Err() != nil {
				break sampling
			}
			log.Printf("probe: sample failed: %v", err)
		} else {
			samples = append(samples, s)
			if err := w.Write(s.row(start)); err != nil {
				return err
			}
			w.Flush()
		}

		select {
		case <-ctx.Done():
			break sampling
		case <-ticker.C:
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if len(samples) == 0 {
		return fmt.Errorf("probe collected no samples")
	}
	printProbeSummary(*out, samples)
	return nil
}

func sampleOnce(ctx context.Context, conn *pgx.Conn) (probeSample, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	s := probeSample{At: time.Now()}
	err := conn.QueryRow(queryCtx, probeQuery).Scan(
		&s.ConnsTotal, &s.ConnsActive, &s.ConnsIdleInTxn,
		&s.LockWaiters, &s.LocksUngranted, &s.OutboxBacklog, &s.TransfersPending,
		&s.ReplicaLagBytes, &s.ReplicaLagSeconds, &s.SyncFlushLagSeconds,
		&s.LongestTxnSeconds, &s.QueriesOver1s,
	)
	return s, err
}

// probeStats is the per-column reduction written next to the raw CSV. Peak
// matters more than mean for every one of these: a pool that is saturated
// for ten seconds out of sixty has a comfortable-looking average and a
// completely saturated moment, and it is the moment the p99 came from.
type probeStats struct {
	Column string
	Mean   float64
	P95    float64
	Max    float64
}

func summarizeProbe(samples []probeSample) []probeStats {
	series := map[string][]float64{}
	for _, s := range samples {
		series["conns_total"] = append(series["conns_total"], float64(s.ConnsTotal))
		series["conns_active"] = append(series["conns_active"], float64(s.ConnsActive))
		series["conns_idle_in_txn"] = append(series["conns_idle_in_txn"], float64(s.ConnsIdleInTxn))
		series["lock_waiters"] = append(series["lock_waiters"], float64(s.LockWaiters))
		series["locks_ungranted"] = append(series["locks_ungranted"], float64(s.LocksUngranted))
		series["outbox_backlog"] = append(series["outbox_backlog"], float64(s.OutboxBacklog))
		series["transfers_pending"] = append(series["transfers_pending"], float64(s.TransfersPending))
		series["replica_lag_bytes"] = append(series["replica_lag_bytes"], float64(s.ReplicaLagBytes))
		series["replica_lag_seconds"] = append(series["replica_lag_seconds"], s.ReplicaLagSeconds)
		series["sync_flush_lag_seconds"] = append(series["sync_flush_lag_seconds"], s.SyncFlushLagSeconds)
		series["longest_txn_seconds"] = append(series["longest_txn_seconds"], s.LongestTxnSeconds)
		series["queries_over_1s"] = append(series["queries_over_1s"], float64(s.QueriesOver1s))
	}

	out := make([]probeStats, 0, len(series))
	for _, col := range probeColumns[1:] { // skip elapsed_s
		values := series[col]
		if len(values) == 0 {
			continue
		}
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		var sum float64
		for _, v := range values {
			sum += v
		}
		out = append(out, probeStats{
			Column: col,
			Mean:   sum / float64(len(values)),
			P95:    percentile(sorted, 95),
			Max:    sorted[len(sorted)-1],
		})
	}
	return out
}

// percentile uses nearest-rank on an already-sorted slice. k6 interpolates;
// this does not, and the difference is irrelevant at these sample counts
// for values that are mostly small integers anyway.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p/100 + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func printProbeSummary(path string, samples []probeSample) {
	fmt.Printf("\nprobe: %d samples over %.0fs -> %s\n\n",
		len(samples), samples[len(samples)-1].At.Sub(samples[0].At).Seconds(), path)
	fmt.Printf("  %-22s %10s %10s %10s\n", "metric", "mean", "p95", "max")
	for _, s := range summarizeProbe(samples) {
		fmt.Printf("  %-22s %10.2f %10.2f %10.2f\n", s.Column, s.Mean, s.P95, s.Max)
	}
	fmt.Println()
}
