#!/bin/bash
# Runs once, on first init of the primary's data directory (official postgres
# image convention: any *.sh in /docker-entrypoint-initdb.d/ runs automatically).
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
  CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '$REPLICATOR_PASSWORD';
EOSQL

# Allow the replica (anywhere on the Docker network) to open a replication connection.
echo "host replication replicator 0.0.0.0/0 md5" >> "$PGDATA/pg_hba.conf"
