# syntax=docker/dockerfile:1
#
# roksbnkargoctl itself as a container image, so it runs with nothing but Docker.
# Build from the repository root (or `make cli-image`):
#
#   docker build -f build/cli/Dockerfile --build-arg VERSION=v0.6.0 \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     -t ghcr.io/jgruberf5/roksbnkargoctl:v0.6.0 .
#
# Run it with the current directory as the home for workspaces:
#
#   docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" \
#     -e IBMCLOUD_API_KEY ghcr.io/jgruberf5/roksbnkargoctl:v0.6.0 workspaces list
#
# Dockerfile.dockerignore beside this file limits the context to what the
# binary needs (BuildKit).

# The build runs on the builder's own platform and cross-compiles: the binary is
# pure Go (CGO_ENABLED=0), so an arm64 image needs no emulated compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# Behind a TLS-intercepting proxy the module download fails with "certificate
# signed by unknown authority". The optional build secret extra_ca adds that
# proxy's CA for this step only; it is never written into a layer:
#   docker build --secret id=extra_ca,src=corp-ca.pem …   (make cli-image EXTRA_CA=…)
# The modules go into this layer, not a cache mount: a cached layer over a cache
# mount that BuildKit has since emptied sends the build step back to the network,
# without the CA (seen locally: "go: downloading … certificate signed by unknown
# authority" from go build after a CACHED go mod download).
RUN --mount=type=secret,id=extra_ca \
    if [ -s /run/secrets/extra_ca ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.pem; \
      export SSL_CERT_FILE=/tmp/ca.pem; \
    fi; \
    go mod download
COPY cmd/roksbnkargoctl/ ./cmd/roksbnkargoctl/
COPY internal/ ./internal/
# VERSION must be exactly vX.Y.Z on a release: checkImageTag picks the check
# image of the same tag from it. Anything else (dev) runs the :dev check image.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# No defaults: a default (ARG TARGETARCH=amd64) wins over the value BuildKit
# sets per platform, and the arm64 image then carries an x86-64 binary (seen).
ARG TARGETOS
ARG TARGETARCH
# The same ldflags as the Makefile and .goreleaser.yaml.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w \
      -X github.com/jgruberf5/roksbnkargoctl/internal/cli.Version=${VERSION} \
      -X github.com/jgruberf5/roksbnkargoctl/internal/cli.Commit=${COMMIT} \
      -X github.com/jgruberf5/roksbnkargoctl/internal/cli.BuildDate=${DATE}" \
      -o /out/roksbnkargoctl ./cmd/roksbnkargoctl \
 && mkdir -p /out/work && chmod 1777 /out/work

# distroless/static rather than scratch: it carries the CA roots (IBM Cloud,
# GitHub, FAR and registries are all HTTPS), tzdata, a passwd entry for the
# nonroot user, and a world-writable /tmp — gitpub writes known_hosts to
# os.TempDir(), which does not exist on scratch.
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="roksbnkargoctl" \
      org.opencontainers.image.description="Install F5 BIG-IP Next for Kubernetes 2.4 on IBM Cloud ROKS with Argo CD" \
      org.opencontainers.image.source="https://github.com/jgruberf5/roksbnkargoctl" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/roksbnkargoctl /usr/local/bin/roksbnkargoctl
# 1777 so the image also works without a mount under any --user; with
# -v "$PWD:/work" the host directory's own permissions apply.
COPY --from=build /out/work /work
# HOME=/work, not only ROKSBNKARGOCTL_HOME: under --user "$(id -u):$(id -g)" the
# uid has no passwd entry, so Docker would set HOME=/ (not writable), and
# everything home-relative — workspaces (~/.roksbnkargoctl), and `~` in a path
# the operator types — would miss the mount. With HOME=/work the workspaces land
# in /work/.roksbnkargoctl, i.e. the host directory mounted there, and
# ROKSBNKARGOCTL_HOME stays free to override.
# ROKSBNKARGOCTL_CONTAINER=1 tells the binary it is this image: self install /
# self update refuse (pull a newer tag instead) and `agent <cli>` explains that
# no agent CLI is installed here.
ENV HOME=/work \
    ROKSBNKARGOCTL_CONTAINER=1
WORKDIR /work
USER 65532:65532
ENTRYPOINT ["roksbnkargoctl"]
