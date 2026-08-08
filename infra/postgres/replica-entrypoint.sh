#!/bin/bash
# Bootstraps a streaming-replication standby from the primary, then hands
# off to the official image's own entrypoint. Used unchanged by both
# postgres-replica-sync and postgres-replica-async (docker-compose.yml) —
# everything that differs between them (slot name, application_name,
# whether the primary waits on it) is env-driven, not scripted twice.
#
# Why not the image's own initdb path: a replica isn't a fresh independent
# database, it's a byte-for-byte copy of the primary's, so its equivalent
# of "first boot" is pg_basebackup, not initdb. This script's whole job is
# populating $PGDATA *before* docker-entrypoint.sh ever looks at it — once
# $PGDATA/PG_VERSION exists, that entrypoint's own init-on-empty-directory
# logic skips initdb entirely and goes straight to starting postgres, which
# is what detects standby.signal below and enters recovery/standby mode.
set -euo pipefail

: "${PRIMARY_HOST:?PRIMARY_HOST must be set}"
: "${PRIMARY_PORT:?PRIMARY_PORT must be set}"
: "${REPLICATION_USER:?REPLICATION_USER must be set}"
: "${REPLICATION_PASSWORD:?REPLICATION_PASSWORD must be set}"
: "${REPLICA_SLOT_NAME:?REPLICA_SLOT_NAME must be set}"
: "${REPLICA_APPLICATION_NAME:?REPLICA_APPLICATION_NAME must be set}"

PGDATA="${PGDATA:-/var/lib/postgresql/data/pgdata}"

if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
  echo "replica-entrypoint: $PGDATA is empty, bootstrapping from $PRIMARY_HOST:$PRIMARY_PORT"

  # depends_on: service_healthy (docker-compose.yml) already waits for the
  # primary's pg_isready, but that's the primary's own readiness, not proof
  # this replica's network path to it is up yet — same "healthy dependency,
  # still worth a short retry loop" reasoning as kafka-init's topic-list
  # poll.
  until PGPASSWORD="$REPLICATION_PASSWORD" pg_isready -h "$PRIMARY_HOST" -p "$PRIMARY_PORT" -U "$REPLICATION_USER" >/dev/null 2>&1; do
    echo "replica-entrypoint: waiting for primary..."
    sleep 2
  done

  # -C/-S creates the physical replication slot named $REPLICA_SLOT_NAME on
  # the primary as part of the backup itself — atomic with the copy, so
  # there's no window where this replica could start streaming before its
  # slot exists (which would let the primary recycle WAL out from under
  # it). That slot is also what survives THIS container restarting: the
  # primary keeps retaining WAL for it whether or not the replica is
  # currently connected.
  PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
    -h "$PRIMARY_HOST" -p "$PRIMARY_PORT" -U "$REPLICATION_USER" \
    -D "$PGDATA" -Fp -Xs -P \
    -C -S "$REPLICA_SLOT_NAME"

  # Written by hand rather than pg_basebackup's -R/--write-recovery-conf:
  # -R infers primary_conninfo purely from the connection parameters used
  # for the backup itself, with no way to set application_name on it. The
  # primary's synchronous_standby_names (docker-compose.yml's `command:`
  # for the `postgres` service) matches replicas by that exact name — a
  # replica that connects under any other name is invisible to it and
  # synchronous_commit silently degrades to nothing waiting on this
  # standby at all, which is the one failure mode this setup most needs to
  # not have.
  cat >> "$PGDATA/postgresql.auto.conf" <<-EOF
primary_conninfo = 'host=$PRIMARY_HOST port=$PRIMARY_PORT user=$REPLICATION_USER password=$REPLICATION_PASSWORD application_name=$REPLICA_APPLICATION_NAME'
primary_slot_name = '$REPLICA_SLOT_NAME'
EOF
  touch "$PGDATA/standby.signal"
  chmod 0700 "$PGDATA"
fi

exec docker-entrypoint.sh postgres
