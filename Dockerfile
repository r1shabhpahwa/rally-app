# Build a static binary: CGO is off because the SQLite driver is pure Go, so
# the runtime image needs no compiler, no libc pinning and no build tools.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Alpine rather than scratch or distroless: when something goes wrong on the VM
# at 9pm, having a shell in the container is worth the few extra megabytes.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /data && chown app:app /data
COPY --from=build /out/server /usr/local/bin/server
USER app
VOLUME /data
ENV DB_PATH=/data/badminton.db \
    BACKUP_DIR=/data/backups \
    PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/server"]
