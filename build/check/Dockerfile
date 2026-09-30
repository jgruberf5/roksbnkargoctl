# syntax=docker/dockerfile:1
#
# The roksbnkargoctl in-cluster check image: one static, standard-library-only
# Go binary on scratch. Build from the repository root:
#
#   docker build -f build/check/Dockerfile --build-arg VERSION=v0.1.0 \
#     -t ghcr.io/jgruberf5/roksbnkargoctl-check:v0.1.0 .
#
# Dockerfile.dockerignore beside this file limits the context to what the
# binary needs (BuildKit).

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/check/ ./cmd/check/
ARG VERSION=dev
# No defaults: a default (ARG TARGETARCH=amd64) wins over the value BuildKit
# sets per platform, and v0.5.0's linux/arm64 image held an x86-64 binary (#17).
# The build stage runs on the build platform and cross-compiles.
ARG TARGETOS
ARG TARGETARCH
# Standard library only: nothing to download, so no module cache step.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/check ./cmd/check

FROM scratch
# The node probe verifies FAR / mirror TLS chains against these roots.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/check /check
USER 65532:65532
ENTRYPOINT ["/check"]
