# syntax=docker/dockerfile:1
# check=skip=SecretsUsedInArgOrEnv
# (GOFER_AUTH_MODE holds a mode name, not a credential.)

# The build stage runs on the build machine's architecture and cross-compiles
# for the target, so multi-architecture images build without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24 AS build

ARG TARGETOS
ARG TARGETARCH
ARG BUILDARCH
ARG TAILWIND_VERSION=v4.2.4
ARG TEMPL_VERSION=v0.3.1001
ARG ESBUILD_VERSION=v0.28.2
# The release version and commit shown in Settings > About. The build has no
# git checkout, so the commit has to be passed in, e.g.
# --build-arg VERSION=v1.2.3 --build-arg COMMIT=$(git rev-parse HEAD)
ARG VERSION=
ARG COMMIT=

# libstdc++ and libgcc are needed by the musl build of the Tailwind CLI.
RUN apk add --no-cache curl libstdc++ libgcc

# Tailwind's standalone CLI, checked against the release checksums.
RUN set -eu; \
    case "$BUILDARCH" in \
      amd64) asset=tailwindcss-linux-x64-musl; sum=d2c074a053f3cb63e3c250e16accabbac9c8aa704bb7d1bd53781ee5dde0e65e ;; \
      arm64) asset=tailwindcss-linux-arm64-musl; sum=c4b49541f9121fcb6cf3fd8e4bcfaa0467b320473c10b418381c1535b0281869 ;; \
      *) echo "unsupported build architecture: $BUILDARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/tailwindcss "https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/${asset}"; \
    echo "$sum  /usr/local/bin/tailwindcss" | sha256sum -c -; \
    chmod +x /usr/local/bin/tailwindcss

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# The same steps as `task generate`, then `task release`.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    go run github.com/a-h/templ/cmd/templ@${TEMPL_VERSION} generate --ignore-pattern='(^|/)dist(/|$)'; \
    tailwindcss -i ./assets/css/input.css -o ./assets/css/output.css --minify; \
    go run github.com/evanw/esbuild/cmd/esbuild@${ESBUILD_VERSION} $(ls assets/js/*.js | grep -v -e '\.min\.js$' -e '/sw\.js$') --minify --outdir=assets/min/js --log-level=warning; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags embedded_assets -trimpath \
      -ldflags "-X github.com/cristianadrielbraun/gofer/internal/buildinfo.Version=${VERSION} -X github.com/cristianadrielbraun/gofer/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/gofer .

FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 1000 gofer \
    && adduser -S -D -H -u 1000 -G gofer gofer \
    && mkdir /data \
    && chown gofer:gofer /data

COPY --from=build /out/gofer /usr/local/bin/gofer

# A container is only reachable through its network interface, so listen on
# all addresses. GOFER_BASE_URL has no useful default and must be set.
ENV GO_ENV=production \
    GOFER_ADDR=0.0.0.0:8090 \
    GOFER_DB_PATH=/data/gofer.db \
    GOFER_AUTH_MODE=personal

USER gofer
WORKDIR /data
VOLUME /data
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD ["gofer", "healthcheck"]

ENTRYPOINT ["gofer"]
