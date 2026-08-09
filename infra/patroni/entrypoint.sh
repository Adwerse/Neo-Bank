#!/bin/bash
# Runs as root only long enough to fix up ownership of the mounted
# volume, then drops to the postgres user and hands the container over to
# Patroni for good.
#
# The chown is not boilerplate: Docker creates a fresh named volume's
# mount point owned by root:root, and both initdb and Patroni's own
# pg_basebackup-based replica bootstrap refuse to touch a data directory
# they don't own. The official postgres image does this same fixup inside
# its own entrypoint; we replaced that entrypoint, so we inherit the job.
#
# 0700 on the parent matters for the same reason it matters on $PGDATA
# itself — postgres refuses to start if group/other can read the data
# directory, and a volume Docker created is 0755.
set -euo pipefail

PGDATA_PARENT="${PGDATA_PARENT:-/var/lib/postgresql/data}"

mkdir -p "$PGDATA_PARENT"
chown -R postgres:postgres "$PGDATA_PARENT"
chmod 0700 "$PGDATA_PARENT"

# Patroni writes a .pgpass here so its pg_basebackup/pg_rewind child
# processes can authenticate as `replicator` without the password
# appearing in a command line. It must exist and be writable by postgres
# before Patroni starts, and must not be group-readable.
install -d -o postgres -g postgres -m 0700 /run/patroni

# exec, not a background start: Patroni becomes PID 1 so it receives
# SIGTERM from `docker stop` directly and can shut its postgres down
# cleanly (and, if it is the leader, release the leader key in etcd
# instead of making the rest of the cluster wait out the full TTL).
exec gosu postgres patroni /etc/patroni/patroni.yml
