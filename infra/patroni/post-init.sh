#!/bin/bash
# Patroni's `bootstrap.post_init` hook: runs exactly once, on the node
# that wins the initial bootstrap race, immediately after initdb and
# before that node ever advertises itself as leader. Every other node
# reaches the same state by cloning this one, so whatever happens here is
# replicated rather than repeated.
#
# It exists because Patroni's bootstrap creates the cluster, the
# superuser and the replication role (all from patroni.yml's
# `authentication:` block) but has no equivalent of the official image's
# POSTGRES_DB — there is no "and also make me a database called neobank"
# knob. Without this, all six services would come up pointing at a
# database that does not exist.
#
# Patroni passes a libpq connection string as $1, already authenticated
# as the superuser against the freshly initialised cluster.
set -euo pipefail

CONNSTR="$1"

# CREATE DATABASE cannot run inside a transaction block, so this is two
# separate psql invocations rather than one IF NOT EXISTS-style DO block
# (which would be a transaction). The existence probe keeps the hook
# idempotent — not strictly required for a once-per-cluster hook, but it
# means re-running it by hand during debugging is harmless.
if ! psql "$CONNSTR" -v ON_ERROR_STOP=1 -tAc "SELECT 1 FROM pg_database WHERE datname = 'neobank'" | grep -q 1; then
  psql "$CONNSTR" -v ON_ERROR_STOP=1 -c "CREATE DATABASE neobank"
  echo "patroni post-init: created database neobank"
else
  echo "patroni post-init: database neobank already exists"
fi
