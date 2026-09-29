# The Argo CD Application: what is in Git, and what is not

This chapter describes the single Argo CD Application that roksbnkargoctl creates and every
object it syncs. After reading it you will be able to review `manifests/git/` before an
install, predict the order in which Argo CD applies and deletes BNK, and explain why some
objects are written straight into ROKS instead of going through Git.

## Three outputs from one render

`roksbnkargoctl render` (and step 4 of [install](./08-install.md)) splits everything the
install consists of three ways and writes it into the workspace:

| Output | Path in the workspace | Who applies it |
|---|---|---|
| Git objects | `manifests/git/NNN-<kind>-<ns>-<name>.yaml` | Argo CD, after `install` publishes the directory to your repo |
| Direct objects | `manifests/direct/NNN-<kind>-<ns>-<name>.yaml` | `install`, by server-side apply into ROKS (field manager `roksbnkargoctl`) |
| The Application | `manifests/application.yaml` | `install`, through the Argo CD API |

The `manifests/` directory is replaced wholesale on every render, so an object that is no
longer rendered does not linger. Files are numbered in sync order (wave, then a kind order
that puts namespaces, CRDs, ServiceAccounts, Secrets and ConfigMaps first, then namespace
and name), which makes the render deterministic: an unchanged configuration produces
byte-identical files and publishing it commits nothing.

`render` changes nothing in the cluster, Argo CD or Git. Review `manifests/git/` before
your first install: it is the whole of what your repository will contain.

## The Application

`install` creates or updates the Application with
`POST /api/v1/applications?upsert=true&validate=true`. Its shape:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: bnk-<workspace>              # argocd.application
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
  labels:
    app.kubernetes.io/managed-by: roksbnkargoctl
    roksbnkargoctl.io/workspace: <workspace>
spec:
  project: default                   # argocd.project
  source:
    repoURL: <git.url>
    targetRevision: main             # git.branch
    path: bnk/<workspace>            # git.path
    directory:
      recurse: true
  destination:
    server: <ROKS API endpoint>      # resolved.argocd_cluster_server
    namespace: f5-bnk                # bnk.namespace
  syncPolicy:
    syncOptions:
      - ServerSideApply=true
      - RespectIgnoreDifferences=true
      - PruneLast=true
    retry:
      limit: 0
```

| Field | Value and reason |
|---|---|
| `metadata.name` | `argocd.application`, default `bnk-<workspace>` |
| `spec.project` | `argocd.project`, default `default` |
| `spec.source` | Your repository, `git.branch` (default `main`), `git.path` (default `bnk/<workspace>`), plain directory source with `recurse: true`. No Helm, Kustomize or plugin source: the hub needs no access to FAR, a Helm repository or a plugin |
| `spec.destination.server` | The ROKS API endpoint chosen by `argocd.cluster_endpoint`: the cluster's **private** service endpoint (default, reached over the transit gateway) or its public one |
| `spec.destination.namespace` | `bnk.namespace` (default `f5-bnk`). Chart objects rendered without a namespace are given their release namespace at render time, so nothing lands here by accident |
| `ServerSideApply=true` | FLO's CRDs exceed the size limit of client-side apply's last-applied annotation |
| `RespectIgnoreDifferences=true` | Honors any `ignoreDifferences` you add to the Application |
| `PruneLast=true` | Pruning happens after the rest of a sync |
| No `automated` block | Sync is manual. The sync is the operator's approval: `install` triggers it, or you trigger it from the Argo CD UI after `install --no-sync` |
| `retry.limit: 0` | A failed sync is not retried by Argo CD. Fix the cause and re-run `install` |
| Finalizer `resources-finalizer.argocd.argoproj.io` | Deleting the Application cascades. Without it Argo CD would delete the Application, orphan BNK, and skip the uninstall checks |

![Argo CD Applications list showing bnk-bnkargo](images/argocd/applications-list.png)

*The Application in Argo CD's Applications list: one Application per cluster, in project `default`, sourced from the Git path `install` published.*

![Resource tree of a healthy bnk-bnkargo Application](images/argocd/application-tree-healthy.png)

*The resource tree after a successful sync: Healthy and Synced, with every resource Synced and none OutOfSync. Pods that share a parent are drawn as one group node (here `Healthy 3 pods` under the node-probe DaemonSet); Argo CD switches to this view above 15 pods.*

### How the destination cluster is registered

Argo CD reaches ROKS with a ServiceAccount token that `install` mints on the cluster:

| Object (on ROKS) | Name |
|---|---|
| ServiceAccount | `kube-system/roksbnkargoctl-argocd-manager` |
| ClusterRoleBinding to `cluster-admin` | `roksbnkargoctl-argocd-manager` |
| Long-lived token Secret (`kubernetes.io/service-account-token`) | `kube-system/roksbnkargoctl-argocd-manager-token` |

The binding is `cluster-admin` because the Application installs CRDs, cluster roles and
SCC bindings. `install` then registers the cluster with `POST /api/v1/clusters?upsert=true`:
name = the ROKS cluster name, server = the chosen endpoint, bearer token = the token above,
label `app.kubernetes.io/managed-by: roksbnkargoctl`.

The CA sent with the registration depends on the endpoint. The ROKS private endpoint's
certificate chains to the cluster's own root CA (the `ca.crt` of the token Secret), so that
CA is sent. The public endpoint presents a publicly trusted certificate, so no CA is sent: a
cluster CA would replace Argo CD's system roots and the connection would fail with
`x509: certificate signed by unknown authority`.

## Sync waves and every object

Every Git object carries `argocd.argoproj.io/sync-wave`. Argo CD applies waves in ascending
order and does not start a wave until the previous one is healthy and its hooks have
finished. Names below use the default namespaces (`bnk.namespace: f5-bnk`,
`bnk.utils_namespace: f5-utils`).

| Wave | Kind | Name | Notes |
|---|---|---|---|
| −20 | Namespace | `f5-bnk`, `f5-utils`, `cert-manager` | `cert-manager` only when `bnk.cert_manager.install` is true (the default). All carry `Delete=false` |
| −19 | ConfigMap | `roksbnkargoctl-check/registry-ca` | Only when `registry.mirror.host` and `registry.mirror.ca_file` are set |
| −19 | DaemonSet | `roksbnkargoctl-check/registry-ca-trust` | Same condition. Installs the mirror CA on every node; see [registry](./13-registry.md) |
| −18 | DaemonSet | `roksbnkargoctl-check/check-node-probe` | `check node-probe` on every node, host network |
| −18 | Job (Sync hook) | `roksbnkargoctl-check/check-pre-install` | Cluster prerequisites and the node-probe verdicts |
| −12 | cert-manager chart | every object of the Jetstack chart `v1.17.3`, including its CRDs | Only when cert-manager is installed by roksbnkargoctl. `startupapicheck` is disabled |
| −11 | Job (Sync hook) | `roksbnkargoctl-check/check-cert-manager-ready` | Waits until cert-manager's webhook admits a dry-run `ClusterIssuer` |
| −10 | ClusterIssuer | `selfsigned-cluster-issuer` | |
| −9 | Certificate | `cert-manager/ext-ca` | Self-signed CA, ECDSA P-256 |
| −8 | ClusterIssuer | `sample-issuer` | CA issuer backed by `ext-ca`; the issuer FLO and the CNEInstance use |
| −6 | NetworkAttachmentDefinition | `f5-bnk/ens3-ipvlan-l2` | ipvlan L2 on `ens3`, static address `bnk.nad_address` (default `10.10.1.1/24`) |
| −6 | ClusterRoleBinding | `system:openshift:scc:privileged:f5-bnk:flo-f5-lifecycle-operator` | Grants FLO's ServiceAccount the `privileged` SCC |
| −6 | Deployment | `roksbnkargoctl-check/check-gateway-api-sweep` | Removes OpenShift's Gateway API CRD admission policy until the F5 Gateway API CRDs exist |
| −5 | FLO chart | every object of `f5-lifecycle-operator` (release `flo`, namespace `f5-bnk`), including its `k8s.f5.com` CRDs | The chart version is the one the BNK 2.4.0 manifest lists (`v2.30.0-0.5.2`) |
| −4 | CNEManifest | `bnk-2.4.0` | Cluster-scoped. `SkipDryRunOnMissingResource=true` (its CRD arrives in wave −5 of the same sync) |
| −2 | CNEInstance | `f5-bnk/f5-bnk-f5-cne-controller` | `deploymentSize: Tiny`, `tmmReplicas` from `bnk.tmm_replicas`. `SkipDryRunOnMissingResource=true` |
| 0 | Job (Sync hook) | `roksbnkargoctl-check/check-license` | Builds the `License`, waits for `Active`, then for `CNEInstance` `Available` |
| PostSync | Job (hook) | `roksbnkargoctl-check/check-post-install` | Verifies the finished install |
| PreDelete | Job (hook) | `roksbnkargoctl-check/check-pre-uninstall` | Drains F5 resources while FLO still runs; CNEInstance last |
| PostDelete | Job (hook) | `roksbnkargoctl-check/check-post-uninstall` | License secrets, namespaces, stuck F5 finalizers |

A render prints the same information as a per-wave summary, for example:

```text
   -20  Namespace×3
   -18  DaemonSet, hook:check-pre-install
   ...
 hooks  PostSync:check-post-install, PreDelete:check-pre-uninstall, PostDelete:check-post-uninstall
```

![Sync status panel listing resources by sync wave](images/argocd/last-sync-waves.png)

*The last sync's result, one row per object with its sync wave, hook and message. The check hooks show as Jobs with their hook type (here the `Sync` hook `check-pre-install`).*

![List view of the Application's resources](images/argocd/application-resource-list.png)

*The list view: every object the Application manages, with its sync and health status.*

### Why the Gateway API sweep is a Deployment

The sweep waits for CRDs that FLO installs in the *next* wave (−5). Argo CD will not start
wave −5 until wave −6's hooks finish, so a blocking hook would deadlock. A Deployment is
healthy as soon as its pod runs, which lets the sync continue while the sweep keeps
working.

### The hooks

Every check runs from the same `check` image. Each hook Job has `backoffLimit: 0`, an
`activeDeadlineSeconds` a little longer than its own `--timeout`, and
`argocd.argoproj.io/hook-delete-policy: BeforeHookCreation`: a failed Job and its logs stay
until the next sync creates the hook again, which is when you need them.

| Job | Argo CD hook | Wave | `--timeout` | Job deadline |
|---|---|---|---|---|
| `check-pre-install` | Sync | −18 | 15m | 20m |
| `check-cert-manager-ready` | Sync | −11 | 10m | 12m |
| `check-license` | Sync | 0 | 35m (License `Active`); 15m more for `CNEInstance` `Available` | 40m |
| `check-post-install` | PostSync | — | 10m | 15m |
| `check-pre-uninstall` | PreDelete | — | 15m | 20m |
| `check-post-uninstall` | PostDelete | — | 15m | 20m |

PreDelete hooks exist only in Argo CD 3.3 and later, which is why `install` refuses an older
Argo CD. `uninstall` does not rely on the two delete hooks alone: it runs the same checks as
its own Jobs, before and after the delete (see [uninstall](./10-uninstall.md#the-sequence)). What each check verifies is in the [checks reference](./16-checks.md).

Charts' own Helm hooks keep their `helm.sh/hook` annotations; Argo CD maps them to its own
hook phases. Their sync wave is the chart's wave (−12 or −5).

### The check image is pinned by digest

Every check pod references the image as `repo@sha256:…`, resolved at render time. With a
mutable tag and `imagePullPolicy: IfNotPresent`, nodes keep running whatever they cached,
so a re-sync after a check fix would run the old check. A new image digest also changes
the render, which rolls the node probes. See [install](./08-install.md#the-check-image).

## What is not in Git, and why

Four kinds of object are written straight into ROKS by `install` and never reach your
repository.

| Object | Where | Why it is not in Git |
|---|---|---|
| Pull secret `far-secret` (FAR) or `mirror-secret` (mirror), type `kubernetes.io/dockerconfigjson` | `f5-bnk`, `f5-utils`; in mirror mode also `roksbnkargoctl-check` and (when installed by roksbnkargoctl) `cert-manager` | A registry credential. Not created in mirror mode when `registry.mirror.username` is empty |
| Secret `bnk-license-jwt` (key `jwt`) | `roksbnkargoctl-check` | The subscription JWT |
| Secret `licenseserver-rootca` (key `licenseserver-rootca.txt`) | `f5-utils` | The FLP root CA; disconnected mode only |
| Any `kind: Secret` a chart renders (FLO renders `external-otelsvr-secret`) | the chart's namespace | Every rendered Secret is moved out of Git, whoever rendered it |
| Namespace `roksbnkargoctl-check`, ServiceAccount `check`, ClusterRole and ClusterRoleBinding `roksbnkargoctl-check`, ClusterRoleBinding `system:openshift:scc:privileged:roksbnkargoctl-check:check` | cluster / `roksbnkargoctl-check` | Must outlive the Application: the PostDelete hook runs after Argo CD has deleted everything it owns |
| Namespaces `f5-bnk`, `f5-utils`, `cert-manager` (again) | cluster | Created before the Secrets that live in them. Argo CD adopts them when it syncs wave −20 |

Before publishing, the renderer scans every Git object for the registry password and the
subscription JWT and refuses to publish if either appears:

```text
render: refusing to publish: the subscription JWT appears in <Kind> <ns>/<name>
```

![CNEInstance summary panel in Argo CD](images/argocd/cneinstance-summary.png)

*The CNEInstance `f5-bnk-f5-cne-controller` as Argo CD shows it: the live manifest carries FLO's finalizer and the sync wave, and the spec is the Tiny, IBM-specific one the renderer writes.*

### The License is built by a hook

In BNK 2.4 `License.spec.jwt` is required and inline; the CRD has no Secret reference. A
`License` in Git would therefore put the JWT in Git. Instead the `check license` hook (wave
0) reads Secret `roksbnkargoctl-check/bnk-license-jwt` and server-side-applies
`License f5-utils/bnk-license` itself:

| `License.spec` field | connected | disconnected |
|---|---|---|
| `jwt` | from the Secret | from the Secret |
| `operationMode` | `connected` | `f5licenseproxy` |
| `teemCertUrl`, `teemEntitlementUrl`, `teemInitialConfigUrl` | not set | `https://<flp-ip>:8443/license-proxy/v1` |
| `licenseProxyServerRootCaPath` | not set | `/etc/cm20/licenseserver-rootca/licenseserver-rootca.txt` |

Building it in a hook also solves an ordering problem: the `License` CRD is installed at
runtime by FLO, not by its chart, so a `License` in Git could not pass Argo CD's dry run.

### The check namespace and RBAC must outlive the Application

The PostDelete hook `check-post-uninstall` runs after Argo CD has pruned every object it
owns. If its namespace, ServiceAccount or RBAC were in Git they would already be gone. They
are created by `install` and removed by `uninstall` only after the Application has been
deleted. The `check` ClusterRole reads core, apps, batch and storage resources, can patch
Deployments, DaemonSets and StatefulSets (to roll CWC), can patch and delete pods, Secrets,
namespaces and a few other core resources, can delete
validating admission policies and webhooks, can create (dry-run) ClusterIssuers, and has
full access to the F5 API groups (`k8s.f5.com`, `k8s.f5net.com`, `gateway.k8s.f5.com`,
`fic.f5.com`, `metrics.f5.com`) because the uninstall checks delete whatever F5 resources
exist. The privileged SCC binding is for the node probe's host network and the CA
installer's `hostPath`.

## `Delete=false`: what Argo CD must not delete

Two groups of Git objects carry `argocd.argoproj.io/sync-options: Delete=false`, so Argo CD
leaves them in place when the Application is deleted:

| Objects | Why |
|---|---|
| Namespaces `f5-bnk`, `f5-utils`, `cert-manager` | A namespace stuck on an F5 finalizer would hang the Application's deletion before the PostDelete check ever ran. `check post-uninstall` deletes them itself and strips F5 finalizers if they stick |
| Every `CustomResourceDefinition` in Git (FLO's, and cert-manager's) | Deleting a CRD deletes every CR of that kind; F5 CRs whose controller is already gone hang their namespace |

## How the Helm charts are rendered

The hub must not need FAR, a Helm repository or a plugin, so all templating happens on your
host. No `helm` binary is used.

| Chart | Pulled from | Release / namespace |
|---|---|---|
| cert-manager `v1.17.3` (`bnk.cert_manager.version`) | `oci://quay.io/jetstack/charts/cert-manager`, or the mirror's copy | `cert-manager` / `cert-manager` |
| `f5-lifecycle-operator` (the version the BNK manifest lists) | `oci://<FAR or mirror>/charts/f5-lifecycle-operator` | `flo` / `bnk.namespace` |

The chart archives are pulled over OCI in-process and templated with the Helm Go SDK in
client-only dry-run mode: CRDs included, OpenAPI validation off, the cluster's Kubernetes
version (read from ROKS; `v1.34.0` with `render --no-cluster`), and the API versions
`config.openshift.io/v1`, `security.openshift.io/v1`, `route.openshift.io/v1` and
`k8s.cni.cncf.io/v1` so that charts detect OpenShift (FLO grants SCC access only when
`config.openshift.io/v1` exists).

Offline, Helm's `lookup` always returns nothing. FLO's chart looks up cert-manager's CRDs
and fails when it cannot see them, so the render sets `skipCertMgr: true`. That skips only
the chart's pre-check; cert-manager is still installed (wave −12) and gated (wave −11)
before FLO.

Values that matter:

| Chart | Values set |
|---|---|
| cert-manager | `crds.enabled: true`, `crds.keep: true`, `startupapicheck.enabled: false`; in mirror mode every component image (`controller`, `webhook`, `cainjector`, `acmesolver`) is redirected to `<mirror>/jetstack/cert-manager-<component>` and the pull secret is added |
| FLO | `skipCertMgr: true`, `containerPlatform: IBM`, `namespace` and `sharedComponentNamespace` from the BNK namespaces, `global.certmgr.clusterIssuer: sample-issuer`, image repository `<FAR or mirror>/images` with `pullPolicy: Always`, the pull secret when there is one |

## The redacted copy under `manifests/direct`

`manifests/direct/` holds every direct object as `install` applies it, except that Secret
values are replaced (`data` values by the base64 of `REDACTED`, `stringData` values by
`REDACTED`) and each Secret gains the annotation `roksbnkargoctl.io/redacted`. The real
Secrets exist only in ROKS. `uninstall` reads this directory to know what to delete, which
needs only kind, namespace and name; keep it in the workspace until you have uninstalled.

![Network view of the Application](images/argocd/application-network.png)

*The network view: the Application's pods, among them FLO and the check pods. The Sync hooks' pods show `completed`, the node probes and the Gateway API sweep `running`.*

## See also

- [How an install flows](./02-how-an-install-flows.md)
- [install](./08-install.md) and [uninstall](./10-uninstall.md)
- [Checks reference](./16-checks.md)
- [Appendix D. Security model](./appendix-d-security.md)
