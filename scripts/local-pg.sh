#!/usr/bin/env bash
# A local Postgres 17 container for running dynamoat on a workstation.
#
#   scripts/local-pg.sh up      create (first time) or start the container
#   scripts/local-pg.sh down    stop it; the data is kept
#   scripts/local-pg.sh reset   delete the container and its data
#   scripts/local-pg.sh status  show whether it is running
#   scripts/local-pg.sh url     print the DYNAMOAT_PG connection string
#   scripts/local-pg.sh psql    open psql inside the container
#
# Listens on 127.0.0.1 only. Override the port with DYNAMOAT_LOCAL_PG_PORT.
set -euo pipefail

name=dynamoat-pg
volume=dynamoat-pgdata
port=${DYNAMOAT_LOCAL_PG_PORT:-5432}
url="postgres://dynamoat:dynamoat@127.0.0.1:${port}/dynamoat?sslmode=disable"

if ! docker info >/dev/null 2>&1; then
  echo "Docker is not running. Start Docker Desktop and try again." >&2
  exit 1
fi

exists() { docker container inspect "$name" >/dev/null 2>&1; }

wait_ready() {
  for _ in $(seq 1 60); do
    if docker exec "$name" pg_isready -q -U dynamoat -d dynamoat 2>/dev/null; then
      echo "ready: $url"
      return 0
    fi
    sleep 1
  done
  echo "Postgres did not become ready; see: docker logs $name" >&2
  return 1
}

case "${1:-}" in
  up)
    if exists; then
      docker start "$name" >/dev/null
    else
      docker run -d --name "$name" \
        -e POSTGRES_USER=dynamoat -e POSTGRES_PASSWORD=dynamoat -e POSTGRES_DB=dynamoat \
        -p "127.0.0.1:${port}:5432" \
        -v "${volume}:/var/lib/postgresql/data" \
        postgres:17 >/dev/null
    fi
    wait_ready
    ;;
  down)
    exists && docker stop "$name" >/dev/null
    echo "stopped (data kept in volume $volume)"
    ;;
  reset)
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker volume rm "$volume" >/dev/null 2>&1 || true
    echo "removed $name and $volume"
    ;;
  status)
    if exists; then docker ps -a --filter "name=^${name}$" --format '{{.Names}}  {{.Status}}  {{.Ports}}'; else echo "not created"; fi
    ;;
  url)
    echo "$url"
    ;;
  psql)
    exec docker exec -it "$name" psql -U dynamoat -d dynamoat
    ;;
  *)
    sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
