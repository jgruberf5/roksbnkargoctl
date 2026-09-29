# D. Security model

This appendix describes how roksbnkargoctl handles secrets, what permissions it
creates in ROKS, IBM Cloud and Argo CD, and how it verifies the parties it talks
to. Use it to review the tool with your security team: every object and permission
named here is one the tool creates, and every secret is shown with where it lives
at each step.

## Principle: no secret value in Git

Your Git repository is Argo CD's only source, and it is typically readable by far
more people than the cluster. So **no secret value is ever published to it**:

- Every `kind: Secret` a render produces — the tool's own, or one a Helm chart
  renders (FLO's chart renders `external-otelsvr-secret`) — is moved out of the Git
  set into the **direct** set, which `install` writes straight into ROKS.
- The subscription JWT cannot live in a `License` in Git either, because the BNK 2.4
  CRD requires it **inline** in `spec.jwt`. It is written to a Secret instead, and
  the `check license` hook builds the License inside the cluster.
- As a last line of defence, the render **refuses to publish** if the registry
  password or the subscription JWT appears anywhere in any Git object:

  ```text
  render: refusing to publish: the subscription JWT appears in <Kind> <namespace>/<name>
  ```

You can confirm the whole of what Git will contain before anything is published:
`roksbnkargoctl render` writes it to `manifests/git/`.

## Where each secret lives

| Secret | Source | On the operator host | In ROKS | Elsewhere |
|---|---|---|---|---|
| IBM Cloud API key | you | environment only (`IBMCLOUD_API_KEY` or `IC_API_KEY`); never written | — | — |
| Argo CD API token | you (or `argocd up`) | environment only (`ARGOCD_AUTH_TOKEN`) | — | — |
| Git token | you | environment only (`ROKSBNKARGOCTL_GIT_TOKEN`) | — | Argo CD repository credential (`install` registers the repo) |
| Git SSH key | you | the file at `git.ssh_key_file` (yours) | — | Argo CD repository credential |
| Mirror password | you | environment only (`ROKSBNKARGOCTL_MIRROR_PASSWORD`) | pull Secret `mirror-secret` | — |
| FAR auth key | MyF5 | read from COS (or `cos.local_far_auth_file`) at run time | pull Secret `far-secret` (`kubernetes.io/dockerconfigjson`) | COS object `f5-far-auth-key.tgz` |
| Subscription JWT | MyF5 | read from COS (or `cos.local_jwt_file`) at run time | Secret `roksbnkargoctl-check/bnk-license-jwt`; `License.spec.jwt` (built in-cluster) | COS object `subscription.jwt` |
| FLP root CA and key | `flp up` | `flp-outputs.json` (mode 0600; holds the CA private key so a re-run reuses the CA) | Secret `f5-utils/licenseserver-rootca` (certificate only) | the proxy VSI |
| Argo CD cluster credential | `install` | — | Secret `kube-system/roksbnkargoctl-argocd-manager-token` | Argo CD's cluster Secret on the hub |
| Test hub admin password | `argocd up` | `argocd-hub.json` (mode 0600) | — | the test hub |

The pull Secret is created in the BNK and utils namespaces, and additionally in
`roksbnkargoctl-check` and (when the tool installs cert-manager) `cert-manager` in
mirror mode, because those images then come from the mirror too.

Secrets are read from environment variables whose **names** are in `config.yaml`
(`*_env` keys); an error names the variable, never the value.

## Redaction

| Where | What is done |
|---|---|
| `manifests/direct/` | every Secret is written with each value replaced by `REDACTED` (base64 `UkVEQUNURUQ=` under `data`), and annotated `roksbnkargoctl.io/redacted` explaining that the real Secret is written to the cluster by `install`, never to Git. `uninstall` needs only kinds and names from these files |
| `diagnose` | never collects Secret objects; `license.yaml` has `spec.jwt` replaced with `REDACTED` |
| `uninstall` log collection | check pod logs only |
| `check license` | logs the JWT's byte count, never its value |

## RBAC created in ROKS

### The check ServiceAccount

`install` creates namespace `roksbnkargoctl-check`, ServiceAccount `check`, and
ClusterRole + ClusterRoleBinding `roksbnkargoctl-check`. The role is broad on the
F5 API groups — the uninstall checks delete whatever F5 custom resources exist —
and narrow elsewhere:

| API group | Resources | Verbs | Used by |
|---|---|---|---|
| core | `nodes`, `namespaces`, `pods`, `services`, `events`, `persistentvolumeclaims`, `serviceaccounts`, `configmaps`, `secrets` | get, list, watch | all modes |
| core | `pods`, `secrets`, `configmaps`, `services`, `serviceaccounts`, `persistentvolumeclaims` | patch, delete | probe annotation, probe restart, CWC license secret cleanup |
| core | `namespaces` | delete, patch | post-uninstall |
| core | `namespaces/finalize` | update | post-uninstall |
| `apps` | `deployments`, `daemonsets`, `statefulsets`, `replicasets`, `controllerrevisions` | get, list, watch | FLO and probe lookups |
| `apps` | `deployments`, `daemonsets`, `statefulsets` | patch | CWC roll (FLP CA) |
| `batch` | `jobs` | get, list, watch | |
| `storage.k8s.io` | `storageclasses` | get, list, watch | pre-install |
| `apiextensions.k8s.io` | `customresourcedefinitions` | get, list, watch | CRD waits, leftover report |
| `config.openshift.io` | `clusterversions` | get, list, watch | pre-install |
| `admissionregistration.k8s.io` | `validatingadmissionpolicies`, `validatingadmissionpolicybindings`, `validatingwebhookconfigurations` | get, list, delete | gateway-api-sweep, F5 webhook sweep |
| `cert-manager.io` | `certificates`, `clusterissuers`, `issuers` | get, list, watch | |
| `cert-manager.io` | `clusterissuers` | create | cert-manager-ready's dry run (a dry-run create is authorized as a create; nothing is created) |
| `k8s.f5.com`, `k8s.f5net.com`, `gateway.k8s.f5.com`, `fic.f5.com`, `metrics.f5.com` | `*` | get, list, watch, create, update, patch, delete | license, pre-/post-uninstall |

The ServiceAccount is also bound to OpenShift's **privileged** SCC
(ClusterRoleBinding `system:openshift:scc:privileged:roksbnkargoctl-check:check`).
Two check workloads need it: the node probe runs on the **host network** (it must
test the node's own path, the one the kubelet pulls images over), and the mirror
CA installer (`registry-ca-trust`, only with a private-CA mirror) writes the CA into
each node's `/etc/containers/certs.d` and `/etc/docker/certs.d` through a
**hostPath** mount, as a privileged container. Every check container itself still
runs non-root with all capabilities dropped, a read-only root filesystem and no
privilege escalation.

These objects are created out of band, not by Argo CD, so they outlive the
Application for the PostDelete check; `uninstall` removes them at the end.

### FLO

The rendered manifests bind FLO's ServiceAccount `flo-f5-lifecycle-operator` in
`f5-bnk` to the privileged SCC
(`system:openshift:scc:privileged:f5-bnk:flo-f5-lifecycle-operator`), named as
roksbnkctl names it so the two tools recognise each other's objects. This binding
is in Git and is synced (wave −6).

### Argo CD's access to the cluster

`install` registers ROKS with your Argo CD using:

| Object | Name |
|---|---|
| ServiceAccount | `kube-system/roksbnkargoctl-argocd-manager` |
| ClusterRoleBinding | `roksbnkargoctl-argocd-manager` → `cluster-admin` |
| Token Secret | `kube-system/roksbnkargoctl-argocd-manager-token` (a long-lived ServiceAccount token) |

`cluster-admin` is required because the Application installs CRDs, cluster roles
and SCC bindings. The token is sent to Argo CD's cluster API; for the private
endpoint, the cluster's CA is sent with it (the private endpoint does not chain to
public roots). `uninstall` deletes the three objects and the Argo CD cluster entry.
Treat the hub's cluster Secret as cluster-admin on ROKS.

## IBM Cloud IAM

`install` creates one IAM **trusted profile**, used by BNK's CNE controller to
program VPC routes:

| Property | Value |
|---|---|
| Name | `<cluster name>-f5-cne-controller-<bnk.namespace>` |
| Link | ROKS ServiceAccount `<bnk.namespace>/f5-cne-controller` in this cluster only (link name `f5-cne-controller-roks-link`) |
| Policy 1 | roles Viewer and Editor on service `is` (VPC Infrastructure Services), scoped to the cluster's VPC ID |
| Policy 2 | role Viewer on service `containers-kubernetes`, scoped to the cluster |

The profile ID is passed to the CNE controller as `CLOUD_TRUSTED_PROFILE`; no API
key is placed in the cluster. `uninstall` deletes the profile unless
`--keep-trusted-profile`.

The API key you run the tool with needs the permissions to do what the commands
do: read the cluster and its admin kubeconfig, attach VPCs to the transit gateway,
manage IAM trusted profiles and policies, read COS, and — for `flp` and `argocd` —
create VPC resources.

## Verifying the other side

### Git over SSH

Every SSH host key is verified.

- The keys of **github.com, gitlab.com and bitbucket.org** are built in. Each was
  verified against the provider's own published list:

  | Host | Verified against |
  |---|---|
  | github.com | `api.github.com/meta` `ssh_keys`, fingerprints matching its `ssh_key_fingerprints` |
  | gitlab.com | `ssh-keyscan`, fingerprints matching GitLab's documentation (`docs.gitlab.com/user/gitlab_com`) |
  | bitbucket.org | Atlassian's published list at `https://bitbucket.org/site/ssh` |

- **`git.known_hosts_file`** adds keys for any other server. It never replaces the
  built-in keys. `install` also adds it to Argo CD (`POST
  /api/v1/certificates?upsert=true`, one entry per hostname, the shape `argocd cert
  add-ssh` sends), because Argo CD clones the repository too and must verify the
  same host. Entries with hashed hostnames cannot be added that way; `install` names
  them for you to add by hand.
- An **unknown host** and a **changed key** are different errors. The first tells
  you to set `git.known_hosts_file`. The second — `SSH HOST KEY MISMATCH … refusing
  to connect (possible man-in-the-middle …)` — refuses outright.
- **`ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1`** switches verification off for the
  tool's own Git operations (`install`, `uninstall --purge-git`). It exists as an
  explicit escape hatch and `install` warns every time it is set: *the Git server's
  SSH host key is NOT verified — anyone on the path can impersonate it. Use
  git.known_hosts_file instead.* It does not change how Argo CD verifies the
  repository.

### Argo CD, IBM Cloud, registries

- The Argo CD API is reached over TLS; `argocd.ca_file` supplies a private CA, and
  `argocd.insecure` turns verification off for a self-signed hub.
- The node probe verifies registry and licensing certificates against the image's
  CA bundle, reporting the presented subject even when verification fails. A
  private mirror CA is trusted by setting `registry.mirror.ca_file`; the FLP, which
  is self-signed with a CA the tool generated, is probed without chain verification
  and trusted by CWC through `licenseserver-rootca`.

## Supply chain of the check image

| Property | Detail |
|---|---|
| Build | static Go binary, standard library only (CI enforces it), on `scratch`, user `65532` |
| Pinning | rendered by digest (`…@sha256:…`), resolved from upstream; in mirror mode the upstream digest must be present in the mirror |
| Runtime | non-root, `RuntimeDefault` seccomp, read-only root filesystem, all capabilities dropped |
| CI | `go vet`, `staticcheck`, race-enabled tests; security workflow with `govulncheck` (current Go and the `go.mod` floor), `gitleaks`, CodeQL and Trivy |

A digest pin means what runs is exactly what was rendered and reviewed. With a
mutable tag and `IfNotPresent`, a node keeps running whatever it cached first, and a
re-sync after a check fix would silently run the old check.

## Files on the operator host

Everything the tool writes into a workspace is owner-only: files mode `0600`,
directories `0700`. That covers `config.yaml`, `manifests/`, `diagnostics/`,
`flp-outputs.json`, `argocd-hub.json`, the agent scaffold, and the `current`
workspace pointer. `config.yaml` and the output records are written atomically
(temporary file and rename), so a crash never leaves a half-written file. On
Windows, POSIX modes do not apply; protect the workspace home with the directory's
own access control.

## See also

- [The Argo CD Application: what is in Git, and what is not](./07-the-application.md)
- [Checks reference](./16-checks.md)
- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Appendix E: design decisions and live findings](./appendix-e-design.md)
