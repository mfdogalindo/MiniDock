FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/minidock ./cmd/minidock

FROM alpine:3.22
RUN apk add --no-cache docker-cli git curl
COPY --from=build /out/minidock /usr/local/bin/minidock
RUN mkdir -p /var/lib/minidock && chown -R minidock:minidock /var/lib/minidock
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/minidock"]
