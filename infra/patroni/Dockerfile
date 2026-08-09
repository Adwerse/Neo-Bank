# Postgres + Patroni in one image, because Patroni is not a sidecar: it
# does not talk to a Postgres running somewhere else, it *is* the process
# that starts, stops, promotes and demotes the local postgres. It calls
# pg_ctl, initdb, pg_basebackup and pg_rewind directly, so those binaries
# have to be in the same container and the same filesystem namespace as
# $PGDATA. Splitting them into two containers would mean Patroni could
# observe the leader dying but could not do the one thing it exists for.
#
# Base is postgres:16-bookworm rather than the -alpine tag the plain
# nodes used: Patroni is Python, and the wheels it needs (psycopg's
# binary build in particular) are manylinux — on Alpine/musl pip falls
# back to building from source, which drags in gcc and the libpq headers
# and turns a 20-second image build into a multi-minute one.
FROM postgres:16-bookworm

# python3-venv, not a bare `pip install`: Debian bookworm marks its
# system Python as externally-managed (PEP 668) and refuses to install
# into it. A venv is the supported answer and also keeps Patroni's
# dependency tree from colliding with anything the base image ships.
#
# patroni[etcd3] pulls the etcd *v3 API* client specifically. Plain
# `patroni[etcd]` is the v2 client, and etcd v3.4+ serves the v2 API only
# when explicitly enabled — pairing them silently yields a Patroni that
# cannot reach its DCS and therefore never elects anyone.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends python3 python3-venv curl; \
    rm -rf /var/lib/apt/lists/*; \
    python3 -m venv /opt/patroni; \
    /opt/patroni/bin/pip install --no-cache-dir --upgrade pip; \
    /opt/patroni/bin/pip install --no-cache-dir "patroni[etcd3]==4.0.4" "psycopg[binary]"

ENV PATH="/opt/patroni/bin:${PATH}"

COPY infra/patroni/patroni.yml /etc/patroni/patroni.yml
COPY infra/patroni/post-init.sh /etc/patroni/post-init.sh
COPY infra/patroni/entrypoint.sh /usr/local/bin/patroni-entrypoint.sh
RUN chmod 0755 /etc/patroni/post-init.sh /usr/local/bin/patroni-entrypoint.sh

# 5432 postgres, 8008 Patroni's REST API — the endpoint HAProxy asks
# "are you the leader?" (see infra/haproxy/haproxy.cfg).
EXPOSE 5432 8008

ENTRYPOINT ["/usr/local/bin/patroni-entrypoint.sh"]
