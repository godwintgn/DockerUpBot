FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -X github.com/dockerupbot/dockerupbot/internal/version.Version=${VERSION} -X github.com/dockerupbot/dockerupbot/internal/version.Commit=${COMMIT} -X github.com/dockerupbot/dockerupbot/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/dockerupbot ./cmd/dockerupbot

# Runtime needs the Docker CLI for `docker compose up -d <service>`.
# Docker socket access typically requires root or docker-group membership;
# non-root is preferred when a rootless/proxy socket is available (spec §41).
FROM docker:29-cli
ARG VERSION=0.1.0
ARG COMMIT=none
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="DockerUpBot" \
      org.opencontainers.image.description="Telegram bot that applies one Docker Compose update after you approve it. Linux amd64 and arm64." \
      org.opencontainers.image.licenses="GPL-3.0" \
      org.opencontainers.image.url="https://github.com/godwintgn/dockerupbot" \
      org.opencontainers.image.documentation="https://github.com/godwintgn/dockerupbot#image" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/godwintgn/dockerupbot"
COPY --from=build /out/dockerupbot /usr/local/bin/dockerupbot
RUN ln -sf /usr/local/bin/dockerupbot /usr/local/bin/dubot \
 && mkdir -p /config /data \
 && adduser -D -H -u 10001 dockerupbot \
 && chown -R dockerupbot:dockerupbot /config /data
ENV HTTP_ADDR=:9467 \
    CONFIG=/data/config.yml
EXPOSE 9467
# Default to root so the mounted docker.sock works out of the box.
# Override with `user: "10001:10001"` when using a rootless Docker socket / API proxy.
USER root
ENTRYPOINT ["/usr/local/bin/dockerupbot"]
