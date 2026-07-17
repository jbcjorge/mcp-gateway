# Build stage
FROM golang:1.26-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w \
    -X main.Version=$(git describe --tags --exact-match 2>/dev/null || echo dev) \
    -X main.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
    -X main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /mcp-gateway .

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 gateway

COPY --from=build /mcp-gateway /usr/local/bin/mcp-gateway

USER 10001

EXPOSE 19900

ENTRYPOINT ["mcp-gateway"]
CMD ["/config/config.json"]
