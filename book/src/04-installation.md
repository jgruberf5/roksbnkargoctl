# Installing roksbnkargoctl

This chapter gets the `roksbnkargoctl` binary onto your workstation, on Linux, macOS or
Windows, and explains where the in-cluster `check` image comes from and how the binary
chooses which one to deploy.

## Build from source

`roksbnkargoctl` is one Go module, `github.com/jgruberf5/roksbnkargoctl`. It needs Go
**1.26** (the module declares `go 1.26.6`) and nothing else: no C toolchain, no cgo, no
other binaries at build time or at run time.

```sh
git clone https://github.com/jgruberf5/roksbnkargoctl.git
cd roksbnkargoctl
make build
./bin/roksbnkargoctl version
```

`make build` produces a static binary at `bin/roksbnkargoctl` and stamps it with version
metadata:

| Stamped value | Source |
|---|---|
| Version | `git describe --tags --always --dirty` |
| Commit | `git rev-parse --short HEAD` |
| Build date | the current UTC time |

Without `make`, a plain Go build works on every platform:

```sh
go build ./cmd/roksbnkargoctl
```

An unstamped build reports its version as `dev`. Put the binary anywhere on your `PATH`.

Other targets in the `Makefile`:

| Target | Does |
|---|---|
| `make test` | `go test -race ./...` |
| `make vet`, `make fmt` | `go vet`, and fails if any file needs `gofmt` |
| `make verify` | fmt, vet, test, build and the check binary: everything a change must pass |
| `make check-binary` | the `check` binary for Linux, into `bin/check` |
| `make check-image` | builds the `check` image with Docker from `build/check/Dockerfile` |

## Windows

The binary is native on Windows. Every feature is implemented in-process, so it does not
shell out to `curl`, `kubectl`, `helm`, `git`, `openssl` or `terraform`; continuous
integration builds it on Windows for every change.

```powershell
go build ./cmd/roksbnkargoctl
.\roksbnkargoctl.exe version
```

Set environment variables for the session with `$env:`:

```powershell
$env:IBMCLOUD_API_KEY = "…"
$env:ARGOCD_AUTH_TOKEN = "…"
$env:ROKSBNKARGOCTL_GIT_TOKEN = "…"
```

The workspace lives under your user profile, in `.roksbnkargoctl\` (see
[Workspaces and init](./05-workspaces-and-init.md)).

## Check the build

```sh
roksbnkargoctl version
```

```text
roksbnkargoctl v1.2.3 (commit 1a2b3c4, built 2026-09-29T12:00:00Z)
```

The values are examples; an unstamped build prints
`roksbnkargoctl dev (commit none, built unknown)`.

## The check image

The prerequisite and lifecycle checks run in ROKS as a container, `check`: one static,
standard-library-only Go binary on `scratch`, running as a non-root user. The binary on
your workstation never runs it; Argo CD does, in the cluster.

It is published, publicly, to `ghcr.io/jgruberf5/roksbnkargoctl-check` for `linux/amd64`
and `linux/arm64`:

| Tag | Published from |
|---|---|
| `:dev` | every change to the check on `main` |
| `:vX.Y.Z` and `:latest` | each release tag `vX.Y.Z` |
| `:pr-N` | a pull request in the repository, so a check change can be proven on a live cluster before it merges |

### Which image a build deploys

When it renders, `roksbnkargoctl` picks the tag from its own version:

- a release build (version exactly `vX.Y.Z`) uses `:vX.Y.Z`;
- any other build (`dev`, or a `git describe` version such as `v1.2.3-4-gabc1234`) uses
  `:dev`, because no image exists for those versions.

It then **resolves the tag to a digest** and renders
`ghcr.io/jgruberf5/roksbnkargoctl-check@sha256:…`. A mutable tag is unsafe here: with
`imagePullPolicy: IfNotPresent`, nodes that cached an older `:dev` would keep running it,
and a re-sync after a check fix would silently run the old check. A new image also
changes the render, which re-rolls the node probes.

To pin a different image (for example a `:pr-N` build), set `check.image` in
`config.yaml`:

```yaml
check:
  image: ghcr.io/jgruberf5/roksbnkargoctl-check:pr-42
```

### In mirror mode

With `registry.source: mirror` the cluster pulls the check image from your mirror, like
every BNK image. `roksbnkargoctl registry replicate` copies it there with the BNK
artifacts. The digest is still resolved **upstream**, from `ghcr.io`, and `render` fails
if the mirror does not hold that exact digest, telling you to run `registry replicate`.
A mirror does not follow a mutable tag, so this is what stops a cluster running a stale
check. If the operator host cannot reach `ghcr.io` at all, the mirror's copy is used with
a warning that its currency could not be checked. See
[Mirroring into a private registry](./13-registry.md).

## See also

- [Prerequisites](./03-prerequisites.md)
- [Workspaces and init](./05-workspaces-and-init.md)
- [Checks reference](./16-checks.md)
