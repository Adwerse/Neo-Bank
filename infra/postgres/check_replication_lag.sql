-- Run on the LEADER (pg_stat_replication only exists there, listing every
-- streaming standby the leader currently sees). See README's "Postgres:
-- автоматический failover (Patroni + etcd)" for what a healthy vs lagging
-- row looks like.
--
--   psql "postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
--     -f infra/postgres/check_replication_lag.sql
--
-- Port 5432 is pg-haproxy, which routes to whichever node is leader — so
-- this works without knowing or caring which container that is today.
-- That is the change from the pre-Patroni version of this file, which
-- named the `postgres` container directly. If you would rather go through
-- docker, ask a node by name and let it redirect you:
--
--   docker compose exec -T pg-node1 psql -U neobank -d neobank < infra/postgres/check_replication_lag.sql
--
-- (-T disables pseudo-tty allocation — without it, `exec` won't accept
-- piped/redirected stdin. Note this only produces rows if pg-node1
-- happens to be the leader; the psql form above always hits the right
-- node.)
--
-- Byte lag (pg_wal_lsn_diff) and time lag (write_lag/flush_lag/replay_lag,
-- native `interval` columns since PG10) answer two different questions:
-- bytes is "how much work is queued", seconds is "how old is what the
-- standby can currently serve" — the one that actually matters for
-- deciding whether a read from a standby is fresh enough.
--
-- sync_state is what to check when in doubt about which standby is
-- synchronous. Exactly one row should read 'sync' and the rest 'async' —
-- but WHICH node that is is now Patroni's decision (synchronous_node_count
-- in infra/patroni/patroni.yml) and changes after every failover, so it is
-- no longer a fixed name to assert on the way `replica_a` once was.
-- application_name is the Patroni member name (pg-node1/2/3).
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
