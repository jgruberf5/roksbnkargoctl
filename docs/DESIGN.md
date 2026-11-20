# roksbnkargoctl design

roksbnkargoctl installs and uninstalls F5 BIG-IP Next for Kubernetes (BNK) **2.4 GA** on an
existing IBM Cloud ROKS cluster, as a single Argo CD Application in an existing, external
Argo CD. It is the slim successor to [roksbnkctl](https://github.com/jgruberf5/roksbnkctl)
for customers who standardise on Argo CD and rejected Terraform as too heavy.

## Constraints (from the customer)

| # | Constraint | Consequence |
|---|---|---|
| C1 | Argo CD YAML is the only install mechanism | Every in-cluster BNK object is plain YAML in Git, synced by Argo CD |
| C2 | Cluster and transit gateway already exist | Taken by name or ID; never created or deleted by `install` |
| C3 | Argo CD already exists, external to ROKS | ROKS is registered with the hub as a cluster; the hub's Argo CD never runs our containers |
| C4 | IBM APIs, not Terraform | Go REST/SDK calls only; no terraform, helm, kubectl, ibmcloud or git binaries |
| C5 | API key from an environment variable | `IBMCLOUD_API_KEY` (or `IC_API_KEY`); never written to disk |
| C6 | Connected (no FLP) and disconnected (FLP) modes | `bnk.mode: connected \| disconnected` |
| C7 | FAR or a replicated private registry | `registry.source: far \| mirror` |
| C8 | Prereq checks are containers in ROKS, deployed by YAML | The `check` image, run by Argo CD hooks in ROKS |
| C9 | Day-0 install, in-place 2.4 upgrade, uninstall | No testing or gateway phases. `upgrade` moves a running install to a newer 2.4 release; FLO rolls it (see *Upgrade within 2.4*) |
| C10 | BNK 2.4 GA only | Manifest `2.4.3` (FAR tag `2.4.3` == `2.4.3-3.3175.0-0.0.671`, same digest `f93af4f3c58c`). One 2.4 release per build, and `self update` never crosses them: a running install moves to a newer one only through `upgrade`, run with that release's build |

## Components

| Command | Does | Talks to |
|---|---|---|
| `init` | Interview, or `--config-file`; resolves cluster → VPC, gateway, COS objects; writes the workspace | IBM |
| `cos` | Publish and discover the FAR auth tarball and subscription JWT | IBM COS |
| `registry` | `bom`, `replicate`, `verify`: FAR → private registry with crane; includes the `check` image. No workspace needed: every input is a flag (`--mirror-*`, `--far-*`, `--cos-*`); the mirror password only ever comes from an environment variable | FAR, mirror |
| `flp` | `up`/`down`/`status`: F5 License Proxy on a VSI attached to the transit gateway | IBM VPC, TGW |
| `argocd` | `up`/`down`/`status`: a **test** Argo CD hub (k3s + Argo CD on a VSI), like roksbnkctl's demo hub | IBM VPC, TGW |
| `render` | Writes every manifest the Application syncs into `<workspace>/manifests/` | FAR (chart pulls) |
| `install` | Out-of-band secrets + trusted profile, cluster registration, Git publish, Application create + sync | IBM, ROKS, Git, Argo CD |
| `upgrade` | Refuses unless an older 2.4 release is running; sets `bnk.version`, re-renders, publishes, syncs **with prune**, waits for FLO to report every component Available and the License Active | IBM, ROKS, Git, Argo CD |
| `uninstall` | Runs `check pre-uninstall`, deletes the Application, runs `check post-uninstall`, verifies the BNK namespaces are gone, then removes the out-of-band objects | Argo CD, ROKS, IBM |
| `status` | Application sync/health, check results, BNK CR state | Argo CD, ROKS |
| `agent` | Scaffolds `AGENTS.md` + troubleshooting personas into the workspace | local |

The workspace is `~/.roksbnkargoctl/<name>/` (override: `ROKSBNKARGOCTL_HOME`).

## What cannot be YAML, and who does it instead

| Item | Why not YAML | Done by |
|---|---|---|
| IAM trusted profile `<cluster>-f5-cne-controller-<bnk namespace>`, link to SA `f5-bnk/f5-cne-controller`, policies (Viewer+Editor on `is` scoped to the VPC; Viewer on `containers-kubernetes` scoped to the cluster) | IBM IAM | `install` / `uninstall` |
| FAR pull secret (`far-secret` or `mirror-secret`) in `f5-bnk`, `f5-utils`, `cert-manager` | Credential; never in Git | `install`, directly into ROKS |
| Subscription JWT (Secret `bnk-license-jwt`) | `License.spec.jwt` is **required and inline** (no Secret ref in the 2.4 CRD), so a License in Git would leak the JWT | `install` writes the Secret; the `check license` hook builds the License from it |
| FLP root CA (Secret `licenseserver-rootca`) | Produced by `flp up` | `install` |
| Single-certificate source (Secret `roksbnkargoctl-check/bnk-single-cert-source`: your CA, or your `tls.crt`/`tls.key`/`ca.crt`) | Holds a private key | `install`, from the files `bnk.certificates.*` names; the `check cert` hook reads it |
| ROKS registration in Argo CD (ServiceAccount + token on ROKS, cluster Secret on the hub) | Cross-cluster | `install` via the ROKS API and the Argo CD API |
| Git repo contents | Argo CD's only source | `install` via go-git |

## Rendering

Argo CD's only source is the customer's Git repo (decision 2026-09-29). The hub must not need
FAR, a Helm repo, or plugins. So `render` does all templating on the operator host:

- **cert-manager** `v1.17.3` (Jetstack chart, or the mirror's copy) and **FLO**
  `f5-lifecycle-operator v2.30.0-0.11.8` in 2.4.3 (from `oci://<chart-host>/charts`) are pulled with
  crane and templated with the Helm Go SDK (`helm.sh/helm/v3/pkg/action`, client-only).
  No `helm` binary.
- FLO's chart `lookup`s cert-manager CRDs and `fail`s when absent; offline, `lookup` is always
  empty, so render sets `skipCertMgr: true`. That only skips the pre-check.
- Helm hooks in rendered charts keep their `helm.sh/hook` annotations; Argo CD maps them.
- Every object gets `argocd.argoproj.io/sync-wave`. The Application uses one Git path.

### Sync order

| Wave | Objects |
|---|---|
| (install) | Out of band, before any sync: namespace `roksbnkargoctl-check`, SA `check`, its ClusterRole + bindings (incl. SCC `privileged`), and every Secret |
| −20 | Namespaces `f5-bnk`, `f5-utils`, `cert-manager` (`Delete=false`: `check post-uninstall` deletes them) |
| −19 | ConfigMap `registry-ca` + DaemonSet `registry-ca-trust` (private-CA mirror only) |
| −18 | DaemonSet `check-node-probe` (`check node-probe` on every node, host network); hook `check pre-install` (Sync) |
| −10 | Hook `check cert` (Sync), `bnk.certificates.mode: single` only: the one TLS Secret FLO mounts, written into every BNK namespace before FLO |
| −6 | NetworkAttachmentDefinition `ens3-ipvlan-l2`; SCC binding for `flo-f5-lifecycle-operator`; Deployment `check-gateway-api-sweep` |
| 0 | The cert-manager chart (`startupapicheck` disabled) and the FLO chart (its 26 `k8s.f5.com` CRDs), as the Application's Helm sources: no wave annotation. Both set `crds.keep`, so their CRDs carry `helm.sh/resource-policy: keep`, which Argo CD honours on delete (verified on 3.5.1) |
| 1 | Hook `check cert-manager-ready` (Sync): dry-run creates a ClusterIssuer until cert-manager's webhook admits it — Deployment health is not webhook readiness (a reinstall raced the cainjector live). Not in single-certificate mode |
| 2 / 3 / 4 | ClusterIssuer `selfsigned-cluster-issuer` / Certificate `ext-ca` / ClusterIssuer `sample-issuer`. Not in single-certificate mode |
| 6 | `CNEManifest bnk-2.4.0` |
| 8 | `CNEInstance <bnk.namespace>-f5-cne-controller` (`f5-bnk-f5-cne-controller` by default) |
| 10 | Hook `check license` (Sync): builds `License` from the JWT Secret, waits `status.state=Active`, then `CNEInstance Available=True` |
| PostSync | Hook `check post-install` |
| PreDelete | Hook `check pre-uninstall` (Argo CD ≥ 3.3); `uninstall` also runs it as a Job first (see Uninstall order) |
| PostDelete | Hook `check post-uninstall`; `uninstall` also runs it as a Job after the delete |

In single-certificate mode the `cert-manager` namespace, the cert-manager chart (wave 0) and
waves 1 to 4 are not rendered; with `bnk.utils_namespace` equal to `bnk.namespace` wave −20
has one BNK namespace. See *Single certificate and one namespace* below.

The sweep is a **Deployment**, not a hook: the CRDs it waits for are installed by FLO in the
*next* wave, and Argo CD will not start a wave until the previous wave's hooks finish. A
blocking hook would deadlock; a Deployment is healthy as soon as it runs.

Every `kind: Secret` of ours is in the out-of-band set, and the render refuses to publish if
any secret value appears in a Git object or a chart's values file. A Secret a chart ships (FLO's
`external-otelsvr-secret`, a constant in the chart) is chart content: Argo CD renders it from the
chart; it is never in Git.

`registry-ca-trust` must run before any pod pulls from a private-CA mirror, including the
`check` image itself, so it uses an image already cached on every node: the
`openshift-dns/node-resolver` image, looked up from the cluster at `install` time (the approach
roksbnkctl's `ensureRegistryCATrust` proved).

`License`'s CRD is installed at runtime by FLO's crd-installer, not by the chart, so it cannot
be a synced object without `SkipDryRunOnMissingResource`; building it in a hook sidesteps that
and keeps the JWT out of Git.

## The `check` container

One static Go binary on `scratch`. Image `ghcr.io/jgruberf5/roksbnkargoctl-check:<version>`
(public). In mirror mode it is replicated with the BNK artifacts and referenced from the mirror.
It only ever runs in ROKS.

| Mode | Hook | Checks and actions |
|---|---|---|
| `pre-install` | Sync −18 | OpenShift ≥ 4.16; 3 zones; ≥ 3 schedulable workers; no pre-existing BNK not owned by this Application (`--argocd-app`, so re-syncs pass); required Secrets present; a default StorageClass (any class serves any TMM replica count: at Tiny TMM mounts no PVC — roksbnkctl#197); then deletes every `check-node-probe` pod and waits for fresh verdicts from all nodes (a re-sync never reads a stale one — roksbnkctl #57), failing on any unreachable target |
| `node-probe` | DaemonSet | From each node: DNS + TCP (+TLS handshake) to FAR or the mirror (`:443`), FLP (`:8443`) in disconnected mode, F5 licensing endpoints in connected mode; writes the result to its pod annotation; sleeps |
| `gateway-api-sweep` | Sync −6 | Deletes OpenShift's `openshift-ingress-operator-gatewayapi-crd-admission` VAP + binding every 5 s until `gateways.gateway.networking.k8s.io` and `gatewaysettings.gateway.k8s.f5.com` exist (timeout 20 min) |
| `cert` | Sync −10 | Single-certificate mode only. Issuer `self-signed`: CA kept in `roksbnkargoctl-check/<secret>-ca` (generated, or replaced near expiry); `ca`: CA from the source Secret, refused unless valid for longer than `--renew-before`; `provided`: the source Secret's certificate, verified (key match, chain to `ca.crt` through intermediates, valid now, every BNK DNS name and `--ip`). Keeps the Secret when in every namespace its `roksbnkargoctl.io/single-cert-spec` digest matches the settings, it is not within `--renew-before` of expiry and it is signed by the current CA (or is exactly the provided one); otherwise issues (or copies). Writes it to every `--namespace`; when a namespace's pods were not yet restarted onto it (it replaced a certificate there, or an earlier restart failed) and every namespace was written, deletes every pod mounting it (components load it at start; DSSM's Redis does not reload) and records that in the annotation `roksbnkargoctl.io/single-cert-restarted` |
| `cert-manager-ready` | Sync 1 | Server-side dry-run `ClusterIssuer` create, retried on webhook/x509/5xx/404 until admitted (10 min); fails fast on anything else (e.g. RBAC) |
| `license` | Sync 10 | Builds `License` from Secret `bnk-license-jwt` (+ FLP URLs/CA path in disconnected mode); waits `Active` (15 min) then `CNEInstance Available` (15 min) |
| `post-install` | PostSync | `CNEInstance.spec.deploymentSize` is `Tiny` (catches a hand edit in Git); FLO Ready; `CNEInstance` Available; `License` Active; TMM replicas Ready and spread across zones; no `ImagePullBackOff`; reports |
| `pre-uninstall` | PreDelete | Sweeps `f5validate-*` webhooks; drains `gateway.k8s.f5.com` and `fic.f5.com` (not FLO-managed `k8s.f5.com` components, which FLO re-creates while the CNEInstance lives, nor `k8s.f5net.com` product defaults the webhook refuses — roksbnkctl #266); waits for IPAM to be gone; deletes `License`, then `CNEInstance`, **while FLO still runs** (roksbnkctl #217). A failed drain leaves the CNEInstance and fails the hook |
| `post-uninstall` | PostDelete | Deletes the 34 CWC license secrets; strips `f5.com`/`f5net.com` finalizers from anything stuck in a Terminating namespace; reports leftover F5 CRDs and cluster objects (the CLI's `uninstall` then removes the CRDs with no objects left; see *Uninstall order*) |

## Single certificate and one namespace

**Single certificate** (`bnk.certificates.mode: single`, #30) is F5's "Single Certificate for
BNK": no cert-manager (no chart, namespace, issuers or readiness gate; the registry BOM drops
cert-manager's chart and 4 images). FLO runs with `global.certmgr.enabled: false` and
`global.certmgr.secretName: <bnk.certificates.secret_name>` and mounts that one
`kubernetes.io/tls` Secret (`tls.crt`, `tls.key`, `ca.crt`) in place of every cert-manager
Secret; the CNEInstance has no `spec.certificate`. The `check cert` hook (wave −10) writes the
Secret into every BNK namespace. Defaults follow F5's procedure: CN `f5net` (a generated CA
`f5net-ca`), O `F5 Networks`, OU `PD`, C `US`, ST `Washington`, L `Seattle`, RSA 4096 written
PKCS#1, 3650 days, reissued 30 days before expiry; SANs are F5's name list for every BNK
namespace plus `extra_dns_names` and `ip_addresses`. The check ClusterRole gains `create` on
Secrets in this mode only. When a sync replaces a certificate the namespaces already held,
and only once every namespace holds the new one, `check cert` deletes every pod mounting it,
at once: components load it at start, and DSSM's Redis does not reload it (live, it then
failed its own readiness probe against the new CA).

**One namespace** (#31): `bnk.utils_namespace` equal to `bnk.namespace` puts every BNK
component in one namespace; every per-namespace object is rendered once.

Neither may change under a running install. `install` records
`resolved.installed_layout` (`namespaces=<ns>[,<utils>] certificates=<mode>`) and refuses a
different one until `uninstall` clears it; `init --refresh` and the interview keep it.
Switching in place destroyed the utilities namespace (CWC, RabbitMQ, License) on roksbnkctl
with BNK 2.3.

## Modes

- **connected:** `License.operationMode: connected`; nodes need egress to FAR (or the mirror)
  and F5 licensing.
- **disconnected:** an FLP VSI on the transit gateway (`flp up`, f5-license-proxy
  `1.29.0-0.10.40` pinned: the 2.4.0 manifest no longer lists it — roksbnkctl#327), `operationMode:
  f5licenseproxy`, `teem*Url: https://<flp-private-ip>:8443/license-proxy/v1`,
  `licenseProxyServerRootCaPath: /etc/cm20/licenseserver-rootca/licenseserver-rootca.txt`.
  Typically paired with `registry.source: mirror`.

## Argo CD

- Access: REST API, `argocd.server` + token from `ARGOCD_AUTH_TOKEN`. Argo CD **≥ 3.3**
  (PreDelete hooks) is enforced.
- Git SSH host keys: github.com, gitlab.com and bitbucket.org (and their port-443 SSH
  endpoints) are built in, verified against the providers' published fingerprints;
  `git.known_hosts_file` adds hosts, is validated before anything changes, and is uploaded to
  Argo CD's certificate store (literal host names only). `ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1`
  switches verification off, with a warning.
- Registration: `install` creates ServiceAccount `roksbnkargoctl-argocd-manager` in
  `kube-system` on ROKS, a ClusterRoleBinding to `cluster-admin`, and a long-lived token Secret;
  then `POST /api/v1/clusters` with the ROKS **private** service endpoint (reached over the
  transit gateway) unless `argocd.cluster_endpoint: public`.
- Repository: `POST /api/v1/repositories` with the Git URL and credentials.
- Application: `bnk-<workspace>`, project `default` unless set, finalizer
  `resources-finalizer.argocd.argoproj.io`, `syncOptions: [ServerSideApply=true,
  RespectIgnoreDifferences=true]`, manual sync (the sync is the operator's approval).
- Health: the hub's `argocd-cm` is the customer's, so no Lua is required. Without Lua, Argo CD
  treats F5 CRs as healthy on creation; the `check license` hook (which waits for `License`
  Active and `CNEInstance` Available) is the real gate, and fails the sync if BNK does not come
  up. `install` prints an optional Lua snippet for operators who want CR health in the UI.

## deploymentSize: Tiny only

IBM ROKS runs only `deploymentSize: Tiny`. F5's CRD offers `Tiny`, `Small`, `Medium`, `Large`
and `Max`; every size above Tiny requests hugepages, and ROKS workers report
`hugepages-2Mi: 0` (verified on bnkargo) with no Machine Config Operator to allocate them
(roksbnkctl#203). So Tiny is a literal in the renderer, `config.yaml` has no size key (strict
parsing rejects one), and `check post-install` fails any CNEInstance that is not Tiny. At Tiny,
TMM mounts no persistent volume, so multiple TMM replicas need no ReadWriteMany storage.

## Findings from the live runs (bnkargo, OpenShift 4.21.31; Argo CD 3.5.1)

These shaped the implementation; each has a regression test that fails against it.

- **The ROKS private endpoint is not publicly trusted.** It chains to the cluster's own root
  CA (the same CA as a ServiceAccount token Secret's `ca.crt`); the public endpoint chains to
  public roots. Registration sends the CA only for the private endpoint.
- **Never apply `annotations: {}`.** A Namespace server-side-applied with an empty annotations
  map never receives OpenShift's `openshift.io/sa.scc.*` annotations, and every pod in it is
  rejected. Empty metadata maps are pruned in the renderer and in `kube.Apply`.
- **Render the check image by digest.** With a mutable tag and `IfNotPresent`, nodes keep
  running whatever they cached; a re-sync after a check fix would run the old check.
- **Argo CD rejects a body-less DELETE without `Content-Type`** (415). Every non-GET request
  declares JSON.
- **IBM VPC paging drops `version`** from `next.href`; it is carried onto every page.
- **The 2.4.0 GA manifest no longer lists the license proxy** (2.3.x did): its version is
  pinned (roksbnkctl#327).
- **cert-manager's webhook can lag its Deployment**: gated by `cert-manager-ready`.

Proven live: connected install, re-sync, uninstall, reinstall on a used cluster;
disconnected (FLP) install and uninstall; mirror mode (Artifactory, 93 artifacts) install and
uninstall with 3 TMM replicas on VPC block storage; `argocd up` and `flp up/down`. Not yet run
live: a private-CA mirror, an anonymous mirror, `flp.external`.

Correction: those uninstalls ended clean because the PostDelete check ran; none of their
saved logs shows a pre-uninstall run, so the drain was never proven through Argo CD's hook
(see Uninstall order). The CLI-run checks are what a live uninstall has to prove next.

## Uninstall order

`uninstall` runs `check pre-uninstall` as a Job it waits on (drains F5 CRs while FLO lives;
a failure stops here and the Application is NOT deleted, unless `--force`) → deletes the
Application with foreground cascading; Argo CD prunes in reverse wave order → runs
`check post-uninstall` as a Job while any BNK namespace remains → fails unless `f5-bnk`,
`f5-utils` and (when installed) `cert-manager` are gone → removes the JWT/pull Secrets, the
check namespace, the trusted profile, the cluster registration and the repository entry →
removes the CRDs BNK brought (unless `--keep-crds`). Git history is left alone. Re-running
`uninstall` with the Application already gone runs `check post-uninstall` again, so an
interrupted uninstall finishes.

**CRDs.** They used to stay: cluster-scoped, shared by any BNK on the cluster, and deleting a
CRD deletes every object of its kind. The cost (#44): on bnkargo the experimental Gateway API
CRDs BNK installs (`backendtlspolicies`, `tcproutes`, `tlsroutes`, `udproutes`, v1.4.1; OpenShift
owns the standard channel and its admission policy against others is what
`check-gateway-api-sweep` removes) left `clusteroperator/ingress` Degraded ("Unmanaged Gateway
API CRDs found"), so `clusterversion/version` sat at 4.21.35 `Partial` from 2026-11-02 with
`Upgradeable=False` — no OpenShift update could start, long after BNK was uninstalled. And 101
F5 and cert-manager CRDs had accumulated. `uninstall` now removes F5's CRDs (groups `f5.com`,
`f5net.com` and their subdomains), experimental-channel Gateway API CRDs, and cert-manager's
when the workspace installed it — never OpenShift's standard Gateway API CRDs or anything else.
The hazards are the guards: nothing at all while a FLO Deployment or a CNEInstance exists
anywhere on the cluster, and a CRD only when no object of its kind is left anywhere, so
deleting it destroys nothing and cannot hang on a finalizer. Each kept CRD is reported with
why. `--keep-crds` keeps them all. While BNK is installed the experimental CRDs exist and the
ingress operator is Degraded; that conflict is BNK's (F5's) to resolve, not the uninstall's.

**Why the CLI runs the checks and not only Argo CD.** Argo CD 3.5.1 can declare a
PreDelete/PostDelete hook complete the moment it creates it (argoproj/argo-cd#29100, open):
a second finalization pass gets `AlreadyExists` from the create, finds no hook in its
not-yet-updated cache, and removes the finalizer. Live on bnkargo the pre-uninstall pod got
SIGTERM 0.4s after it started, the cascade pruned FLO beneath it, post-uninstall never ran,
and `f5-bnk`, `f5-utils` and `cert-manager` stayed behind while the old `uninstall` printed
"uninstalled". The hooks stay in Git so a delete from the Argo CD UI still tries them, best
effort; both checks are idempotent, so a hook that does run after the CLI's copy finds
nothing left. `install` also keeps Argo CD's `pre-delete-finalizer…`/`post-delete-finalizer…`
finalizers when it re-upserts the Application: Argo CD's upsert replaces the list.

The upsert replaces the spec too, chart versions included. The FLO chart's version is in the
Application, not Git, so an operator who runs BNK from the Argo CD UI changes it there (runs
112 and 114). The render's version, the one the release manifest lists, stays the source of
truth, but `install` and `upgrade` read the hub's Application first and name every Helm chart
whose `targetRevision` they change: `upgrade` as the change it makes
(`Application: chart f5-lifecycle-operator v2.30.0-0.11.4 -> v2.30.0-0.11.8`), `install` as a
warning that it is putting the manifest's version back over an edit made outside roksbnkargoctl.

## The binary itself: `self install`, `self update`, install.sh, install.ps1

A binary installs one BNK release (`config.BNKVersion`), and its release archive says which:
`roksbnkargoctl_<version>_bnk-<BNK>_<os>_<arch>.tar.gz` (`.zip` on Windows), beside
`roksbnkargoctl_<version>_checksums.txt`. Later BNK lines ship as sibling archives.

- `self update` installs only the archive for its own BNK version. "Latest" is the highest
  non-draft, non-prerelease `vX.Y.Z` that has that archive; a newer release cut for another
  line is skipped. `--version` naming a release without it is refused, naming the BNK
  versions the release does have. It never switches lines.
- The archive's SHA256 must match the release's checksums file; a release without one, or
  one that does not list the archive, is refused (goreleaser always publishes it).
- Replace is a same-directory temp file and a rename. Windows cannot overwrite a running
  .exe, so it moves the binary aside to `.old` first and rolls back on failure; if the
  rollback also fails the error names the sidecar and the `Move-Item` that restores it, and
  the exit code is 125. The `.old` is removed at the next start.
- Everything is in-process Go (net/http, archive/tar, archive/zip, crypto/sha256).
- `install.sh`/`install.ps1` resolve GitHub's latest release (or `VERSION`), download the
  `BNK_VERSION` archive (default 2.4.3) and fail, naming the BNK versions present, when that
  release lacks it. They verify the checksum (mandatory) and hand off to
  `self install --force`.

## The CLI image

`ghcr.io/jgruberf5/roksbnkargoctl` runs the tool with only Docker (`build/cli/Dockerfile`,
`.github/workflows/cli-image.yml`): `:vX.Y.Z` (+ `:latest` for a final release) on a tag,
`:dev` from main, `:pr-N` for a same-repo PR; linux/amd64 and arm64. The binary is stamped
like a release (`Version` is the tag, so it runs the check image of the same tag).

- `distroless/static:nonroot`, not scratch: CA roots, tzdata and a writable `/tmp`
  (gitpub writes known_hosts to `os.TempDir()`). Runs as 65532; works under any `--user`.
- `HOME=/work`, the working directory and the mount point: workspaces land in
  `/work/.roksbnkargoctl`, i.e. the host directory, owned by the host user under
  `--user "$(id -u):$(id -g)"`. An unknown uid would otherwise get `HOME=/`.
- The image sets `ROKSBNKARGOCTL_CONTAINER=1`, and only exactly `1` counts. There, `self
  update` and `self install` refuse before any network call and name the image tag to pull;
  `self update --check` works and names it too. `agent <cli>` refuses before scaffolding
  (the image has no agent CLI); `agent <cli> --show` works.
- Secrets pass with `-e`; a TLS-intercepting proxy's CA can be mounted into
  `/etc/ssl/certs/` (Go reads every file there). Building behind one takes the optional
  `extra_ca` build secret, used by `go mod download` only.

**Deleting from the Argo CD UI** (what a customer did, 2026-11): the hooks are the only
uninstall checks then. Measured on bnkargo with one namespace (run 92): Argo CD raised a run of
transient `DeletionError … not found` conditions while it pruned (`check-license`,
`check-pre-install`, the CNEManifest, `selfsigned-cluster-issuer`, the FLO SCC
ClusterRoleBinding) — objects something else had already deleted; each clears on the next
pass and the delete carries on. The same messages appear during a clean `uninstall`. The
PostDelete hook ran that time and deleted `f5-bnk` and `cert-manager`; when it does not
(#29100), they stay. Either way the out-of-band objects stay, because they are not the
Application's: the check namespace and RBAC, `roksbnkargoctl-argocd-manager` and the trusted
profile. `roksbnkargoctl uninstall` afterwards finds the Application gone, runs `check
post-uninstall` if a BNK namespace remains, and removes them (5s in run 92).

## Upgrade within 2.4

`upgrade`, run with the newer release's build, reads the running release from the cluster
(the CNEInstance's `spec.manifestVersion`, not the workspace: a workspace can be edited, and an
install made before this command has no record), then does what `install` does with
`bnk.version` set to the build's release, syncing with prune, and waits for FLO.

| Running | `install` | `upgrade` |
|---|---|---|
| none | installs | refuses: use `install` |
| this build's release | re-syncs | re-syncs with prune and waits: finishes an interrupted upgrade |
| older, same line | **refuses: use `upgrade`** | upgrades |
| newer | refuses: no downgrades | refuses |
| another line | refuses | refuses |

The check runs before the first change in the cloud or Git, after the pending install record
(local, locks nothing). Before it existed, `install` with a newer build over a running
install upgraded it without being asked, and an older build downgraded it.

Measured, 2.4.1 → 2.4.2 in one namespace (run 93, bnkargo, OpenShift 4.21.40): the change to
the CNEManifest and `manifestVersion` is all FLO needs. It rolled crd-installer, fluentd, the
TMM pod manager, f5ingress, coremond and qkview, then TMM one pod at a time; the CNEInstance's
`Available` was False during the roll, and every `*Available` condition True 3m38s after the
sync started (3m53s to the end of the sync). The License stayed Active throughout. Without
prune the old CNEManifest `bnk-2.4.1` stayed and the Application was OutOfSync: hence the prune
sync. `upgrade` waits on the CNEInstance and the License, not the sync alone.

Its first live run (run 94, 2.4.1 → 2.4.2) ended on one timed-out read of the Application: the
hub stopped answering for minutes, and `WaitOperation` returned on the first error although
the sync succeeded. The waits now poll through a read that gets no answer, a 429 or a 5xx
until their own timeout, and stop at once on an answer that says no (401, 403, 404). The
CNEInstance asks for the new release as soon as the sync applies it, so an upgrade
interrupted after that point is finished by running `upgrade` again: with its release
running, it re-syncs with prune and waits. 2.4.1 and 2.4.2
ship the same FLO chart, so a FLO chart upgrade was not exercised there.

Without the CLI (run 112, 2.4.2 → 2.4.3, bnkargo): a commit changing only the CNEInstance's
`manifestVersion`, then a sync with Argo CD's defaults, is a complete release upgrade. FLO finds
no CNEManifest for the new release, pulls `release/f5-bigip-k8s-manifest:<release>` from its
registry and creates `bnk-<release>` itself (1m14s after the sync), then rolls; the last TMM
was replaced 2m59s after the sync. The FLO chart is not part of that: it is in the
Application's sources, not Git, and stays on the old release's (2.4.3 ships `0.11.8`, 2.4.2
`0.11.4`) until the Application is edited. Moving it replaced only the FLO pod; BNK stayed
Available. `upgrade` still publishes the new CNEManifest and syncs with prune, so Git matches
the cluster. Putting the Application in Git is #45.

F5 documents the other order, for every release since 2.2: FLO first (`helm upgrade` to the
release's FLO chart), then the CNEInstance. `upgrade` already does that within its one sync,
because the FLO chart is wave 0 and the CNEInstance wave 8. Measured in that order through
the Application and Git (run 114): FLO 0.11.4 → 0.11.8 on 2.4.2 replaced only the FLO pod;
then the `manifestVersion` commit rolled TMM between 2m06s and 3m21s after the sync, Argo CD
reported the sync Succeeded at 6m22s while `DSSMAvailable` was still False, and every condition
was True at 9m09s. That is why `upgrade` waits on the CNEInstance, not the sync. In either
order FLO leaves the CNEManifest it creates (no ownerReferences, no finalizers) after the
Application is deleted, and `uninstall` then keeps the `cnemanifests` CRD.

In mirror mode, replicate the new release into the mirror first (`registry replicate` with the
new build): render pulls the new manifest from the mirror.

## Not in scope

Cluster or gateway creation, the gateway (Infra/GatewaySettings/Gateway) phase, traffic
tests, BNK 2.3, upgrades across BNK lines (2.4 → 2.5) and downgrades. Within 2.4, see
*Upgrade within 2.4*. (`roksbnkargoctl self update` updates the tool itself, not BNK.)
