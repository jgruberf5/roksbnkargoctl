# Connected and disconnected modes

roksbnkargoctl has two independent switches: how BNK licenses (`bnk.mode`) and where the
cluster pulls images and charts from (`registry.source`). This chapter explains both, what
each needs in terms of network egress, and how they combine. After reading it you will be
able to choose the combination that matches your network and know which extra components
to build first.

## The two switches

| Key | Values | Default | Decides |
|---|---|---|---|
| `bnk.mode` | `connected`, `disconnected` | `connected` | Whether BNK licenses directly with F5 or through an F5 License Proxy (FLP) |
| `registry.source` | `far`, `mirror` | `far` | Whether BNK images and charts come from the F5 Artifact Registry or from your private registry |

## Licensing: connected vs disconnected

| | connected | disconnected |
|---|---|---|
| `License.spec.operationMode` | `connected` | `f5licenseproxy` |
| License endpoints | F5 directly: `product.apis.f5.com`, `product-s.apis.f5.com` | The FLP: `teemCertUrl`, `teemEntitlementUrl` and `teemInitialConfigUrl` all set to `https://<flp-private-ip>:8443/license-proxy/v1` |
| FLP root CA | not used | Secret `f5-utils/licenseserver-rootca` (key `licenseserver-rootca.txt`), mounted by CWC at `/etc/cm20/licenseserver-rootca/licenseserver-rootca.txt` (`licenseProxyServerRootCaPath`) |
| Extra component | none | An FLP: `flp up`, or an existing one in `flp.external` |
| Node egress for licensing | `product.apis.f5.com:443`, `product-s.apis.f5.com:443` | The FLP's private IP on `:8443`, over the transit gateway |

In disconnected mode `install` reads the FLP's URL and root CA from `flp-outputs.json`
(written by `flp up`) or from `flp.external.url` and `flp.external.root_ca_file`, writes the
CA Secret, and renders the FLP URL into the `check license` hook. The hook waits for the CA
Secret, then rolls the CWC Deployment (`f5-utils/f5-spk-cwc`) once per CA, because CWC builds
its trust store at start and would ignore a CA written after it started. The rollout is
keyed on the annotation `roksbnkargoctl.io/flp-ca-hash`, so a re-sync with the same CA does
not restart CWC. Only then does it create the `License`.

The FLP itself still needs egress to F5 and to FAR through its own public gateway; only the
cluster is spared that egress. See [The F5 License Proxy](./12-flp.md).

## Registry: FAR vs mirror

| | `far` | `mirror` |
|---|---|---|
| Image host in the CNEInstance and FLO values | `registry.far_host` (default `repo.f5.com`) | `<registry.mirror.host>/<registry.mirror.prefix>` |
| Pull secret | `far-secret`: FAR's fixed user `_json_key_base64` with the service-account key from the FAR auth tarball | `mirror-secret`: `registry.mirror.username` and the password from `$ROKSBNKARGOCTL_MIRROR_PASSWORD`; none if no username |
| Pull secret namespaces | `f5-bnk`, `f5-utils` | `f5-bnk`, `f5-utils`, `cert-manager` (when installed by roksbnkargoctl), `roksbnkargoctl-check` |
| cert-manager images | `quay.io/jetstack/…` | `<mirror>/jetstack/cert-manager-<component>` |
| Check image | `ghcr.io/jgruberf5/roksbnkargoctl-check@sha256:…` | `<mirror>/jgruberf5/roksbnkargoctl-check@sha256:…` |
| Where `render` pulls charts from | FAR (and `quay.io` for cert-manager) | The mirror only |
| Extra work | none | `registry replicate` before the install |

See [Mirroring into a private registry](./13-registry.md) for filling the mirror and for
private CAs.

## What each node must reach

The `check-node-probe` DaemonSet tests these targets from every node, over the node's own
network, before any BNK object is applied. `check pre-install` fails if any node cannot
reach any target.

| Condition | Target | Why |
|---|---|---|
| `registry.source: far` | `<far_host>:443` (TLS) | F5 Artifact Registry |
| `registry.source: far` | `ghcr.io:443` (TLS) | Check image |
| `registry.source: far` and cert-manager installed | `quay.io:443` (TLS) | cert-manager images |
| `registry.source: mirror` | `<mirror-host>:<port>` (default 443, TLS; handshake without verification when `registry.mirror.ca_file` is set) | Private registry |
| `bnk.mode: connected` | `product.apis.f5.com:443`, `product-s.apis.f5.com:443` (TLS) | F5 licensing |
| `bnk.mode: disconnected` | `<flp-ip>:8443` (TLS, self-signed, handshake without verification) | F5 License Proxy |

## What your operator host must reach

| Endpoint | When |
|---|---|
| IBM Cloud APIs (IAM, VPC, transit gateway, Kubernetes Service, resource controller) | Always |
| The ROKS API (public endpoint, or private with `ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1`) | `install`, `uninstall`, `status`, `diagnose`, `render` without `--no-cluster` |
| The Argo CD server | `install`, `uninstall`, `status`, `diagnose` |
| The Git server | `install`, `uninstall --purge-git` |
| COS (`s3.<region>.cloud-object-storage.appdomain.cloud`) | Unless both `cos.local_far_auth_file` and `cos.local_jwt_file` are set |
| FAR | `render`/`install` in `far` mode; `registry bom`, `replicate`, `verify`, `cos verify` and `flp up` in any mode |
| `quay.io` | `render`/`install` in `far` mode with cert-manager installed; `registry replicate` |
| The mirror | `render`/`install` in `mirror` mode; `registry replicate` and `verify` |
| `ghcr.io` | Resolving the check image digest at `render`/`install` (in `mirror` mode optional: without it the mirror's digest is used, with a warning); `registry replicate` copies the check image from it |

## The combinations

| `bnk.mode` | `registry.source` | Nodes need egress to | Build first | Typical use |
|---|---|---|---|---|
| connected | far | FAR, `ghcr.io`, `quay.io`, F5 licensing | nothing | Clusters with internet egress (the default) |
| connected | mirror | The mirror, F5 licensing | `registry replicate` | Images must come from an approved registry; licensing may go out |
| disconnected | far | FAR, `ghcr.io`, `quay.io`, the FLP | `flp up` | Licensing must not leave the network, image pulls may |
| disconnected | mirror | The mirror and the FLP only | `registry replicate`, `flp up` | No internet egress from the cluster |

Connected, disconnected (FLP) and mirror installs have each been installed and uninstalled
on a live cluster.

## Changing modes

`bnk.mode` and `registry.source` are ordinary configuration keys, but BNK has no in-place
upgrade and a mode change alters the `License`, the CNEInstance and the Secrets. Treat a
change of mode as uninstall and install:

```sh
roksbnkargoctl uninstall --yes
# edit config.yaml: bnk.mode / registry.source, plus flp.* or registry.mirror.*
roksbnkargoctl render        # review manifests/git/
roksbnkargoctl install
```

## See also

- [The F5 License Proxy (flp)](./12-flp.md)
- [Mirroring into a private registry (registry)](./13-registry.md)
- [Prerequisites](./03-prerequisites.md)
- [Appendix A. config.yaml reference](./appendix-a-config.md)
