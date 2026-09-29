# How an install flows

This chapter follows one install from the first command to the last, and then back out
again. After reading it you will know which system each step talks to, what lands where,
the order Argo CD applies BNK in, and why that order is what it is.

## The five commands

```sh
roksbnkargoctl init        # gather the details, resolve them against IBM Cloud
roksbnkargoctl render      # write every manifest into the workspace (changes nothing)
roksbnkargoctl install     # IBM + ROKS prerequisites, Git, Application, sync
roksbnkargoctl status      # Application, check and BNK state
roksbnkargoctl uninstall   # delete the Application; uninstall checks run in ROKS
```

## Who talks to whom

```text
 operator host (roksbnkargoctl, one Go binary; no terraform/helm/kubectl/git)
 |
 |-- IBM Cloud APIs ------------ IAM (trusted profile + policies), Kubernetes Service
 |                               (cluster lookup, admin kubeconfig), VPC (read),
 |                               Transit Gateway (attach VPC), COS (FAR key, JWT)
 |
 |-- FAR or mirror, ghcr.io ---- render: BNK manifest, FLO + cert-manager charts,
 |                               check image digest
 |
 |-- ROKS API (public endpoint)- install: Secrets, check namespace + RBAC,
 |                               ServiceAccount Argo CD manages ROKS with
 |
 |-- Git (HTTPS or SSH) -------- install: commit manifests/git to <branch>:<path>
 |
 '-- Argo CD API --------------- install: cluster, repository, Application, sync
                                  |
                                  v
   +--------------------------------+          +-----------------------------------+
   | Argo CD hub (yours, external)  |  clone   |  Git repository                   |
   |  - Application bnk-<workspace> |--------->|  <path>/  plain YAML, no secrets  |
   |  - no BNK or check pods here   |          +-----------------------------------+
   +--------------------------------+
                 |  ROKS private service endpoint, over the transit gateway
                 |  (bearer token + the cluster's own CA)
                 v
   +-----------------------------------------------------------------------------+
   | ROKS cluster (OpenShift, 3 zones)                                           |
   |  roksbnkargoctl-check : check hooks, check-node-probe DaemonSet, sweep      |
   |  cert-manager         : cert-manager                                        |
   |  f5-bnk / f5-utils    : FLO, CNEManifest, CNEInstance, License, TMM, ...    |
   |      nodes pull from FAR or the mirror; license with F5 or through the FLP  |
   +-----------------------------------------------------------------------------+
```

Two things to notice:

- **The hub only talks to ROKS's API and to Git.** Every container the install needs,
  including every check, runs in ROKS. The hub never needs FAR, a Helm repository, a
  config management plugin or custom health Lua.
- **The operator host reaches ROKS over the cluster's default endpoint** - the public
  one on a cluster that has both - using an admin kubeconfig fetched from IBM Cloud. If
  your workstation only has private access, set `ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1` and it
  fetches a kubeconfig for the private endpoint instead. That is independent of which
  endpoint Argo CD uses (`argocd.cluster_endpoint`, default `private`).

## 1. init

`init` builds the workspace, `~/.roksbnkargoctl/<workspace>/`. It either interviews you
or reads a `config.yaml` (`--config-file`), then resolves everything against IBM Cloud
and records the result in the `resolved:` section of the config:

- the cluster by name or ID: it must be OpenShift, in `ibmcloud.region`, in exactly one
  VPC; its private and public service endpoints and worker zones are recorded (fewer than
  three zones is a warning here and a failure in the pre-install check);
- the transit gateway by name or ID, and whether the cluster's VPC is already attached;
- the endpoint Argo CD will use for the cluster (private by default);
- the COS instance and bucket, and that both the FAR auth tarball and the JWT objects
  exist (skipped when `cos.local_far_auth_file` is set).

It then makes the workspace current. Details: [Workspaces and init](./05-workspaces-and-init.md).

## 2. render

`render` changes nothing outside the workspace. It:

1. pulls the BNK `2.4.0` manifest from FAR or the mirror;
2. pulls the f5-lifecycle-operator (FLO) chart the manifest names, and the cert-manager
   chart (`v1.17.3` by default);
3. reads the cluster's Kubernetes version (and, with a private-CA mirror, the
   `openshift-dns/node-resolver` image); `--no-cluster` skips this and uses defaults;
4. resolves the `check` image to a digest;
5. templates both charts in-process with the Helm Go SDK, adds the BNK objects and the
   checks, stamps every object with its sync wave, and splits the result three ways.

```text
~/.roksbnkargoctl/<workspace>/manifests/
  git/               exactly what is published to Git and synced by Argo CD
  direct/            what install writes straight into ROKS (Secrets REDACTED)
  application.yaml   the Argo CD Application
```

Every rendered `Secret` - yours or a chart's - goes to `direct/`, never to `git/`. As a
last line of defence, the render fails if the registry password or the JWT appears
anywhere in a Git object. Rendering is deterministic: an unchanged config re-renders
byte for byte, and re-publishing it commits nothing.

## 3. install

`install` is idempotent: when it stops, fix the cause and run it again. In order:

| Step | Talks to | What happens |
|---|---|---|
| 1. Argo CD | Argo CD | Reads the server version; refuses anything older than 3.3 (PreDelete hooks are needed for a clean uninstall), and proves the token works (`GET /api/v1/session/userinfo` must report `loggedIn`) |
| 2. Transit gateway | IBM Transit Gateway, VPC | If the cluster's VPC is not attached, checks its address prefixes do not overlap any VPC already on the gateway, then attaches it and waits (up to 10 minutes) |
| 3. Trusted profile | IBM IAM | Creates `<cluster>-f5-cne-controller-<bnk namespace>`, links it to ServiceAccount `f5-cne-controller` in the BNK namespace of this cluster, and grants Viewer + Editor on VPC Infrastructure (`is`) scoped to the cluster's VPC and Viewer on `containers-kubernetes` scoped to the cluster |
| 4. Render | FAR or mirror, ROKS | As `render` above |
| 5. Out-of-band objects | ROKS | Applies `manifests/direct/`: namespaces, then the `check` ServiceAccount and RBAC, then every Secret |
| 6. Register ROKS | ROKS, Argo CD | Creates ServiceAccount `roksbnkargoctl-argocd-manager` in `kube-system` bound to `cluster-admin` with a long-lived token Secret, then adds the cluster to Argo CD with that token |
| 7. Publish | Git | Commits `manifests/git/` to `git.path` on `git.branch` |
| 8. Application | Argo CD | Adds SSH known hosts if configured, adds the repository, creates or updates the Application, then syncs it and waits (`--timeout`, default 75 minutes); `--no-sync` stops before the sync |

### Out-of-band objects

These cannot live in Git, so `install` writes them directly into ROKS.

| Object | Namespace(s) | Why not Git |
|---|---|---|
| `far-secret` or `mirror-secret` (pull secret) | BNK and utils namespaces; with `mirror`, also `cert-manager` (if installed) and `roksbnkargoctl-check` | Credential |
| `bnk-license-jwt` | `roksbnkargoctl-check` | `License.spec.jwt` is required and inline in the 2.4 CRD; a `License` in Git would leak the JWT. The `check license` hook builds the `License` from this Secret. |
| `licenseserver-rootca` (disconnected only) | utils namespace | Produced by `flp up`, or read from `flp.external.root_ca_file` |
| Namespace `roksbnkargoctl-check`, SA `check`, its ClusterRole and bindings | cluster | Must outlive the Application, so the PostDelete check can still run after Argo CD has deleted everything it owns |
| Any Secret a chart renders | chart's namespace | Every `kind: Secret` is kept out of Git |

### Cluster registration

Argo CD reaches ROKS through the cluster's **private** service endpoint, over the transit
gateway, unless you set `argocd.cluster_endpoint: public`. The private endpoint presents
a certificate that chains to the cluster's own root CA, not to a public root, so the
registration sends that CA (taken from the token Secret's `ca.crt`). For the public
endpoint no CA is sent: its certificate is publicly trusted, and a cluster CA would
replace Argo CD's system roots and break verification.

### The Application

| Field | Value |
|---|---|
| Name | `argocd.application`, default `bnk-<workspace>` |
| Namespace | `argocd` |
| Project | `argocd.project`, default `default` |
| Source | `git.url`, `git.branch` (default `main`), `git.path` (default `bnk/<workspace>`), directory recursion on |
| Destination | the registered ROKS endpoint, namespace `bnk.namespace` |
| Finalizer | `resources-finalizer.argocd.argoproj.io` (so deletion cascades and the uninstall hooks run) |
| Sync options | `ServerSideApply=true`, `RespectIgnoreDifferences=true`, `PruneLast=true` |
| Sync policy | Manual, no retries: the sync is your approval |

The hub's `argocd-cm` is yours, so no custom health Lua is required. Without it, Argo CD
treats F5 custom resources as healthy as soon as they exist; the `check license` hook is
the real gate, and fails the sync if BNK does not come up.

## 4. The sync, wave by wave

Argo CD applies the Git path in ascending sync waves and does not start a wave until the
previous wave is healthy and its hooks have finished.

| Wave | Objects |
|---|---|
| (install) | Out of band, before any sync: namespace `roksbnkargoctl-check`, SA `check`, its ClusterRole and bindings (including SCC `privileged`), and every Secret |
| −20 | Namespaces `f5-bnk`, `f5-utils`, `cert-manager` (`Delete=false`: `check post-uninstall` deletes them) |
| −19 | ConfigMap `registry-ca` + DaemonSet `registry-ca-trust` (private-CA mirror only) |
| −18 | DaemonSet `check-node-probe` (`check node-probe` on every node, host network); hook `check pre-install` (Sync) |
| −12 | cert-manager chart (CRDs `Delete=false`; `startupapicheck` disabled) |
| −11 | Hook `check cert-manager-ready` (Sync): dry-run creates a ClusterIssuer until cert-manager's webhook admits it |
| −10 / −9 / −8 | ClusterIssuer `selfsigned-cluster-issuer` / Certificate `ext-ca` / ClusterIssuer `sample-issuer` |
| −6 | NetworkAttachmentDefinition `ens3-ipvlan-l2`; SCC binding for `flo-f5-lifecycle-operator`; Deployment `check-gateway-api-sweep` |
| −5 | FLO chart (its `k8s.f5.com` CRDs, `Delete=false`) |
| −4 | `CNEManifest bnk-2.4.0` |
| −2 | `CNEInstance <bnk.namespace>-f5-cne-controller` (`f5-bnk-f5-cne-controller` by default) |
| 0 | Hook `check license` (Sync): builds `License` from the JWT Secret, waits for `status.state=Active`, then for `CNEInstance Available=True` |
| PostSync | Hook `check post-install` |
| PreDelete | Hook `check pre-uninstall` (Argo CD 3.3 or later); `uninstall` also runs it itself, first |
| PostDelete | Hook `check post-uninstall`; `uninstall` also runs it itself, after the delete |

The namespace names shown are the defaults (`bnk.namespace`, `bnk.utils_namespace`). The
`cert-manager` namespace and chart are absent when `bnk.cert_manager.install: false`.

Why the order is what it is:

- **Nothing of BNK is applied if the cluster is not ready.** Wave −18 runs the node probe
  on every node and the pre-install check, which waits for fresh verdicts from all nodes.
  If any node cannot reach FAR or the mirror, F5 licensing or the license proxy, or the
  cluster is too small, the sync stops before cert-manager.
- **Deployment health is not webhook readiness.** cert-manager's webhook can lag its
  Deployment, so the issuers wait for `check cert-manager-ready` rather than for the
  chart to report healthy.
- **The Gateway API sweep is a Deployment, not a hook.** It must keep deleting
  OpenShift's Gateway API admission policy while FLO's CRD installer runs in the *next*
  wave. A blocking hook in wave −6 would deadlock, because Argo CD will not start wave −5
  until the hooks of −6 finish.
- **The `License` is built by a hook.** Its CRD is installed at runtime by FLO, and its
  spec carries the JWT inline. Building it in the wave 0 hook avoids both problems.

A connected install on a three-zone cluster typically spends 8 to 10 minutes in the sync.

## 5. status

`status` shows the workspace, the Application's sync and health and its last operation
message, the result of each check Job, and the state of the `CNEInstance` and `License`.
`diagnose` collects everything (Application tree, check logs, pods, events, BNK CR state)
into `diagnostics/<time>/` in the workspace. See [status and diagnose](./09-status-and-diagnose.md).

## 6. uninstall

```text
uninstall
  -> runs check pre-uninstall, waits      drain F5 resources while FLO still runs;
                                           License, then CNEInstance, last
  -> Argo CD deletes the Application (foreground cascade)
       -> prune, in reverse wave order     (CRDs and BNK namespaces are kept)
  -> runs check post-uninstall, waits     license secrets, stuck F5 finalizers,
                                           BNK namespaces, leftovers report
  -> verifies the BNK namespaces are gone
  -> CLI removes what install wrote out of band:
       Secrets, check namespace + RBAC, the Argo CD manager ServiceAccount,
       the Argo CD cluster registration, the IAM trusted profile
```

`uninstall` runs the two checks as Jobs it waits on, not only as Argo CD's
PreDelete/PostDelete hooks: Argo CD 3.5.1 can count a delete hook as finished the moment it
creates it ([argoproj/argo-cd#29100](https://github.com/argoproj/argo-cd/issues/29100)).
Throughout, `uninstall` streams the checks' logs into `diagnostics/uninstall-<time>/`,
because a hook pod is deleted with the Application. F5 CRDs stay by design: deleting a CRD deletes every CR, and F5 CRs whose
controller is gone can hang their namespace. Git history stays unless `--purge-git`; the
repository entry in Argo CD stays unless `--remove-repo`; the transit gateway connection
stays unless `--detach-tgw` (and then only if `install` created it). See
[uninstall](./10-uninstall.md).

## See also

- [The Argo CD Application: what is in Git, and what is not](./07-the-application.md)
- [Checks reference](./16-checks.md)
- [Appendix D: Security model](./appendix-d-security.md)
