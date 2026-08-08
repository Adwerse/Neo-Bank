-- Run on the PRIMARY (pg_stat_replication only exists there, listing every
-- streaming standby the primary currently sees). See README's "Postgres:
-- потоковая репликация — primary + 2 реплики" for what a healthy vs
-- lagging row looks like and how to reproduce lag on demand.
--
--   docker compose exec -T postgres psql -U neobank -d neobank < infra/postgres/check_replication_lag.sql
--
-- (-T disables pseudo-tty allocation — without it, `exec` won't accept
-- piped/redirected stdin.)
--
-- Byte lag (pg_wal_lsn_diff) and time lag (write_lag/flush_lag/replay_lag,
-- native `interval` columns since PG10) answer two different questions:
-- bytes is "how much work is queued", seconds is "how old is what the
-- replica can currently serve" — the one that actually matters for
-- deciding whether a read from postgres-replica-async is fresh enough.
-- sync_state is what to check when in doubt about which replica is
-- synchronous: 'sync' should be exactly replica_a, never replica_b.
SELECT
    application_name,
    client_addr,
    state,
    sync_state,
    pg_wal_lsn_diff(pg_current_wal_lsn(), sent_lsn)   AS pending_bytes,
    pg_wal_lsn_diff(pg_current_wal_lsn(), write_lsn)  AS write_lag_bytes,
    pg_wal_lsn_diff(pg_current_wal_lsn(), flush_lsn)  AS flush_lag_bytes,
    pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS replay_lag_bytes,
    write_lag,
    flush_lag,
    replay_lag
FROM pg_stat_replication
ORDER BY application_name;
