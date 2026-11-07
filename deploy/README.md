# Deployment

- `Dockerfile`: multi-stage build (Go on `golang:1.27.3-bookworm`, run on
  `gcr.io/distroless/static-debian12:nonroot`); the build context is the
  repository root.
- `docker-compose.yml`: dynamoat with a Postgres 17 database for local
  development. It publishes dynamoat on `127.0.0.1:8000` only; edit the
  `ports` mapping to expose it on other interfaces. See `docs/running.md`.
- [`helm/dynamoat/`](helm/dynamoat/): the Helm chart. Its
  [README](helm/dynamoat/README.md) covers install, the values, the GKE + Cloud
  SQL and AKS + Flexible Server profiles, and use as a subchart.
