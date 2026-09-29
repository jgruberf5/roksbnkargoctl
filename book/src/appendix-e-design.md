# E. Design decisions and live findings

This appendix records why roksbnkargoctl is built the way it is: the decisions that
shape every install, the defects the first live runs found and the rule each one
left behind, and the roksbnkctl issues whose lessons were carried over. Read it when
you want to know whether a behaviour is deliberate before you work around it — in
almost every case below, the obvious alternative was tried, or was known to fail.

## Constraints

The design follows from what the customer asked for:

| # | Constraint | Consequence |
|---|---|---|
| C1 | Argo CD YAML is the only install mechanism | every in-cluster BNK object is plain YAML in Git, synced by Argo CD |
| C2 | the cluster and transit gateway already exist | taken by name or ID; never created or deleted by `install` |
| C3 | Argo CD already exists, outside ROKS | ROKS is registered with the hub as a cluster; the hub never runs the tool's containers |
| C4 | IBM Cloud APIs, not Terraform | Go REST/SDK calls only; no `terraform`, `helm`, `kubectl`, `ibmcloud` or `git` binaries |
| C5 | the API key comes from an environment variable | `IBMCLOUD_API_KEY` (or `IC_API_KEY`); never written to disk |
| C6 | connected and disconnected modes | `bnk.mode: connected \| disconnected` |
| C7 | FAR or a replicated private registry | `registry.source: far \| mirror` |
| C8 | prerequisite checks are containers in ROKS, deployed by YAML | the `check` image, run by Argo CD hooks in ROKS |
| C9 | day-0 install and uninstall only | no traffic tests, gateway phase or upgrades |
| C10 | BNK 2.4 GA only | manifest `2.4.0` (FAR tags `2.4.0` and `2.4.0-3.3175.0-0.0.380` are the same artifact) |

## Design decisions

### Git is Argo CD's only source

The Application has one source: a path in your Git repository. Argo CD must not
need access to FAR, a Helm repository or a config-management plugin. That keeps the
hub's configuration yours, makes `manifests/git/` the complete, reviewable record
of what is installed, and means an air-gapped hub works the same as a connected
one. The consequences are the next two decisions.

### Helm charts are rendered offline, on the operator host

`render` pulls cert-manager `v1.17.3` and FLO `f5-lifecycle-operator` from FAR (or
the mirror) as OCI artifacts and templates them in-process with the Helm Go SDK in
client-only mode — no `helm` binary. Details that follow from rendering offline:

- FLO's chart `lookup`s cert-manager's CRDs and fails when it cannot see them.
  Offline, `lookup` is always empty, so the render sets `skipCertMgr: true`; that
  skips only the chart's pre-check.
- Helm hooks keep their `helm.sh/hook` annotations; Argo CD maps them. cert-manager's
  `startupapicheck` hook is disabled — `cert-manager-ready` does that job better.
- Every object gets an `argocd.argoproj.io/sync-wave`, and the render is sorted and
  deterministic: re-rendering an unchanged config produces identical files, and
  re-publishing commits nothing.
- The render's **run id** is a hash of the config (without the `resolved`
  bookkeeping), the manifest version, the check image digest, the FLP URL, the mirror
  CA and the node image. Any change re-rolls the node probes; no change leaves them
  alone.

### Secrets go direct, never through Git

Every rendered Secret, whichever chart produced it, is moved to the out-of-band set
that `install` writes into ROKS, and the render refuses to publish if the registry
password or the JWT appears in any Git object. See
[Appendix D](./appendix-d-security.md).

### The License is built by a hook

BNK 2.4's License CRD requires the subscription JWT **inline** (`spec.jwt`), with no
Secret reference, so a License in Git would publish the JWT. Its CRD is also
installed at runtime by FLO's crd-installer, not by the chart, so a synced License
would need `SkipDryRunOnMissingResource`. Building it in the `check license` hook,
from a Secret `install` wrote, solves both, and gives the sync a real health gate:
the hook waits for the License to be Active and the CNEInstance Available.

### Every check runs in ROKS, as part of the Application

The checks are hook Jobs, a DaemonSet and a Deployment in the Application itself.
They run with the cluster's own view: the node probe runs on each worker's host
network, which is the path the kubelet pulls images over. A check run on the
operator host would report a confident green for a mirror the workers cannot route
to. The check namespace, ServiceAccount and RBAC are created out of band so they
outlive the Application for the PostDelete check.

### The gateway API sweep is a Deployment, not a hook

The CRDs it waits for arrive with FLO in the next wave, and Argo CD does not start a
wave until the previous wave's hooks finish. A blocking hook would deadlock the
sync; a Deployment is healthy as soon as it runs.

### `Delete=false` on namespaces and CRDs

- **BNK namespaces** carry `argocd.argoproj.io/sync-options: Delete=false`. A
  namespace stuck on an F5 finalizer would otherwise hang the Application's
  deletion before the PostDelete check ran. `check post-uninstall` deletes them
  instead, handling finalizers.
- **CRDs** carry `Delete=false`. Deleting a CRD deletes every CR of that kind, and
  F5 CRs whose finalizer's controller is gone hang their namespace. F5 CRDs stay
  after uninstall, as they do with roksbnkctl.

### Application settings

| Setting | Why |
|---|---|
| manual sync, no automatic retry (`retry.limit: 0`) | the sync is the operator's approval |
| `ServerSideApply=true` | FLO's CRDs exceed the client-side apply annotation limit |
| `PruneLast=true` (with `RespectIgnoreDifferences=true`) | prune only after everything else has applied |
| finalizer `resources-finalizer.argocd.argoproj.io` | foreground-cascading delete: without it Argo CD deletes the Application and orphans BNK, skipping the uninstall checks |
| Argo CD ≥ 3.3 enforced | PreDelete hooks, which the drain depends on |
| no custom health (Lua) required | the hub's `argocd-cm` is yours; the license hook is the real gate instead |

### The check image is pinned by digest

With a mutable tag and `imagePullPolicy: IfNotPresent`, a node keeps running
whatever it cached first, so a re-sync after a check fix would run the old check.
The render resolves the tag to a digest, upstream; in mirror mode that digest must
exist in the mirror.

### `deploymentSize: Tiny` only

Every larger size requests hugepages that ROKS workers cannot allocate. Tiny is a
render literal with no config key, and `check post-install` fails anything else. See
[Appendix C](./appendix-c-sizing.md).

### The F5 License Proxy version is pinned

The BNK 2.4.0 GA manifest no longer lists the license proxy (2.3.x did), so its
version cannot be derived from the manifest. `flp up` pins f5-license-proxy
`1.29.0-0.10.40`, the newest tag on FAR when 2.4.0 GA shipped (roksbnkctl #327).

## Findings from the live runs

The tool was proven on a ROKS cluster running OpenShift 4.21.31 with Argo CD 3.5.1:
connected, disconnected (FLP) and mirror (Artifactory, 93 artifacts) installs and
uninstalls, re-sync, and reinstall on a used cluster, with 3 TMM replicas on VPC
block storage. An install takes roughly 8–10 minutes of sync. Each finding below
changed the code, and each has a regression test that fails against the old
behaviour.

| Finding | Rule it produced |
|---|---|
| The ROKS **private** endpoint's certificate chains to the cluster's own root CA (the same CA as a ServiceAccount token Secret's `ca.crt`); the public endpoint chains to public roots | the cluster CA is sent to Argo CD only for the private endpoint; for the public one it would replace the system roots and fail |
| A Namespace server-side-applied with `annotations: {}` never receives OpenShift's `openshift.io/sa.scc.*` annotations, and every pod in it is rejected (`unable to find annotation openshift.io/sa.scc.uid-range`) | empty metadata maps are pruned in the renderer and before every apply |
| A re-sync after a check fix ran the unfixed check: the `:dev` tag was already cached on every node | render the check image by digest |
| Pinning `:dev` against the mirror faithfully pinned a copy replicated before a check fix | resolve the digest upstream; require it in the mirror, else stop and say `registry replicate` |
| A `git describe` build version names no published image, so every check pod would `ImagePullBackOff` | only an exact `vX.Y.Z` release tag is used; anything else uses `:dev` |
| Argo CD 3.5.1 answered the Application DELETE with `415 Invalid content type` (a body-less request without `Content-Type`) | every non-GET request declares JSON |
| IBM VPC paging drops the required `version` parameter from `next.href`, so a second page answered 400 | the parameters are carried onto every page |
| A reinstall failed at the issuer wave: `failed calling webhook webhook.cert-manager.io … x509: certificate signed by unknown authority` — the cainjector lagged cert-manager's Deployment health | the `cert-manager-ready` Sync hook (wave −11) dry-runs a ClusterIssuer until the webhook admits it |
| `uninstall` deleted the check namespace, and with it the uninstall checks' logs; Argo CD deletes the PreDelete pod with the Application | check logs are collected every few seconds **while** the Application is deleted, into `diagnostics/uninstall-<time>/` |
| An uninstall printed `uninstalled` and left `f5-bnk`, `f5-utils` and `cert-manager`: Argo CD 3.5.1 stopped the PreDelete pod 0.4 s after it started and never ran PostDelete ([argoproj/argo-cd#29100](https://github.com/argoproj/argo-cd/issues/29100)) | `uninstall` runs both checks as its own Jobs around the delete and fails unless the BNK namespaces are gone; `install` keeps Argo CD's delete-hook finalizers when it updates the Application |
| `registry replicate` pushed to Artifactory anonymously (`Authentication is required`, 93 of 93 failed): mirror credentials were applied only once `registry.source` was already `mirror` | mirror credentials and CA apply whenever a mirror is configured, because the mirror is filled before the install switches to it |
| One artifact is listed with a `+` in its version, which is illegal in an OCI tag; FAR stores it with `_` | the bill of materials maps `+` to `_`; the mirror holds all 93 artifacts |
| pre-install failed `tmm_replicas > 1` on block storage, believing the replicas share one ReadWriteOnce `tmm-pvc` | removed: at Tiny TMM mounts no PVC (roksbnkctl #197); now an informational note |
| Tiny was implicit | made explicit: a render test pins it, config parsing rejects a size key, post-install fails anything else |
| Every live test over SSH switched host-key checking off, because there was no way to configure a host | built-in verified keys for github.com, gitlab.com and bitbucket.org; `git.known_hosts_file` for others (also given to Argo CD); unknown host and changed key reported differently; the insecure override warns every time |
| The 2.4.0 GA manifest no longer lists the license proxy | its version is pinned (roksbnkctl #327) |

## Lessons carried over from roksbnkctl

roksbnkargoctl is the slim successor to roksbnkctl, the Terraform-based installer.
Several of its checks encode roksbnkctl issues directly:

| roksbnkctl issue | What it found | How roksbnkargoctl applies it |
|---|---|---|
| #197 | filed as "TMM replicas share one ReadWriteOnce volume"; resolved by live measurement: at `deploymentSize: Tiny` TMM mounts no PVC (3 and 9 replicas ran on block storage, one per node) | no ReadWriteMany requirement for TMM replicas |
| #203 | every size above Tiny requests hugepages; ROKS workers have none and no Machine Config Operator to allocate them | Tiny only, enforced three ways |
| #217 | deleting FLO before the CNEInstance's CRs finish finalizing leaves `f5-bnk` stuck Terminating | `uninstall` runs `check pre-uninstall` before it deletes the Application, so FLO is still running, and the check deletes the CNEInstance last |
| #327 | the 2.4.0 manifest no longer lists `charts/f5-license-proxy` | the FLP version is pinned |
| #57 | a probe 73 s after a transit gateway attach failed on a path healthy minutes later; old verdicts looked sticky | the node probe retries for 180 s; pre-install restarts probe pods and trusts only fresh verdicts |
| #90 | the License apply races the ResourceQuota controller on every fresh install | the license hook retries refused applies for 5 minutes |
| #172 | teardown left license secrets and CRD groups behind, which broke the next install | post-uninstall deletes the 34 CWC secrets by name; pre-install refuses an install over any leftover |
| #241 | FLO re-creates its validating webhook within a second during teardown, so deletes are refused | the webhook is swept on a timer and on every refused delete |
| #266 | draining every F5 group fights FLO and the product webhook | only `gateway.k8s.f5.com` and `fic.f5.com` are drained before the CNEInstance |

## Not in scope

Cluster or transit gateway creation, BNK upgrades (BNK has no in-place upgrade: a
version or mode change is uninstall and install), the gateway phase (Infra,
GatewaySettings, Gateway), traffic tests, and BNK 2.3.

## See also

- [What roksbnkargoctl does (and doesn't)](./01-what-it-does.md)
- [Checks reference](./16-checks.md)
- [Appendix C: ROKS sizing](./appendix-c-sizing.md)
- [Appendix D: security model](./appendix-d-security.md)
