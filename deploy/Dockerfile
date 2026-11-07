# Build context is the repository root:
#
#   docker build -f deploy/Dockerfile .

# Build a static binary. go.mod lives in server/, so the module builds from there.
FROM golang:1.27.3-bookworm AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY . .
WORKDIR /src/server
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" -o /dynamoat ./cmd/dynamoat

# Run on distroless: no shell, no package manager, non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.source="https://github.com/toclocoinc/dynamoat" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="dynamoat" \
      org.opencontainers.image.description="DynamoDB API-compatible server on PostgreSQL"
EXPOSE 8000
COPY --from=build /dynamoat /dynamoat
ENTRYPOINT ["/dynamoat"]
