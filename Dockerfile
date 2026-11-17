# syntax=docker/dockerfile:1
FROM golang:1.26.11-bookworm AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY *.go ./
COPY api/ ./api/
COPY internal/ ./internal/
COPY migrations/ ./migrations/
COPY seed_assets/ ./seed_assets/
RUN go build -mod=readonly -trimpath -buildvcs=false -o /out/tenantku .

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 tenantku \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin tenantku

WORKDIR /app
COPY --from=build /out/tenantku /usr/local/bin/tenantku
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/tenantku"]

FROM runtime AS worker

ENV WORKER_HEALTH_ADDR=127.0.0.1:8081
EXPOSE 8081
HEALTHCHECK --interval=15s --timeout=4s --start-period=25s --retries=3 \
    CMD curl --noproxy '*' --fail --silent --max-time 3 http://127.0.0.1:8081/readyz > /dev/null
CMD ["worker"]

FROM runtime AS api

USER root
RUN install -d -m 0750 -o 10001 -g 10001 /app/uploads
USER 10001:10001

ENV GIN_MODE=release HOST=0.0.0.0 PORT=8080 UPLOAD_DIR=/app/uploads
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=4s --start-period=20s --retries=3 \
    CMD curl --noproxy '*' --fail --silent --max-time 3 "http://127.0.0.1:${PORT}/readyz" > /dev/null
