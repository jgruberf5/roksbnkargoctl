# Quick start: a connected install

This chapter walks you through a complete install and uninstall in the simplest shape:
**connected** mode, images from **FAR**, the F5 files in **COS**. When you finish you
will have BNK 2.4 GA running on your ROKS cluster as one Argo CD Application, you will
have seen every file the install publishes, and you will have removed it again.

Before you start, work through [Prerequisites](./03-prerequisites.md). In particular you
need the cluster and transit gateway, an Argo CD 3.3 or later that can reach the
cluster's private endpoint over the gateway, a Git repository you can push to, the FAR
auth tarball and subscription JWT in a COS bucket, and the binary from
[Installing roksbnkargoctl](./04-installation.md).

## 1. Write config.yaml

Every value below is an example; replace the names with yours. The file holds no
secrets, so you can keep it in version control.

```yaml
# config.yaml - connected mode, images from FAR, F5 files in COS
ibmcloud:
  region: us-south                 # region of the ROKS cluster
  resource_group: default

cluster: my-roks                   # existing ROKS cluster, name or ID
transit_gateway: my-tgw            # existing transit gateway, name or ID

bnk:
  version: "2.4.0"                 # the only supported version
  mode: connected                  # nodes license directly with F5
  tmm_replicas: 3

registry:
  source: far                      # pull from repo.f5.com

cos:
  instance: bnk-supply-chain       # COS instance holding the F5 files
  bucket: my-bnk-bucket
  region: us-south                 # the bucket's region
  far_auth_object: f5-far-auth-key.tgz
  jwt_object: subscription.jwt

argocd:
  server: https://argocd.example.internal
  project: default
  cluster_endpoint: private        # Argo CD reaches ROKS over the transit gateway

git:
  url: https://github.com/example-org/platform-gitops.git
  branch: main
  path: bnk/demo                   # the only path the install writes
  username: git
```

Everything not listed takes its default: the Application is named `bnk-demo`, BNK goes
into `f5-bnk` and `f5-utils`, cert-manager `v1.17.3` is installed, and the secrets are
read from the default environment variables. [Appendix A](./appendix-a-config.md) lists
every key.

## 2. Export the secrets

```sh
export IBMCLOUD_API_KEY=…            # IBM Cloud API key
export ARGOCD_AUTH_TOKEN=…           # Argo CD API token
export ROKSBNKARGOCTL_GIT_TOKEN=…    # Git token with push rights to the repo
```

They are read from the environment on each run and never written to disk.

## 3. init

```sh
roksbnkargoctl init -w demo --config-file config.yaml
```

```text
→ resolving cluster my-roks
✓ cluster my-roks (<cluster-id>), OpenShift 4.21.31, VPC my-roks-vpc, zones us-south-1,us-south-2,us-south-3
→ resolving transit gateway my-tgw
✓ transit gateway my-tgw: the cluster VPC is attached
→ resolving COS instance bnk-supply-chain
✓ COS bnk-supply-chain/my-bnk-bucket has f5-far-auth-key.tgz and subscription.jwt
✓ workspace "demo" saved to /home/you/.roksbnkargoctl/demo/config.yaml
  next: roksbnkargoctl render   (then review /home/you/.roksbnkargoctl/demo/manifests)
```

If the cluster's VPC is not yet on the gateway you will see this instead, which is fine:

```text
⚠ transit gateway my-tgw: the cluster VPC is NOT attached; `install` will attach it
```

`demo` is now the current workspace, so later commands need no
`-w`.

## 4. render, and review

```sh
roksbnkargoctl render
```

`render` pulls the BNK manifest and charts from FAR, reads the cluster's Kubernetes
version, resolves the check image, and writes every manifest. It changes nothing in the
cluster, Argo CD or Git. It ends with a summary of the Git objects by sync wave
(abbreviated here):

```text
✓ rendered … Git objects and … direct objects into /home/you/.roksbnkargoctl/demo/manifests
     -20  Namespace×3
     -18  DaemonSet, hook:check-pre-install
     -12  …                                   (the cert-manager chart)
     -11  hook:check-cert-manager-ready
     -10  ClusterIssuer
      -9  Certificate
      -8  ClusterIssuer
      -6  ClusterRoleBinding, Deployment, NetworkAttachmentDefinition
      -5  CustomResourceDefinition×…, …       (the FLO chart)
      -4  CNEManifest
      -2  CNEInstance
       0  hook:check-license
   hooks  PostSync:check-post-install, PreDelete:check-pre-uninstall, PostDelete:check-post-uninstall
```

Now review what will be published:

```sh
ls ~/.roksbnkargoctl/demo/manifests/git/
ls ~/.roksbnkargoctl/demo/manifests/direct/
cat ~/.roksbnkargoctl/demo/manifests/application.yaml
```

What to look for:

- **`manifests/git/` is the whole of what your repository will contain** under
  `bnk/demo`. There is no `Secret` in it, and no value from your FAR key or JWT.
- **`manifests/direct/`** is what `install` will write straight into ROKS: the
  `roksbnkargoctl-check` namespace and RBAC, the BNK namespaces, `far-secret` in
  `f5-bnk` and `f5-utils`, and `bnk-license-jwt` in `roksbnkargoctl-check`. Secret values
  are shown as `REDACTED`.
- The `CNEInstance` file shows `deploymentSize: Tiny` and your TMM replica count.
- The check Jobs and DaemonSet reference the check image **by digest**
  (`ghcr.io/jgruberf5/roksbnkargoctl-check@sha256:…`).
- `application.yaml` points at your repo, branch and path, and at the cluster's private
  endpoint.

## 5. install

```sh
roksbnkargoctl install
```

`install` publishes, registers, creates the Application and syncs it, then waits for the
sync to finish. On a first install the output has this shape (abbreviated):

```text
✓ Argo CD v3.5.1+… at https://argocd.example.internal
✓ cluster VPC attached to transit gateway my-tgw
→ creating IAM trusted profile my-roks-f5-cne-controller-f5-bnk
✓ trusted profile my-roks-f5-cne-controller-f5-bnk (Profile-…)
→ fetching the BNK 2.4.0 manifest from repo.f5.com
✓ rendered … Git objects, … direct objects
→ writing … out-of-band objects into ROKS (Secrets, check namespace + RBAC)
✓ out-of-band objects applied
→ registering my-roks with Argo CD (private endpoint https://c…private.us-south.containers.cloud.ibm.com:…)
✓ cluster registered
→ publishing manifests/git to https://github.com/example-org/platform-gitops.git (main:bnk/demo)
✓ published commit 1a2b3c4d5e
✓ Application bnk-demo created in project default
→ syncing bnk-demo (pre-install check → cert-manager → FLO → CNEInstance → license)
  [14:02:11] sync=OutOfSync health=Missing  Running …
  …
  [14:11:40] sync=Synced health=Healthy  Succeeded …
✓ sync Succeeded: …
```

Expect roughly 8 to 10 minutes in the sync. The first thing the sync runs is the
pre-install check: if any node cannot reach FAR, `ghcr.io`, `quay.io` or F5 licensing, or
the cluster is too small, it fails there and nothing of BNK is applied. `install` is
idempotent; fix the cause and run it again.

You can watch the same sync in the Argo CD UI: the Application is `bnk-demo`.

## 6. status

```sh
roksbnkargoctl status
```

```text
Workspace:       demo
Cluster:         my-roks
Transit gateway: my-tgw
Mode / registry: connected / far (repo.f5.com)
Git:             https://github.com/example-org/platform-gitops.git main:bnk/demo
Last published:  1a2b3c4d5e
Application:     bnk-demo  sync=Synced  health=Healthy
Last operation:  Succeeded — …
Checks:
  check-cert-manager-ready     Succeeded
  check-license                Succeeded
  check-post-install           Succeeded
  check-pre-install            Succeeded
CNEInstance:     … Available=True …
License:         state=Active …
```

A healthy install has every check `Succeeded`, the `CNEInstance` `Available=True` and the
`License` `state=Active`. If anything is not, run `roksbnkargoctl diagnose` and read
`diagnostics/<time>/summary.md`; see [status and diagnose](./09-status-and-diagnose.md).

## 7. uninstall

```sh
roksbnkargoctl uninstall
```

It asks `Uninstall BNK from my-roks (Application bnk-demo)? [y/N]:`. Pass `--yes` to
skip the prompt; without a terminal and without `--yes` it refuses.

```text
→ deleting Application bnk-demo (PreDelete check → prune → PostDelete check)
  [14:30:02] sync=Synced health=Healthy  …
  …
✓ Application deleted
✓ uninstall check logs (… pods) saved to /home/you/.roksbnkargoctl/demo/diagnostics/uninstall-…
→ removing out-of-band objects
✓ uninstalled
```

Argo CD runs `check pre-uninstall` (drains F5 resources while FLO still runs, then
deletes the `License` and the `CNEInstance`), prunes everything it synced, and runs
`check post-uninstall` (license secrets, stuck finalizers, the BNK namespaces). The tool
then removes the Secrets, the check namespace and RBAC, the Argo CD cluster registration
and its ServiceAccount, and the IAM trusted profile.

What stays, by design:

| Left in place | To remove it too |
|---|---|
| F5 CRDs in the cluster | Not removed: deleting a CRD deletes every CR and can hang namespaces |
| The manifests in your Git history | `--purge-git` removes the path in a new commit (history remains) |
| The repository entry in Argo CD | `--remove-repo` (other Applications may use it) |
| The transit gateway connection | `--detach-tgw`, and only if `install` created it |
| The trusted profile | It is removed unless you pass `--keep-trusted-profile` |

## Next steps

- Install without internet egress: [Connected and disconnected modes](./11-modes.md),
  [The F5 License Proxy](./12-flp.md), [Mirroring into a private registry](./13-registry.md).
- Understand what is in Git: [The Argo CD Application](./07-the-application.md).
- No Argo CD to try it with? [A test Argo CD hub](./15-test-hub.md).

## See also

- [install](./08-install.md)
- [uninstall](./10-uninstall.md)
- [Troubleshooting guide](./17-troubleshooting.md)
