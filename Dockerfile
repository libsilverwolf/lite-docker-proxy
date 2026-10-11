# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/lite-docker-proxy .

FROM alpine:3.20
# ca-certificates is required for HTTPS calls to the upstream registries.
RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S -G app app
COPY --from=build /out/lite-docker-proxy /usr/local/bin/lite-docker-proxy

USER app
EXPOSE 8080
ENV LISTEN_ADDR=:8080

# The binary performs the probe itself, so no curl/wget is needed in the image.
# /healthz is intentionally unauthenticated (unlike /v2/ which returns 401 when
# PROXY_USERNAME is set), otherwise the container would never become healthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["lite-docker-proxy", "healthcheck"]

ENTRYPOINT ["lite-docker-proxy"]
