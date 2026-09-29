# roksbnkargoctl

Install and uninstall **F5 BIG-IP Next for Kubernetes (BNK) 2.4 GA** on an existing
IBM Cloud ROKS cluster as **one Argo CD Application** in your existing Argo CD.

**Install and uninstall only.** It does not upgrade BNK. BNK supports an in-place upgrade by
changing the manifest version in its custom resources; follow F5's BNK documentation for
that. It is beyond the scope of this tool.

No Terraform. IBM Cloud, Kubernetes, Git and Argo CD are driven through their
APIs by one Go binary that runs natively on Linux, macOS and Windows. It is the
slim, Argo-CD-only successor to [roksbnkctl](https://github.com/jgruberf5/roksbnkctl).

**The book:** https://jgruberf5.github.io/roksbnkargoctl/ (also a PDF on every release).

## Install

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.ps1 | iex
```

Both install the latest release for BNK 2.4.0, verify its SHA256, and put it on `PATH`
with `roksbnkargoctl self install`. `ROKSBNKARGOCTL_VERSION` pins a release,
`ROKSBNKARGOCTL_BNK_VERSION` picks another BNK release and `ROKSBNKARGOCTL_INSTALL_DIR`
the directory (see the book's install chapter). Later, `roksbnkargoctl self update`
updates the tool in place, for the same BNK version; it does not upgrade BNK.

Or build it from the repository: `make build && ./bin/roksbnkargoctl self install`
(Go 1.26).

Or download the archive for your platform from
[Releases](https://github.com/jgruberf5/roksbnkargoctl/releases). Each binary installs
one BNK release, and its name says which:
`roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>`. Linux and macOS use `.tar.gz`,
Windows uses `.zip`, each for amd64 and arm64. Check it against `…_checksums.txt`, put
`roksbnkargoctl` on your `PATH`, and confirm with:

```sh
roksbnkargoctl version    # roksbnkargoctl v0.5.0 for BNK 2.4.0 (…)
```

## What you need

- A ROKS cluster (OpenShift 4.16+, workers in 3 zones) and a transit gateway, by name or ID.
- An Argo CD **3.3 or later** outside the cluster, reachable over the transit gateway,
  and an API token for it (clusters, repositories and applications; plus `certificates,
  create` when `git.known_hosts_file` is set).
- A Git repo Argo CD can read, and a token (or SSH key) that can push to it. SSH host keys
  are verified: github.com, gitlab.com and bitbucket.org are built in; for any other Git
  server set `git.known_hosts_file` (it is also added to Argo CD).
- The FAR auth tarball and subscription JWT from MyF5, in COS (or local files).
- `IBMCLOUD_API_KEY` in the environment.

## Quick start

```sh
export IBMCLOUD_API_KEY=…  ARGOCD_AUTH_TOKEN=…  ROKSBNKARGOCTL_GIT_TOKEN=…

roksbnkargoctl init -w demo            # interview; or --config-file config.yaml
roksbnkargoctl render                  # writes every manifest into the workspace
roksbnkargoctl install                 # IBM + ROKS prerequisites, Git, Application, sync
roksbnkargoctl status
roksbnkargoctl uninstall
```

`render` shows you exactly what will be published: `~/.roksbnkargoctl/<ws>/manifests/git/`
is the complete content of your Git path. Secrets never go to Git; `install` writes
them straight into ROKS (`manifests/direct/` shows them redacted).

## How it works

| Where | What |
|---|---|
| IBM Cloud (`install`) | IAM trusted profile for BNK's CNE controller; attaches the cluster VPC to the transit gateway if needed |
| ROKS, out of band (`install`) | FAR or mirror pull secret, the JWT, the FLP CA, the `roksbnkargoctl-check` namespace and RBAC, and the ServiceAccount Argo CD manages the cluster with |
| Git → Argo CD | namespaces, cert-manager, the issuer chain, FLO, `CNEManifest`, `CNEInstance`, and the check hooks, ordered by sync wave |

Nothing runs on the Argo CD hub. Every check runs **in ROKS**, in the `check`
container (one static binary on `scratch`):

| Check | When | What |
|---|---|---|
| `pre-install` | before anything of BNK | OpenShift version, zones, workers, storage class, no existing BNK, Secrets present — and, **from every node**, DNS + TLS reachability to FAR or the mirror, the license proxy or F5 licensing |
| `gateway-api-sweep` | during FLO's install | keeps OpenShift's Gateway API admission policy out of FLO's CRD installer's way |
| `license` | last sync wave | builds `License` from the JWT Secret, waits for `Active` and `CNEInstance Available` |
| `post-install` | PostSync | FLO, CNEInstance, License, TMM replicas spread across zones, no image-pull failures |
| `pre-uninstall` | `uninstall`, before the delete (and PreDelete) | drains F5 resources while FLO still runs, `CNEInstance` last |
| `post-uninstall` | `uninstall`, after the delete (and PostDelete) | license secrets, namespaces, stuck F5 finalizers, leftovers report |

## Modes

- **connected**: nodes reach FAR and F5 licensing directly.
- **disconnected**: BNK licenses through an F5 License Proxy. `roksbnkargoctl flp up`
  builds one on a VSI attached to the transit gateway. Pair it with a private
  registry: `roksbnkargoctl registry replicate`, then `registry.source: mirror`.

## Optional components

| Command | Purpose |
|---|---|
| `cos publish/list/verify` | keep the FAR tarball and JWT in COS; check the FAR key reads GA |
| `registry bom/replicate/verify` | replicate BNK 2.4 GA, cert-manager and the check image into Harbor, Artifactory or ICR |
| `flp up/down/status` | the license proxy VSI for disconnected mode |
| `argocd up/down/status` | a **test** Argo CD hub (k3s + Argo CD on a VSI) for trying the install without your own |
| `agent init`, `agent <cli>` | troubleshoot with your own agentic CLI; `diagnose` collects everything it needs without kubectl |

## Design

[`docs/DESIGN.md`](docs/DESIGN.md) is the contract: the constraints, the sync order,
what cannot be YAML and who does it instead, and the check container's modes.

## License

MIT.
