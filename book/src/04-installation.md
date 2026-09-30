# Installing roksbnkargoctl

This chapter gets the `roksbnkargoctl` binary onto your workstation, on Linux, macOS or
Windows, or runs it from its container image with nothing but Docker, keeps it up to date,
and explains where the in-cluster `check` image comes from and how the binary chooses which
one to deploy.

## Install with one line

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.ps1 | iex
```

Then confirm:

```sh
roksbnkargoctl version
```

```text
roksbnkargoctl v0.5.0 for BNK 2.4.0 (commit 1a2b3c4, built 2026-09-29T12:00:00Z)
```

The installer:

1. finds the latest release on GitHub (or the one you pin, below);
2. downloads the archive for your operating system, your architecture and the BNK
   release you want;
3. verifies its SHA256 against the release's checksums file, and refuses to go on if
   the checksum does not match, the checksums file is missing or does not list the
   archive;
4. extracts the binary and hands off to `roksbnkargoctl self install --force`, which
   copies it onto your `PATH` (below);
5. removes the downloaded archive and the extracted copy.

### One binary per BNK release

Each `roksbnkargoctl` binary installs exactly one BNK release, and the release assets are
named for it:

```text
roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>.tar.gz   (Linux, macOS)
roksbnkargoctl_<version>_bnk-<BNK version>_windows_<arch>.zip  (Windows)
roksbnkargoctl_<version>_checksums.txt
```

Release 0.5.0 ships binaries for BNK 2.4.0 for Linux, macOS and Windows on `amd64` and
`arm64`. `roksbnkargoctl version` prints both versions. Later BNK releases will ship as
their own binaries beside these.

### Installer options

| Variable | Default | Meaning |
|---|---|---|
| `ROKSBNKARGOCTL_VERSION` (or `VERSION`) | the latest release | install this release tag, for example `v0.5.0` (on Linux and macOS also the first argument: `… \| sh -s -- v0.5.0`). Anything but a release tag is refused |
| `ROKSBNKARGOCTL_BNK_VERSION` (or `BNK_VERSION`) | `2.4.0` | the BNK release the binary installs. If the chosen release has no archive for it, the installer fails and lists the BNK releases it does have |
| `ROKSBNKARGOCTL_INSTALL_DIR` | see [self install](#where-it-goes-self-install) | the directory to install into; spaces are fine |
| `ROKSBNKARGOCTL_INSTALL_ARGS` | none | more arguments for `self install`, split on spaces |
| `GITHUB_TOKEN` | none | authenticates the one GitHub API call, if you hit its rate limit. It is sent to the API only, and on Linux and macOS it is passed to `curl` in a file, not on the command line |

The `ROKSBNKARGOCTL_` names win over the short ones, which are common in CI
environments. The Linux and macOS installer runs nothing until it has been downloaded
in full, so a `curl | sh` cut off mid-transfer does nothing. The Windows installer
leaves no variables or settings behind in your PowerShell session, and supports amd64
and arm64 only.

```sh
# a specific release, into ~/bin
curl -fsSL https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.sh \
  | ROKSBNKARGOCTL_VERSION=v0.5.0 ROKSBNKARGOCTL_INSTALL_DIR="$HOME/bin" sh
```

```powershell
$env:ROKSBNKARGOCTL_VERSION = 'v0.5.0'
irm https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.ps1 | iex
```

You can also download an archive from the
[Releases page](https://github.com/jgruberf5/roksbnkargoctl/releases), check it against the
checksums file, and run `roksbnkargoctl self install` from where you extracted it.

### Where it goes: self install

```sh
roksbnkargoctl self install [--dir DIR] [--force]
```

`self install` copies the running binary onto your `PATH`. The installer calls it for
you; run it yourself after a manual download or a build from source.

| Platform | Default directory |
|---|---|
| Linux, macOS | `$HOME/.local/bin` if it exists or is on `PATH`; else `$HOME/bin` if it does; else `$HOME/.local/bin` is created, with a hint to add it to `PATH`. `/usr/local/bin` only when there is no home directory |
| Windows | `%LOCALAPPDATA%\Microsoft\WindowsApps` if it is on `PATH` and writable; else the first writable `PATH` entry under your profile; else `%LOCALAPPDATA%\Programs\roksbnkargoctl`, with a hint to add it to `PATH` |

`--dir` picks the directory (a leading `~` is expanded). Without `--force` it does nothing
when the running binary already is the installed one.

This is not the same as `roksbnkargoctl install`, which installs **BNK** on your cluster
([chapter 8](./08-install.md)).

## Keep it up to date: self update

```sh
roksbnkargoctl self update              # choose a newer release, then confirm
roksbnkargoctl self update --check      # list newer releases; change nothing
roksbnkargoctl self update --yes        # take the latest without asking
roksbnkargoctl self update --version v0.5.1
```

`self update` replaces the running binary with one from a GitHub release. It downloads
the archive, verifies its SHA256 against the release's checksums file (and refuses
without one), and replaces the binary in place. On Windows a running `.exe` cannot be
overwritten, so the old one is moved aside and restored if the replacement fails.

- **It never switches BNK releases.** It only installs the archive for the BNK release
  the running binary installs. "Latest" is the newest release that has one; a newer
  release that ships only other BNK releases is skipped. A pinned `--version` without
  one is refused, and the error names the BNK releases it does have.
- On a terminal, without `--yes`, it lists the newer releases (newest first, the default)
  and asks `Switch to vX? (y/n)`. With `--yes`, or without a terminal, it takes the
  latest, unless the running binary is already newer (a prerelease or a local build): it
  never goes back to an older release unless `--version` asks for it.
- Each request to GitHub has its own 5-minute timeout, so time spent at the prompts
  does not count against the download.
- `--version` pins a release tag (the `v` is optional; anything else is refused). It can
  go back to an older release, reinstall the current one, or install a prerelease.
- `--check` prints the current version and the newer releases, changes nothing and
  exits 0.
- `GITHUB_TOKEN`, when set, is sent to `api.github.com` only, never with a download.
- On Windows, if the replacement failed **and** the rollback failed, it exits with code
  125 and prints the `.old` file and the `Move-Item` command that restores it.

**This updates roksbnkargoctl, not BNK.** roksbnkargoctl installs and uninstalls BNK; it
does not upgrade it. BNK supports an in-place upgrade by changing the manifest version in
its custom resources, as F5's BNK documentation describes, and that is beyond the scope
of this tool.

## Run with Docker

If you would rather not install anything, run the tool from its container image,
`ghcr.io/jgruberf5/roksbnkargoctl`:

| Tag | Published from |
|---|---|
| `:vX.Y.Z` | each release tag `vX.Y.Z` |
| `:latest` | the newest final release (not a prerelease) |
| `:dev` | every change on `main` |
| `:pr-N` | a pull request in the repository |

Each tag is built for `linux/amd64` and `linux/arm64`. The binary in `:vX.Y.Z` is stamped
with that version, so it deploys the check image of the same tag, exactly as the release
binary does.

```sh
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" \
  -e IBMCLOUD_API_KEY -e ARGOCD_AUTH_TOKEN -e ROKSBNKARGOCTL_GIT_TOKEN \
  ghcr.io/jgruberf5/roksbnkargoctl:<tag> init -w demo --config-file config.yaml
```

Everything after the image name is a `roksbnkargoctl` command line. The pieces:

| Part | Why |
|---|---|
| `-v "$PWD:/work"` | The image's working directory and home is `/work` (`HOME=/work`), so workspaces land in `./.roksbnkargoctl` in your current directory and survive the container. Files the config names (the F5 tarball and JWT, an SSH key, CA files) are given as paths under it, such as `config.yaml` or `./certs/mirror-ca.pem` |
| `--user "$(id -u):$(id -g)"` | Files the tool writes are owned by you, not by the image's user (65532) |
| `-e NAME` | Passes a secret through from your environment without putting its value on the command line. Add `-e` for every variable the command reads: `ROKSBNKARGOCTL_MIRROR_PASSWORD`, `BNK_FORGE_PASSWORD`, `ROKSBNKARGOCTL_*` overrides |
| `-it` | A terminal, for the `init` interview and confirmations; omit it in automation and pass `--yes` |

`ROKSBNKARGOCTL_HOME` still overrides where the workspaces go, as on the host.

Inside the image, three things behave differently:

| Command | In the image |
|---|---|
| `self install` | Refuses: it would copy the binary onto a `PATH` inside a container that is gone when it exits |
| `self update` | Refuses, and names the image tag to pull instead (`docker pull ghcr.io/jgruberf5/roksbnkargoctl:<tag>`). `self update --check` works, and names the tag to pull when there is a newer release |
| `agent <cli>` | Refuses: the image has no agent CLI. `agent <cli> --show` works and prints the command to run on the host ([chapter 18](./18-agent.md)) |

The image is `distroless/static` with the standard CA roots. Behind a corporate proxy that
intercepts TLS, calls to IBM Cloud, GitHub, FAR and your registries fail with
`certificate signed by unknown authority` until you mount the proxy's CA into
`/etc/ssl/certs/`:

```sh
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" \
  -v /path/to/proxy-ca.pem:/etc/ssl/certs/proxy-ca.pem:ro \
  -e IBMCLOUD_API_KEY ghcr.io/jgruberf5/roksbnkargoctl:<tag> cos instances
```

To build the image yourself, `make cli-image` (`EXTRA_CA=corp-ca.pem` trusts such a proxy
for the module download only; it is not written into the image).

## Build from the repository

`roksbnkargoctl` is one Go module, `github.com/jgruberf5/roksbnkargoctl`. It needs Go
**1.26** (the module declares `go 1.26.6`) and nothing else: no C toolchain, no cgo, no
other binaries at build time or at run time.

```sh
git clone https://github.com/jgruberf5/roksbnkargoctl.git
cd roksbnkargoctl
make build
./bin/roksbnkargoctl self install
roksbnkargoctl version
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

An unstamped build reports its version as `dev`: `roksbnkargoctl dev for BNK 2.4.0
(commit none, built unknown)`. A build from a checkout that is not exactly on a release
tag deploys the `:dev` check image (see [below](#which-image-a-build-deploys)).

Other targets in the `Makefile`:

| Target | Does |
|---|---|
| `make test` | `go test -race ./...` |
| `make vet`, `make fmt`, `make staticcheck` | `go vet`; fails if any file needs `gofmt`; the same staticcheck CI runs |
| `make verify` | fmt, vet, staticcheck, test, build and the check binary: everything a change must pass |
| `make check-binary` | the `check` binary for Linux, into `bin/check` |
| `make check-image` | builds the `check` image with Docker from `build/check/Dockerfile` |
| `make cli-image` | builds the `roksbnkargoctl` image with Docker from `build/cli/Dockerfile` |
| `make book`, `make book-pdf` | this book as HTML, or as a PDF and an HTML archive in `dist/` (needs Docker) |

### On Windows

The binary is native on Windows. Every feature is implemented in-process, so it does not
shell out to `curl`, `kubectl`, `helm`, `git`, `openssl` or `terraform`; continuous
integration builds and tests it on Windows for every change.

```powershell
go build ./cmd/roksbnkargoctl
.\roksbnkargoctl.exe self install
roksbnkargoctl version
```

## Environment

Set the credentials for the session. On Linux and macOS:

```sh
export IBMCLOUD_API_KEY=…  ARGOCD_AUTH_TOKEN=…  ROKSBNKARGOCTL_GIT_TOKEN=…
```

On Windows:

```powershell
$env:IBMCLOUD_API_KEY = "…"
$env:ARGOCD_AUTH_TOKEN = "…"
$env:ROKSBNKARGOCTL_GIT_TOKEN = "…"
```

The workspace lives under your home directory (your user profile on Windows), in
`.roksbnkargoctl` (see [Workspaces and init](./05-workspaces-and-init.md)); in the
container image, in `./.roksbnkargoctl` of the directory you mount.

## The check image

The prerequisite and lifecycle checks run in ROKS as a container, `check`: one static,
standard-library-only Go binary on `scratch`, running as a non-root user. The binary on
your workstation never runs it; Argo CD does, in the cluster.

It is published, publicly, to `ghcr.io/jgruberf5/roksbnkargoctl-check` for `linux/amd64`
and `linux/arm64`. ROKS workers are `amd64`. (The `:v0.5.0` image's `linux/arm64` variant holds
an `amd64` binary, which fails on an arm64 node with `exec format error`
([#17](https://github.com/jgruberf5/roksbnkargoctl/issues/17)); images built after v0.5.0
carry the right binary for each architecture.)

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
