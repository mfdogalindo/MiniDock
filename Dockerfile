FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/minidock ./cmd/minidock

FROM alpine:3.22
RUN apk add --no-cache docker-cli git curl su-exec
COPY --from=build /out/minidock /usr/local/bin/minidock
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint
RUN addgroup -S minidock && adduser -S -D -H -G minidock minidock \
    && mkdir -p /var/lib/minidock \
    && chown -R minidock:minidock /var/lib/minidock \
    && chmod 0755 /usr/local/bin/docker-entrypoint
LABEL org.opencontainers.image.title="MiniDock" \
      org.opencontainers.image.version="1.0.0" \
      org.opencontainers.image.vendor="Kyberix"
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/docker-entrypoint"]
CMD ["/usr/local/bin/minidock"]

