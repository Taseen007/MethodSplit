#!/bin/bash
# If the data directory is empty, take a fresh base backup from the primary
# and write a standby.signal + primary_conninfo (pg_basebackup -R). Once
# PGDATA is populated, the official entrypoint below starts Postgres normally
# and it comes up in hot-standby/recovery mode.
set -e

if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
  echo "PGDATA is empty; waiting for the primary and taking a base backup..."
  until pg_isready -h "$PRIMARY_HOST" -U "$REPLICATOR_USER" >/dev/null 2>&1; do
    sleep 2
  done

  PGPASSWORD="$REPLICATOR_PASSWORD" pg_basebackup \
    -h "$PRIMARY_HOST" \
    -U "$REPLICATOR_USER" \
    -D "$PGDATA" \
    -Fp -Xs -R -P

  chmod 0700 "$PGDATA"
fi

exec docker-entrypoint.sh "$@"
