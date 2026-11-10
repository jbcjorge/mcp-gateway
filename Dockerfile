# Build stage
# GO_VERSION is injected by CI from go.mod (single source of truth). The default
# is a fallback for a plain local `docker build` without the build-arg.
ARG GO_VERSION=1.27.0
FROM golang:${GO_VERSION}-alpine AS build

RUN apk add --no-cache ca-certificates

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w \
    -X github.com/jbcjorge/mcp-gateway/internal/version.Version=${VERSION} \
    -X github.com/jbcjorge/mcp-gateway/internal/version.Commit=${COMMIT} \
    -X github.com/jbcjorge/mcp-gateway/internal/version.BuildDate=${BUILD_DATE}" \
    -o /mcp-gateway .

# Runtime stage
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /src/LICENSE /LICENSE
COPY --from=build /src/NOTICES /NOTICES
COPY --from=build /mcp-gateway /mcp-gateway

USER 10001

EXPOSE 19900

ENTRYPOINT ["/mcp-gateway"]
CMD ["/config/config.json"]
