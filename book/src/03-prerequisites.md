# Prerequisites

This chapter lists everything that must exist before you run `roksbnkargoctl install`:
the cluster, the network path, Argo CD, Git, the F5 entitlement files, IBM Cloud
credentials and permissions, and the egress each mode needs. Work through it once as a
checklist; most failed installs trace back to one line of it.

## At a glance

| You need | Checked by |
|---|---|
| A ROKS cluster: OpenShift 4.16 or later, schedulable workers in 3 zones, at least 3 of them | `init` (OpenShift, one VPC, zones warning), `check pre-install` (version, zones, workers) |
| A transit gateway | `init` (resolves it), `install` (attaches the cluster VPC if needed) |
| Argo CD 3.3 or later, outside the cluster, reachable over the transit gateway to the ROKS private endpoint | `install` (version), the first sync wave (reachability) |
| An Argo CD API token | `init` (when you use an existing Argo CD), `install` |
| A Git repository, and a token or SSH key that can push to it (read access is enough for `install --no-publish`) | `init` and `install` (access check), `install` (publish), Argo CD (clone) |
| The FAR auth tarball and the subscription JWT from MyF5, in COS or as local files | `init` (objects exist), `render` / `install` (read them) |
| `IBMCLOUD_API_KEY` in the environment, with the IAM access below | every command that calls IBM Cloud |
| Node egress for your mode | `check-node-probe` and `check pre-install`, before any BNK object is applied |
| Hub egress to the chart registry (FAR and `quay.io`, or the mirror) | Argo CD, when it renders the Application's Helm sources |

## The ROKS cluster

| Requirement | Detail |
|---|---|
| Platform | Red Hat OpenShift on IBM Cloud (ROKS) on VPC. `init` refuses a cluster that is not OpenShift, that is in a region other than `ibmcloud.region`, or that spans more than one VPC. |
| Version | OpenShift 4.16 or later (`check pre-install`). |
| Zones and workers | Schedulable, Ready workers spanning at least 3 zones, and at least 3 of them. BNK spreads TMM replicas across zones. |
| Storage | A default StorageClass (or set `bnk.storage_class`). At `Tiny`, TMM mounts no persistent volume, so any class serves any number of TMM replicas; ReadWriteMany is not required. |
| No existing BNK | `check pre-install` fails if it finds a BNK install not owned by this Application. Re-syncs of the same Application pass. On a cluster where BNK was installed before, see [uninstall](./10-uninstall.md). |
| Service endpoints | A private service endpoint (the default for Argo CD) or a public one (`argocd.cluster_endpoint: public`). The operator host uses the cluster's default endpoint (the public one, when the cluster has both) unless `ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1`. |

## The transit gateway

The transit gateway is how the Argo CD hub (and, in disconnected mode, the license proxy
VSI) reaches the cluster privately. It must already exist; you give its name or ID as
`transit_gateway`.

- If the cluster's VPC is already attached, nothing changes.
- If it is not, `install` attaches it. It first reads the address prefixes of every VPC
  already on the gateway and **refuses to attach** if any overlaps the cluster VPC's
  prefixes, because a gateway silently blackholes one of two overlapping VPCs. Plan your
  VPC address space accordingly.
- The network your Argo CD hub runs in must also be connected to the same gateway.

## Argo CD

| Requirement | Detail |
|---|---|
| Version | **3.3 or later**. The uninstall depends on PreDelete hooks, which arrived in 3.3. `install` reads `/api/version` and refuses an older server. |
| Location | Outside the ROKS cluster. The hub's own Argo CD never runs any container of this install. |
| Reachability | The hub must reach the ROKS **private** service endpoint over the transit gateway (or the public endpoint with `argocd.cluster_endpoint: public`), your Git repository, and the registry the charts come from: see [From the Argo CD hub](#from-the-argo-cd-hub). |
| API access from the operator host | `argocd.server` is the URL of the Argo CD API (normally `https://…`) and must be reachable from the operator host. Use `argocd.ca_file` for a private CA, or `argocd.insecure: true` to skip verification of a self-signed server. |
| Application namespace | The Application is created in namespace `argocd`. |
| Project | `argocd.project` (default `default`) must allow the Git repository, the chart registries (as source repositories) and the ROKS destination. |
| Health customisation | None needed. The `check license` hook is the gate for BNK coming up. |

### The Argo CD API token

`init` (for an existing Argo CD) and `install` read the token from `ARGOCD_AUTH_TOKEN` (or the variable named by
`argocd.token_env`). The account behind it must be allowed to:

- read the server version;
- create, update and delete **clusters** (the ROKS registration);
- create, update and delete **repositories** (the Git repository entry and one OCI Helm
  entry per chart registry), and **certificates** if you use `git.known_hosts_file` (SSH
  known hosts) or a mirror with `registry.mirror.ca_file` (its CA, as a TLS certificate);
- create, get, sync and delete **applications** in the chosen project, and read their
  resource tree and managed resources (used by `status` and `diagnose`).

On a hub you administer, the `admin` account or a dedicated local account with
equivalent RBAC works. Generate the token in the Argo CD UI or with the Argo CD CLI.

## Git

| Requirement | Detail |
|---|---|
| Repository | `git.url`, over HTTPS (`https://…`) or SSH (`git@…`). Argo CD must be able to read it. |
| Branch and path | `git.branch` (default `main`) and `git.path` (default `bnk/<workspace>`), a relative path inside the repo. Only that path is written. |
| HTTPS credential | A token that can push, from `ROKSBNKARGOCTL_GIT_TOKEN` (or `git.token_env`), with `git.username` (default `git`). `init` checks the repository can be read and warns if the token cannot push; `install` refuses to start without push rights. With `install --no-publish` read access is enough, and the credential is optional for a repository readable without one. |
| SSH credential | A private key file, `git.ssh_key_file`. |
| SSH host keys | Always verified. The keys of `github.com`, `gitlab.com` and `bitbucket.org` are built in. For any other Git server, set `git.known_hosts_file`; its entries are also added to Argo CD so the hub's clone verifies the same host. |

The same credential is given to Argo CD as the repository credential, so a token with
push rights is stored in Argo CD. Every `install` sets it again (an upsert). If your
policy requires a read-only credential on the hub, commit an `export` yourself and run
`install --no-publish` with a read-only token or deploy key: `--no-publish` needs only
read access, and that credential is the one registered in Argo CD (see
[install](./08-install.md#without-letting-roksbnkargoctl-push-to-git)).

`ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1` switches SSH host-key verification off. It is an
escape hatch that prints a loud warning; prefer `git.known_hosts_file`.

## F5 entitlement: FAR auth tarball and subscription JWT

Download both from MyF5 for your BNK subscription:

- the **FAR auth tarball** (`.tgz`), which holds the service-account key used to pull
  from the F5 Artifact Registry;
- the **subscription JWT**, which licenses BNK.

Keep them either:

| In COS (default) | As local files |
|---|---|
| `cos.instance` (default `bnk-supply-chain`), `cos.bucket`, `cos.region` (default `us-south`), objects `cos.far_auth_object` (default `f5-far-auth-key.tgz`) and `cos.jwt_object` (default `subscription.jwt`) | `cos.local_far_auth_file` and `cos.local_jwt_file` |

`init` checks both COS objects exist. `roksbnkargoctl cos publish` uploads them and
`cos verify` checks the FAR key logs in and the JWT parses; see
[The COS supply chain](./14-cos.md). The JWT is written only into the cluster, as Secret
`bnk-license-jwt`; it never reaches Git or the workspace.

The FAR key is written into the cluster as the pull secret `far-secret`, and is also
registered in Argo CD as the login for FAR's chart registry, so that the hub can pull the
FLO chart. It never reaches Git.

## IBM Cloud API key

`IBMCLOUD_API_KEY` must hold an API key for the account that owns the cluster
(`ibmcloud.api_key_env` names a different variable; `IC_API_KEY` is also accepted). It is
read from the environment on every run and never written to disk.

### IAM access the key needs

These are the IBM Cloud services the tool calls, and the access each call implies.

| Service | Calls | Access needed |
|---|---|---|
| Kubernetes Service (`containers-kubernetes`) | Get and list clusters; fetch the **admin** kubeconfig | Viewer to look up; **Administrator** platform role on the cluster for the admin kubeconfig |
| VPC Infrastructure Services (`is`) | Read the cluster's VPC and its address prefixes; read the prefixes of every VPC already on the transit gateway | Viewer |
| Transit Gateway | List gateways and connections; create a VPC connection (only when the cluster VPC is not attached); delete it on `uninstall --detach-tgw` | Viewer, plus Editor to attach or detach |
| IAM Identity Service | Read the key's own details (account ID); create, find, link and delete the trusted profile | A role that can create and delete trusted profiles (Administrator does) |
| IAM Access Management | Create and list access policies for the trusted profile | Administrator on the resources being granted: VPC Infrastructure Services for the cluster's VPC, and Kubernetes Service for the cluster |
| Resource Controller | List the account's COS instances, to find one by name, GUID or CRN | Viewer on its resource group |
| Cloud Object Storage | List the instance's buckets (to find the bucket's region); list the bucket; read the FAR tarball and JWT | Reader (or Content Reader) on the bucket. Without the right to list the instance's buckets, reads fall back to `cos.region` ([chapter 14](./14-cos.md#finding-the-buckets-region)) |

`flp up` and `argocd up` create VPC resources (VSI, subnet, security group, floating IP,
public gateway, SSH key) and need Editor on VPC Infrastructure Services in addition; see
their chapters.

## Egress

### From the cluster's nodes

`check-node-probe` runs on every node and tests DNS, TCP and a TLS handshake to each
target before anything of BNK is applied. The targets depend on your choices:

| Choice | Targets (every node) |
|---|---|
| `registry.source: far` | `registry.far_host` (default `repo.f5.com`):443 - F5 Artifact Registry; `ghcr.io`:443 - the check image; `quay.io`:443 - cert-manager images (unless `bnk.cert_manager.install: false`) |
| `registry.source: mirror` | the mirror host from `registry.mirror.host` (its port, default 443) |
| `bnk.mode: connected` | `product.apis.f5.com`:443 and `product-s.apis.f5.com`:443 - F5 licensing |
| `bnk.mode: disconnected` | the license proxy's host and port (default 8443), TLS handshake without verification (its certificate is self-signed) |

So a connected install from FAR needs egress to five hosts on 443; a disconnected install
from a mirror needs none to the internet, only the mirror and the license proxy.

### From the Argo CD hub

Argo CD's repository server pulls the two Helm charts over HTTPS:

| Choice | Destination |
|---|---|
| `registry.source: far` | `registry.far_host` (default `repo.f5.com`):443 for the FLO chart; `quay.io`:443 for the cert-manager chart (unless `bnk.cert_manager.install: false`) |
| `registry.source: mirror` | the mirror host from `registry.mirror.host` (its port, default 443), for both charts |

`install` registers these registries in Argo CD with the registry login; see
[the chart registries in Argo CD](./07-the-application.md#the-chart-registries-in-argo-cd).

### From the operator host

| Destination | Used by |
|---|---|
| `iam.cloud.ibm.com`, `containers.cloud.ibm.com`, `transit.cloud.ibm.com`, `resource-controller.cloud.ibm.com`, `<region>.iaas.cloud.ibm.com` | `init`, `install`, `uninstall`, `status` |
| `s3.<region>.cloud-object-storage.appdomain.cloud`, for the bucket's region (and `cos.region`) | reading the FAR tarball and JWT from COS |
| The ROKS default (normally public) service endpoint (or private, with `ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1`) | `render`, `install`, `uninstall`, `status`, `diagnose` |
| FAR (`repo.f5.com`) or the mirror; `quay.io` for the cert-manager chart when pulling from FAR | `render` (manifest and charts) |
| `ghcr.io` | `render` resolves the check image's digest (in mirror mode it falls back to the mirror's copy, with a warning, if `ghcr.io` is unreachable) |
| `argocd.server` | `install`, `uninstall`, `status`, `diagnose` |
| The Git server | `install`, `uninstall --purge-git` |

### From the Argo CD hub

The Git server, and the ROKS API endpoint it was registered with (private, over the
transit gateway, by default). Nothing else.

## See also

- [Installing roksbnkargoctl](./04-installation.md)
- [Connected and disconnected modes](./11-modes.md)
- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Appendix D: Security model](./appendix-d-security.md)
