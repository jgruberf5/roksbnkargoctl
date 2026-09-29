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
| C9 | Day-0 install and uninstall only | No testing, gateway or upgrade phases |
| C10 | BNK 2.4 GA only | Manifest `2.4.0` (FAR tag `2.4.0` == `2.4.0-3.3175.0-0.0.380`, same digest) |

## Components

| Command | Does | Talks to |
|---|---|---|
| `init` | Interview, or `--config-file`; resolves cluster → VPC, gateway, COS objects; writes the workspace | IBM |
| `cos` | Publish and discover the FAR auth tarball and subscription JWT | IBM COS |
| `registry` | `bom`, `replicate`, `verify`: FAR → private registry with crane; includes the `check` image | FAR, mirror |
| `flp` | `up`/`down`/`status`: F5 License Proxy on a VSI attached to the transit gateway | IBM VPC, TGW |
| `argocd` | `up`/`down`/`status`: a **test** Argo CD hub (k3s + Argo CD on a VSI), like roksbnkctl's demo hub | IBM VPC, TGW |
| `render` | Writes every manifest the Application syncs into `<workspace>/manifests/` | FAR (chart pulls) |
| `install` | Out-of-band secrets + trusted profile, cluster registration, Git publish, Application create + sync | IBM, ROKS, Git, Argo CD |
| `uninstall` | Deletes the Application (PreDelete/PostDelete checks run in ROKS), then the out-of-band objects | Argo CD, ROKS, IBM |
| `status` | Application sync/health, check results, BNK CR state | Argo CD, ROKS |
| `agent` | Scaffolds `AGENTS.md` + troubleshooting personas into the workspace | local |

The workspace is `~/.roksbnkargoctl/<name>/` (override: `ROKSBNKARGOCTL_HOME`).

## What cannot be YAML, and who does it instead

| Item | Why not YAML | Done by |
|---|---|---|
| IAM trusted profile `<cluster>-f5-cne-controller`, link to SA `f5-bnk/f5-cne-controller`, policies (Viewer+Editor on `is` scoped to the VPC; Viewer on `containers-kubernetes` scoped to the cluster) | IBM IAM | `install` / `uninstall` |
| FAR pull secret (`far-secret` or `mirror-secret`) in `f5-bnk`, `f5-utils`, `cert-manager` | Credential; never in Git | `install`, directly into ROKS |
| Subscription JWT (Secret `bnk-license-jwt`) | `License.spec.jwt` is **required and inline** (no Secret ref in the 2.4 CRD), so a License in Git would leak the JWT | `install` writes the Secret; the `check license` hook builds the License from it |
| FLP root CA (Secret `licenseserver-rootca`) | Produced by `flp up` | `install` |
| ROKS registration in Argo CD (ServiceAccount + token on ROKS, cluster Secret on the hub) | Cross-cluster | `install` via the ROKS API and the Argo CD API |
| Git repo contents | Argo CD's only source | `install` via go-git |

## Rendering

Argo CD's only source is the customer's Git repo (decision 2026-09-29). The hub must not need
FAR, a Helm repo, or plugins. So `render` does all templating on the operator host:

- **cert-manager** `v1.17.3` (Jetstack chart, or the mirror's copy) and **FLO**
  `f5-lifecycle-operator v2.30.0-0.5.2` (from `oci://<chart-host>/charts`) are pulled with
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
| −12 | cert-manager chart (CRDs `Delete=false`; `startupapicheck` disabled — Argo CD's Deployment health is the gate) |
| −10 / −9 / −8 | ClusterIssuer `selfsigned-cluster-issuer` / Certificate `ext-ca` / ClusterIssuer `sample-issuer` |
| −6 | NetworkAttachmentDefinition `ens3-ipvlan-l2`; SCC binding for `flo-f5-lifecycle-operator`; Deployment `check-gateway-api-sweep` |
| −5 | FLO chart (its 26 `k8s.f5.com` CRDs, `Delete=false`) |
| −4 | `CNEManifest bnk-2.4.0` |
| −2 | `CNEInstance f5-bnk-f5-cne-controller` |
| 0 | Hook `check license` (Sync): builds `License` from the JWT Secret, waits `status.state=Active`, then `CNEInstance Available=True` |
| PostSync | Hook `check post-install` |
| PreDelete | Hook `check pre-uninstall` (Argo CD ≥ 3.3) |
| PostDelete | Hook `check post-uninstall` |

The sweep is a **Deployment**, not a hook: the CRDs it waits for are installed by FLO in the
*next* wave, and Argo CD will not start a wave until the previous wave's hooks finish. A
blocking hook would deadlock; a Deployment is healthy as soon as it runs.

Every rendered `kind: Secret` — ours or a chart's (FLO renders `external-otelsvr-secret`) — is
moved to the out-of-band set, and the render refuses to publish if any secret value appears in
a Git object.

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
| `pre-install` | Sync −18 | OpenShift ≥ 4.16; 3 zones; ≥ 3 schedulable workers; no pre-existing BNK not owned by this Application (`--argocd-app`, so re-syncs pass); required Secrets present; with `tmm_replicas` > 1 a ReadWriteMany (`vpc.file.csi.ibm.io`) StorageClass, since TMM replicas share one volume; then deletes every `check-node-probe` pod and waits for fresh verdicts from all nodes (a re-sync never reads a stale one — roksbnkctl #57), failing on any unreachable target |
| `node-probe` | DaemonSet | From each node: DNS + TCP (+TLS handshake) to FAR or the mirror (`:443`), FLP (`:8443`) in disconnected mode, F5 licensing endpoints in connected mode; writes the result to its pod annotation; sleeps |
| `gateway-api-sweep` | Sync −6 | Deletes OpenShift's `openshift-ingress-operator-gatewayapi-crd-admission` VAP + binding every 5 s until `gateways.gateway.networking.k8s.io` and `gatewaysettings.gateway.k8s.f5.com` exist (timeout 20 min) |
| `license` | Sync 0 | Builds `License` from Secret `bnk-license-jwt` (+ FLP URLs/CA path in disconnected mode); waits `Active` (15 min) then `CNEInstance Available` (15 min) |
| `post-install` | PostSync | FLO Ready; `CNEInstance` Available; `License` Active; TMM replicas Ready and spread across zones; no `ImagePullBackOff`; reports |
| `pre-uninstall` | PreDelete | Sweeps `f5validate-*` webhooks; drains `gateway.k8s.f5.com` and `fic.f5.com` (not FLO-managed `k8s.f5.com` components, which FLO re-creates while the CNEInstance lives, nor `k8s.f5net.com` product defaults the webhook refuses — roksbnkctl #266); waits for IPAM to be gone; deletes `License`, then `CNEInstance`, **while FLO still runs** (roksbnkctl #217). A failed drain leaves the CNEInstance and fails the hook |
| `post-uninstall` | PostDelete | Deletes the 34 CWC license secrets; strips `f5.com`/`f5net.com` finalizers from anything stuck in a Terminating namespace; reports leftover F5 CRDs and cluster objects (CRDs are kept by design) |

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

## Uninstall order

`uninstall` → Argo CD deletes the Application → PreDelete `check pre-uninstall` drains F5 CRs
while FLO lives → Argo CD prunes in reverse wave order → PostDelete `check post-uninstall` →
CLI removes the JWT/pull Secrets, the trusted profile, the cluster registration and the
repository entry. Git history is left alone.

## Not in scope

Cluster or gateway creation, BNK upgrades (BNK has no in-place upgrade), the gateway
(Infra/GatewaySettings/Gateway) phase, traffic tests, BNK 2.3.
