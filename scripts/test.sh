#!/bin/sh
# The repository's test entry point (also what "npm test" runs, which is what
# the build agents' gate calls). Formatting, vet, then the Go tests; the race
# detector when a C compiler is available.
#
# Postgres integration tests run only when DYNAMOAT_TEST_PG is set to a
# connection string, e.g. postgres://postgres:pw@127.0.0.1:5417/dynamoat_test.
set -eu
cd "$(dirname "$0")/.."
GO=scripts/go.sh

unformatted=$("$($GO env GOROOT)/bin/gofmt" -l server)
if [ -n "$unformatted" ]; then
  echo "gofmt needed on:" >&2; echo "$unformatted" >&2; exit 1
fi

$GO -C server vet ./...

if command -v cc >/dev/null 2>&1; then
  CGO_ENABLED=1 $GO -C server test -race -count=1 ./...
else
  $GO -C server test -count=1 ./...
fi

# Node packages: the conformance suite (AWS SDK v3, run against dynamoat),
# tools/import and bench/. Install a package's dependencies when they are
# missing or older than its package-lock.json (npm writes
# node_modules/.package-lock.json on install), then run its tests.
node_suite() {
  if [ ! -f "$1/node_modules/.package-lock.json" ] || [ "$1/package-lock.json" -nt "$1/node_modules/.package-lock.json" ]; then
    (cd "$1" && npm ci --no-audit --no-fund)
  fi
  (cd "$1" && npm test)
}
node_suite conformance
node_suite tools/import
node_suite bench

# smoke: readable checks of client workflows, run against a locally started
# dynamoat. Unsupported operations report ○ and do not fail the run.
if [ ! -f "smoke/node_modules/.package-lock.json" ] || [ "smoke/package-lock.json" -nt "smoke/node_modules/.package-lock.json" ]; then
  (cd smoke && npm ci --no-audit --no-fund)
fi
(cd smoke && node run-all.mjs --start)
