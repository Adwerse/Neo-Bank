#!/bin/bash
# Runs once, via /docker-entrypoint-initdb.d/, only on a fresh data
# directory (the official postgres image's own rule for everything in that
# folder — see docker-compose.yml's primary-init.sh mount). Two things a
# plain single-instance Postgres never needed:
#
#   1. A role that can authenticate as a replica, not as the application.
#      `replicator` gets REPLICATION LOGIN only — no table grants, no
#      superuser — so a compromised replica connection can stream WAL and
#      nothing else.
#   2. A pg_hba.conf line for the "replication" pseudo-database. initdb's
#      generated pg_hba.conf grants `host all all all scram-sha-256`, but
#      "all" there does NOT cover replication connections — that's a
#      separate pseudo-database in pg_hba.conf's matching rules. Without
#      this line, postgres-replica-sync/async's pg_basebackup and ongoing
#      streaming both fail at the authentication step, not anywhere WAL-
#      related, which is a confusing place to first notice it's missing.
set -euo pipefail

: "${REPLICATION_USER:?REPLICATION_USER must be set}"
: "${REPLICATION_PASSWORD:?REPLICATION_PASSWORD must be set}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE ROLE $REPLICATION_USER WITH REPLICATION LOGIN PASSWORD '$REPLICATION_PASSWORD';
EOSQL

echo "host replication $REPLICATION_USER all scram-sha-256" >> "$PGDATA/pg_hba.conf"
