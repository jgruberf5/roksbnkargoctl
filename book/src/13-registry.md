# Mirroring into a private registry (registry)

A cluster without internet egress cannot pull from the F5 Artifact Registry (FAR),
`quay.io` or `ghcr.io`. This chapter describes `roksbnkargoctl registry`, which copies every
artifact an install pulls into a registry you control (Harbor, Artifactory, a plain
`registry:2`, IBM Container Registry), and how to switch an install to it. After reading it
you will be able to fill and verify a mirror, with or without a workspace, trust a mirror
with a private CA, and keep the mirror current when you upgrade roksbnkargoctl.

## Commands

| Command | Does |
|---|---|
| `roksbnkargoctl registry bom` | Lists every artifact (kind and source reference) on stdout, and the count on stderr |
| `roksbnkargoctl registry replicate [--concurrency 2] [--state FILE]` | Copies the list into the mirror, skipping what is already there |
| `roksbnkargoctl registry verify` | Reports every artifact missing from the mirror; exits non-zero if any is |

All three read the BNK manifest from **FAR**, so they need the FAR auth key (from COS or
`cos.local_far_auth_file`) whatever `registry.source` says. `replicate` and `verify` need
`registry.mirror.host`. None of them needs a workspace
([below](#without-a-workspace)).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `registry.source` | `far` | `mirror` makes the install pull from the mirror |
| `registry.far_host` | `repo.f5.com` | FAR's registry host |
| `registry.mirror.host` | none | Bare `host[:port]`, no scheme |
| `registry.mirror.prefix` | empty | Optional path under the host, for example the Artifactory repository name |
| `registry.mirror.username` | empty | Mirror login; empty means anonymous and no pull secret |
| `registry.mirror.password_env` | `ROKSBNKARGOCTL_MIRROR_PASSWORD` | Environment variable holding the mirror password |
| `registry.mirror.ca_file` | empty | PEM of the mirror's CA, when it is not publicly trusted |

The mirror is addressed as `<host>/<prefix>`, the *mirror base*. The registry commands use
`registry.mirror.*` even while `registry.source` is still `far`: you fill the mirror first
and switch afterwards.

## The bill of materials

For BNK 2.4.0 with cert-manager installed by roksbnkargoctl (the default), the list has
**93 artifacts**:

| Artifacts | Count | Source |
|---|---|---|
| BNK manifest chart | 1 | `repo.f5.com/release/f5-bigip-k8s-manifest:2.4.0` |
| Charts listed by the manifest | 25 | `repo.f5.com/<charts/…>:<version>` |
| Images listed by the manifest | 61 | `repo.f5.com/<images/…>:<version>` |
| cert-manager chart | 1 | `quay.io/jetstack/charts/cert-manager:v1.17.3` |
| cert-manager images | 4 | `quay.io/jetstack/cert-manager-{controller,webhook,cainjector,acmesolver}:v1.17.3` |
| Check image | 1 | `ghcr.io/jgruberf5/roksbnkargoctl-check:<version>` (or `check.image`) |

With `bnk.cert_manager.install: false` the five cert-manager artifacts are left out. The
F5 License Proxy images are not in the list: the FLP VSI pulls them from FAR itself.

```text
$ roksbnkargoctl registry bom
chart  repo.f5.com/release/f5-bigip-k8s-manifest:2.4.0
chart  repo.f5.com/charts/…
image  repo.f5.com/images/…
…
image  ghcr.io/jgruberf5/roksbnkargoctl-check:vX.Y.Z
93 artifacts
```

## How artifacts are copied

| Rule | Detail |
|---|---|
| Path | The source reference minus its host, under the mirror base: `repo.f5.com/images/f5-tmm` → `<mirror>/images/f5-tmm`; `quay.io/jetstack/cert-manager-controller` → `<mirror>/jetstack/cert-manager-controller`; `ghcr.io/jgruberf5/roksbnkargoctl-check` → `<mirror>/jgruberf5/roksbnkargoctl-check`. The renderer uses the same rule, so what `replicate` writes is exactly what the install references |
| `+` in versions | `+` is not legal in an OCI tag, and Helm stores a chart version like `14.91.12+0.4.7` under tag `14.91.12_0.4.7`; the copy uses the `_` form |
| Architecture | Images are narrowed to `linux/amd64`, the ROKS worker architecture. Charts and single-architecture artifacts are copied as they are |
| Skip | If the mirror already has the artifact with the source's digest it is reported `present` and not copied again, so re-running is cheap |
| Retries | Connection resets, timeouts, `EOF`, 502/503/504 and `TOOMANYREQUESTS` are retried, up to 4 attempts |
| Concurrency | `--concurrency` parallel copies, default 2 |
| Record | `<workspace>/registry-mirror.json`, or the file `--state` names: mirror base, BNK version, counts of copied and skipped artifacts, the failed ones, timestamp. Without a workspace none is written unless `--state` is given. It is a report; nothing reads it back |

Progress is printed per artifact as `[n/93] copied …`, `[n/93] present …`, or a warning with
the error. `replicate` exits non-zero if any artifact failed; run it again to retry just
those.

`verify` checks each artifact's destination and lists what is missing
(`MANIFEST_UNKNOWN`, `NAME_UNKNOWN` or 404). Any other error, for example an authentication
failure, is reported as an error rather than as missing.

## Credentials

Credentials are scoped to their own host. The FAR key (username `_json_key_base64`) is sent
only to `registry.far_host`, and the mirror login only to the mirror host, never to
`quay.io` or `ghcr.io`. The mirror password is read from the environment variable named by
`registry.mirror.password_env` and never written to disk:

```sh
export ROKSBNKARGOCTL_MIRROR_PASSWORD='…'
roksbnkargoctl registry replicate
```

The account needs push rights for `replicate` and pull rights for the cluster. In mirror
mode `install` writes the same login into Secret `mirror-secret` in `f5-bnk`, `f5-utils`,
`cert-manager` (when roksbnkargoctl installs it) and `roksbnkargoctl-check`, keyed by the mirror host (kubelet matches pull
secrets by host, not by path).

## Without a workspace

Every setting the three commands read is also a flag, bound to its `config.yaml` key, so
they run with `--no-workspace` (or with no workspace selected) from flags and
`ROKSBNKARGOCTL_*` variables alone. With a workspace, a flag wins over `config.yaml` for
that run and is not saved.

| Flag | Key it overrides | `bom` | `replicate`, `verify` |
|---|---|---|---|
| `--far-host` | `registry.far_host` | yes | yes |
| `--far-auth-file` | `cos.local_far_auth_file` | yes | yes |
| `--cos-instance`, `--cos-bucket`, `--cos-region` | `cos.instance`, `cos.bucket`, `cos.region` | yes | yes |
| `--far-auth-object` | `cos.far_auth_object` | yes | yes |
| `--api-key-env` | `ibmcloud.api_key_env` (for reading the FAR key from COS) | yes | yes |
| `--cert-manager-install` | `bnk.cert_manager.install` (`--cert-manager-install=false` leaves the five cert-manager artifacts out) | yes | yes |
| `--cert-manager-version` | `bnk.cert_manager.version` | yes | yes |
| `--check-image` | `check.image` (default: this build's check image) | yes | yes |
| `--mirror-host` | `registry.mirror.host` | | yes |
| `--mirror-prefix` | `registry.mirror.prefix` | | yes |
| `--mirror-username` | `registry.mirror.username` | | yes |
| `--mirror-password-env` | `registry.mirror.password_env`: the **name** of the variable holding the password | | yes |
| `--mirror-ca-file` | `registry.mirror.ca_file` | | yes |

`replicate` also takes `--concurrency` and `--state`. **The mirror password is never a
flag.** It is read from the environment variable `--mirror-password-env` names (default
`ROKSBNKARGOCTL_MIRROR_PASSWORD`), so it never appears in a command line or a shell history.

```sh
export ROKSBNKARGOCTL_MIRROR_PASSWORD='…'
roksbnkargoctl registry replicate --no-workspace \
  --far-auth-file f5-far-auth-key.tgz \
  --mirror-host registry.example.com --mirror-prefix bnk --mirror-username robot \
  --state ./registry-mirror.json
roksbnkargoctl registry verify --no-workspace --far-auth-file f5-far-auth-key.tgz \
  --mirror-host registry.example.com --mirror-prefix bnk --mirror-username robot
```

Instead of `--far-auth-file`, `--cos-bucket` (with `--cos-instance`, unless it is the
default `bnk-supply-chain`) reads the FAR key from COS, finding the bucket's region as
[chapter 14](./14-cos.md#finding-the-buckets-region) describes. With neither, the commands
stop before contacting anything:

```text
no FAR key: pass --far-auth-file <f5-far-auth-key.tgz>, or --cos-bucket (with --cos-instance) to read it from COS
```

and `replicate` or `verify` without a mirror host:

```text
registry.mirror.host is not set: pass --mirror-host (or set ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST, or registry.mirror.host in config.yaml)
```

The mirror you fill this way must be the one the install later names in
`registry.mirror.*`, with the same `--check-image` (or `check.image`): the install pins the
check image by digest and requires that digest in the mirror
([below](#keeping-the-check-image-current)).

## A mirror with a private CA

Set `registry.mirror.ca_file` to the CA's PEM file. It is used in three places:

- **On your host:** trusted, in addition to the system roots, for every pull and push.
- **On every node:** the renderer adds ConfigMap `roksbnkargoctl-check/registry-ca` and the
  privileged DaemonSet `roksbnkargoctl-check/registry-ca-trust` in wave −19. Its pod copies
  the CA to `/etc/containers/certs.d/<mirror-host>/ca.crt` and
  `/etc/docker/certs.d/<mirror-host>/ca.crt` on the host, then sleeps.
- **In Argo CD:** `install` adds it as a TLS certificate for the mirror's host name
  (without the port), so Argo CD verifies the mirror when it pulls the charts. It stays in
  Argo CD after `uninstall`.

The DaemonSet must run before any pod pulls from the mirror, including the check image
itself, so it cannot use an image from the mirror. It runs on the
`openshift-dns/node-resolver` image, which every ROKS node already has cached
(`imagePullPolicy: IfNotPresent`). `install` reads that image reference from the cluster and
caches it in `resolved.node_resolver_image` (which `render --no-cluster` then uses).

Because the node probe runs before the CA is in its own trust store, it probes a private-CA
mirror with a TLS handshake without verification and reports the certificate subject.

## Switching an install to the mirror

1. Configure `registry.mirror.*` (and export the password).
2. `roksbnkargoctl registry replicate`
3. `roksbnkargoctl registry verify` — expect `all 93 artifacts present in the mirror`.
4. Set `registry.source: mirror`.
5. `roksbnkargoctl render` and review `manifests/git/` and `manifests/application.yaml`:
   images, the check image and the Application's chart sources now reference the mirror
   base, and the pull secret is `mirror-secret`.
6. `roksbnkargoctl install`.

Do this before the first install. For a cluster that already runs BNK from FAR, treat the
switch like any mode change: uninstall, then install from the mirror (see
[Connected and disconnected modes](./11-modes.md#changing-modes)).

A mirror install on a live cluster (Artifactory, 93 artifacts) pulled every pod image from
the mirror.

## Keeping the check image current

The check image tag follows the roksbnkargoctl release, and in mirror mode `install`
requires the exact upstream digest of that tag to be in the mirror. After upgrading
roksbnkargoctl, run `registry replicate` again before `install`: everything else is reported
`present` and only the new check image is copied. If you forget, `install` stops at the
render step with:

```text
the mirror has an older copy (sha256:…) of the check image, not sha256:… (ghcr.io/jgruberf5/roksbnkargoctl-check:vX.Y.Z): run `roksbnkargoctl registry replicate` so the cluster runs the current check
```

## See also

- [Connected and disconnected modes](./11-modes.md)
- [install](./08-install.md#the-check-image)
- [The Argo CD Application](./07-the-application.md)
- [Appendix A. config.yaml reference](./appendix-a-config.md)
