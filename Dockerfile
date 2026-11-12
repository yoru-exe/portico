# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
# Runs on the build machine's own architecture and cross-compiles, so multi-arch
# images build quickly without QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# /out/data becomes the writable /data volume of the non-root user.
RUN mkdir -p /out/data && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -tags http3 \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/portico .

# ---- runtime stage ---------------------------------------------------------
# distroless/static: no shell, no package manager, just CA certificates and
# timezone data (needed for ACME / Let's Encrypt). Runs as uid 65532 (nonroot).
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="Portico" \
      org.opencontainers.image.description="A small web server and reverse proxy with automatic HTTPS" \
      org.opencontainers.image.source="https://github.com/yoru-exe/portico"

COPY --from=build /out/portico /usr/local/bin/portico
COPY docker/portico.json /etc/portico/portico.json
COPY --chown=65532:65532 docker/index.html /srv/index.html
COPY --from=build --chown=65532:65532 /out/data /data

# Certificates obtained via ACME are cached in ./portico-certs, i.e. in /data.
# Mount a volume here so they survive container restarts.
WORKDIR /data
VOLUME /data

# HTTP, HTTPS (TCP) and HTTP/3 (UDP). The default config only uses 8080.
EXPOSE 8080 80 443 443/udp

ENTRYPOINT ["/usr/local/bin/portico"]
CMD ["run", "-config", "/etc/portico/portico.json"]
