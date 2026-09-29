# A test Argo CD hub (argocd)

roksbnkargoctl installs into your existing Argo CD. To exercise the install without one,
`roksbnkargoctl argocd up` builds a small, disposable Argo CD on a VSI. This chapter
describes what it builds, how it is secured, and how to point a workspace at it. After
reading it you will be able to stand up a test hub, run an install against it, and remove
it.

> The test hub is for testing only. It is a single-node k3s cluster with upstream Argo CD,
> a self-signed certificate and one admin-level API account. Do not use it as a production
> Argo CD.

## Commands

| Command | Does |
|---|---|
| `roksbnkargoctl argocd up` | Builds the VSI, waits for Argo CD, mints an API token, points the workspace at the hub, prints `export ARGOCD_AUTH_TOKEN=…` |
| `roksbnkargoctl argocd status` | Prints the URL, the version and the instance id, and fails if Argo CD does not answer |
| `roksbnkargoctl argocd down [-y]` | Deletes what `argocd up` built and removes `argocd-hub.json` |

None take command-specific flags. `argocd down` asks for confirmation; pass `--yes` when
there is no terminal.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `test_hub.cidr` | none (required) | Address prefix and subnet for the hub VPC, a /24 to /28 not used by any VPC on the transit gateway, for example `10.248.1.0/28` |
| `test_hub.region` | `ibmcloud.region` | Region for the hub |
| `test_hub.zone` | `<region>-1` | Zone |
| `test_hub.profile` | `bx2-4x16` | VSI profile |
| `test_hub.version` | `3.5.1` | Upstream Argo CD version |
| `test_hub.k3s_channel` | `stable` | k3s release channel |
| `test_hub.node_port` | `30443` | NodePort for the Argo CD API and UI |
| `test_hub.allowed_cidr` | `0.0.0.0/0` | Source allowed to reach the NodePort (and `:22` with an SSH key). Narrow it to your own address |
| `test_hub.ssh_key` | empty (no SSH) | Name of an existing VPC SSH key |
| `test_hub.attach_to_tgw` | `true` | Attach the hub VPC to the cluster's transit gateway |
| `test_hub.name` | `<workspace>-argocd` | Base name of every resource |

## What `argocd up` builds

The network follows the same pattern as the [FLP](./12-flp.md), under the base name
`test_hub.name`:

| Resource | Name | Details |
|---|---|---|
| VPC | `<name>-vpc` | Manual address prefixes; `test_hub.cidr` checked for overlap against the transit gateway before creation (when attaching) |
| Address prefix | `<name>-prefix` | `test_hub.cidr` |
| Public gateway | `<name>-pgw` | Egress for k3s, Argo CD manifests and images |
| Subnet | `<name>-subnet` | `test_hub.cidr` |
| Security group | `<name>-sg` | Outbound: all. Inbound TCP `node_port` from `allowed_cidr`; TCP `22` from `allowed_cidr` when `ssh_key` is set |
| Floating IP | `<name>-fip` | Always; the hub's URL is `https://<floating-ip>:<node_port>` |
| Instance | `<name>` | Latest `ibm-ubuntu-24-04-*-minimal-amd64-*`, 100 GB boot volume |
| Transit gateway connection | `<name>` | Unless `attach_to_tgw: false`. Lets the hub reach the ROKS private endpoint |

Everything is found by name before it is created, and a failed `up` writes a partial record
that `argocd down` cleans up.

### On the VSI

Cloud-init:

1. Installs k3s from `get.k3s.io` on `test_hub.k3s_channel`.
2. Creates namespace `argocd` and server-side-applies the upstream
   `argoproj/argo-cd` `v<version>/manifests/install.yaml`.
3. Changes Service `argocd-server` to `NodePort`: HTTPS on `node_port` (and HTTP on
   `30080`, which the security group does not open).
4. Waits for `argocd-server`, `argocd-repo-server` and `argocd-application-controller`.
5. Sets the admin password and deletes `argocd-initial-admin-secret`.
6. Enables a local account `roksbnkargoctl` with the `apiKey` capability in `argocd-cm`, and
   grants it `role:admin` in `argocd-rbac-cm`.
7. Restarts `argocd-server`.

`argocd up` then polls `https://<floating-ip>:<node_port>/api/version` for up to 20 minutes
(the k3s and Argo CD install takes about 6 minutes).

## The admin password and the API token

| Secret | How it is made | Where it is kept |
|---|---|---|
| Admin password | 18 random bytes, base64url, generated on your host. Only its bcrypt hash goes into the VSI's user data | `<workspace>/argocd-hub.json` (`admin_password`), mode 0600. Reused if you run `argocd up` again |
| API token | Minted after the hub is ready: log in as `admin` (`POST /api/v1/session`), then `POST /api/v1/account/roksbnkargoctl/token` | Printed once as `export ARGOCD_AUTH_TOKEN=<token>`; not stored |

`argocd-hub.json` also holds the URL, the Argo CD version and the id of every resource
created. For the UI, log in as `admin` with the password from that file.

## Pointing a workspace at the hub

On success `argocd up` updates the workspace's `config.yaml`:

| Key | Set to |
|---|---|
| `argocd.server` | `https://<floating-ip>:<node_port>` |
| `argocd.insecure` | `true` (the hub's certificate is self-signed) |

and prints the token line. Run it in your shell, then install as usual:

```sh
roksbnkargoctl argocd up
export ARGOCD_AUTH_TOKEN=…        # the line argocd up printed
roksbnkargoctl install
```

The variable name printed is `argocd.token_env` (default `ARGOCD_AUTH_TOKEN`). If you lose
the token, run `argocd up` again: it reuses the VSI and the password and mints a new one.

The hub registers the cluster by its private endpoint by default, so keep
`test_hub.attach_to_tgw: true` unless you set `argocd.cluster_endpoint: public`. The hub
satisfies the Argo CD 3.3 minimum; `install` was verified against Argo CD 3.5.1.

## Teardown

Uninstall BNK first (the Application lives on the hub), then:

```sh
roksbnkargoctl argocd down --yes
```

It deletes, waiting for each: the transit gateway connection, the instance, the floating
IP, the subnet, the security group, the public gateway and the VPC. `argocd.server` in
`config.yaml` is left as it was; point it at your real Argo CD before the next install.

## See also

- [install](./08-install.md)
- [The F5 License Proxy](./12-flp.md), built the same way
- [Prerequisites](./03-prerequisites.md)
- [Appendix A. config.yaml reference](./appendix-a-config.md)
