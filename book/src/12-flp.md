# The F5 License Proxy (flp)

In disconnected mode BNK licenses through an F5 License Proxy (FLP) instead of reaching F5
directly. This chapter describes `roksbnkargoctl flp up`, `flp status` and `flp down`: what
they build in IBM Cloud, what runs on the VSI, how its CA is handled, and how `install`
connects BNK to it. After reading it you will be able to stand up an FLP, with or without a
workspace, point an install at it (or at one you already run), and remove it, even after
its state file is lost.

## Commands

| Command | Does |
|---|---|
| `roksbnkargoctl flp up` | Builds the FLP VSI and its network, and records everything in the state file (`flp-outputs.json` in the workspace) |
| `roksbnkargoctl flp status` | Shows the instance, its status, the URL BNK uses, the floating IP, the proxy version and the state file |
| `roksbnkargoctl flp down [-y]` | Deletes exactly what the state file records, then removes it; with the state lost, finds the resources by their exact names |

`flp down` asks for confirmation; pass `--yes` when there is no terminal.

### Flags

Every `flp.vsi` key is a flag of `flp up`, and so are where the FLP goes and where the F5
files come from. Each overrides its key (and the key's `ROKSBNKARGOCTL_*` variable) for the
run; none is saved.

| Flag | Key it overrides | `up` | `down` | `status` |
|---|---|---|---|---|
| `--name` | none: the base name, default the workspace name | yes | yes | yes |
| `--state` | none: the state file (below) | yes | yes | yes |
| `--ca-out` | none: write the CA certificate alone to this file | yes | | |
| `--region` | `ibmcloud.region` | yes | yes | |
| `--resource-group` | `ibmcloud.resource_group` | yes | yes | |
| `--transit-gateway` | `transit_gateway` | yes | yes | |
| `--zone` | `flp.vsi.zone` | yes | yes | |
| `--vpc` | `flp.vsi.vpc` | yes | yes | |
| `--cidr` | `flp.vsi.cidr` | yes | | |
| `--profile` | `flp.vsi.profile` | yes | | |
| `--allowed-cidrs` | `flp.vsi.allowed_cidrs` (comma-separated) | yes | | |
| `--ssh-key` | `flp.vsi.ssh_key` | yes | | |
| `--floating-ip` | `flp.vsi.floating_ip` | yes | | |
| `--far-auth-file`, `--jwt-file` | `cos.local_far_auth_file`, `cos.local_jwt_file` | yes | | |
| `--cos-instance`, `--cos-bucket`, `--cos-region` | `cos.instance`, `cos.bucket`, `cos.region` | yes | | |
| `--far-auth-object`, `--jwt-object` | `cos.far_auth_object`, `cos.jwt_object` | yes | | |

`flp status` reads the region from the state file, so it needs only `--name` or `--state`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `flp.vsi.cidr` | none (required) | Address prefix and subnet for the FLP VPC, a /24 to /28 not used by any VPC on the transit gateway, for example `10.248.0.0/28` |
| `flp.vsi.zone` | `<ibmcloud.region>-1` | Zone for the subnet and VSI |
| `flp.vsi.vpc` | empty (create one) | Name or id of an existing VPC to build in instead |
| `flp.vsi.profile` | `bx2-4x16` | VSI profile |
| `flp.vsi.allowed_cidrs` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | Sources allowed to reach `:8443` (and `:22` with an SSH key) |
| `flp.vsi.ssh_key` | empty (no SSH) | Name of an existing VPC SSH key |
| `flp.vsi.floating_ip` | `true` | Reserve and bind a floating IP |
| `flp.external.url` | empty | Use an FLP you already run, for example `https://10.0.0.5:8443` |
| `flp.external.root_ca_file` | empty | PEM file of that FLP's root CA; required with `flp.external.url` |

`flp up` also needs the FAR auth key and the subscription JWT, because the VSI logs in to
FAR to pull the proxy images and the proxy licenses with the JWT. Give them as local files
(`--far-auth-file`, `--jwt-file`) or from COS (`--cos-bucket`, with `--cos-instance` unless
it is the default `bnk-supply-chain`); see [The COS supply chain](./14-cos.md).

## What `flp up` builds

All names derive from `<name>-flp`, where `<name>` is `--name` or, by default, the
workspace name (shown as `<ws>` below). It must be lowercase letters, digits and hyphens,
start with a letter and be at most 42 characters, so that every resource name is valid in
IBM Cloud. Each piece is found by name before it is created, so a re-run after a failure
reuses what exists.

| Resource | Name | Details |
|---|---|---|
| VPC | `<ws>-flp-vpc` | Created with manual address prefixes, unless `flp.vsi.vpc` is set. Before creating it, `flp up` checks `flp.vsi.cidr` against every VPC on the transit gateway and refuses an overlap |
| Address prefix | `<ws>-flp-prefix` | `flp.vsi.cidr` in the zone (only in a VPC it created) |
| Public gateway | `<ws>-flp-pgw` | Egress for packages, FAR image pulls and F5 licensing |
| Subnet | `<ws>-flp-subnet` | `flp.vsi.cidr`, attached to the public gateway |
| Security group | `<ws>-flp-sg` | Outbound: all. Inbound TCP `8443` from each `allowed_cidrs` entry; TCP `22` from each entry when `ssh_key` is set |
| Floating IP | `<ws>-flp-fip` | Unless `floating_ip: false`. Reserved before the VSI so its address can go into the proxy certificate |
| Instance | `<ws>-flp` | Latest public `ibm-ubuntu-24-04-*-minimal-amd64-*` image, 100 GB boot volume, cloud-init user data |
| Transit gateway connection | `<ws>-flp` | Attaches the FLP VPC to the cluster's transit gateway so the ROKS workers reach the FLP privately. With `flp.vsi.vpc`, only if that VPC is not already on the gateway; a connection it already had is used as it is and is not recorded |

If a step fails, the partial record is still written and `flp up` ends with
`(partial state saved in <state file>; `flp down` cleans it up)`. On success it prints the
URL, the state file, and that the pod takes about 5 minutes to come up.

`flp up` refuses a state file that records a different FLP (another name or region),
rather than overwrite it and orphan that FLP's resources:
`<state file> records license proxy <a>-flp in <region>, not <b>-flp in <region>: pass another --state, or `flp down` that one first`.

### On the VSI

Cloud-init installs `podman`, `openssl`, `jq` and `curl`, writes the inputs to `/opt/flp/`
and runs a one-shot systemd unit that brings the stack up as a podman pod named `flp`,
publishing port 8443:

| Container | Image | Role |
|---|---|---|
| `postgresql` | `<far_host>/images/postgresql:1.29.0-0.10.40` | The proxy's database, TLS with client certificates |
| `vault` | `<far_host>/images/vault:2.0.0` | Secret storage for the proxy |
| `vault-init` | `<far_host>/images/vault-init:1.29.0-0.10.40` | Unseals Vault and issues the proxy's Vault client certificate, with rotation |
| `f5-license-proxy` | `<far_host>/images/f5-license-proxy:1.29.0-0.10.40` | The proxy, TLS on `0.0.0.0:8443`, talking upstream to `https://product.apis.f5.com/ee/v1` and `https://product-s.apis.f5.com/ee/v1` |

The server, client and Vault certificates are generated on the VSI and signed by the CA that
`flp up` injects. The proxy certificate's SANs are `flp`, `localhost`, the host name, the
private IP and, when there is one, the floating IP. The pod is registered with systemd so it
comes back after a reboot, and a timer (`flp-health.timer`, every 2 minutes after the first
5) restarts the pod if the proxy stops answering after it has served once.

The VSI always pulls the proxy images from FAR, even when the cluster uses a mirror; the FLP
images are not part of the [registry](./13-registry.md) bill of materials. The JWT and the FAR
key are part of the VSI's user data (written to mode 0600 files on the VSI).

### Why the version is pinned

The FLP version is pinned to `1.29.0-0.10.40` (Vault `2.0.0`). The BNK 2.3.x manifests listed
the license proxy; the 2.4.0 GA manifest no longer does, so its version cannot be read from
the manifest. `1.29.0-0.10.40` was the newest tag on FAR when 2.4.0 GA shipped; the proxy,
`vault-init` and `postgresql` images share it.

### `prod_jwks`

The proxy verifies F5's signed responses with F5's public key set. `flp up` pulls the
`f5-license-proxy` chart at the pinned version from `<far_host>/charts/`, extracts the
base64 value under `prod_jwks.txt:` from its templates, checks it is a JWKS (it has
`"keys"`), and ships it to the VSI as `prod_jwks.txt`.

## The CA

| Property | Value |
|---|---|
| Generated by | `flp up`, on your host |
| Key and certificate | ECDSA P-256, CN `flp.ca`, O `F5`, valid 10 years |
| Stored in | The state file (`root_ca_pem`, `root_ca_key_pem`), mode 0600 |
| Certificate alone | Also written to `--ca-out` (default `./<name>-flp-ca.pem` without a workspace), mode 0600, for `flp.external.root_ca_file` |
| On re-run | Reused (`keeping the CA from the previous flp up`), because BNK may already trust it |
| After `flp down` | Gone with the state file (and the `--ca-out` file it recorded); the next `flp up` generates a new one |

The state file also records the instance, private and floating IPs, the URL
(`https://<private-ip>:8443`), the proxy version, the `--ca-out` path and the id of every
resource created. Treat it as a secret: it holds the CA private key.

## How install uses the FLP

With `bnk.mode: disconnected`, `render` and `install`:

1. Take the URL and CA from `flp.external` if `flp.external.url` is set, otherwise from
   `flp-outputs.json`. Without either they stop with
   `disconnected mode needs an F5 License Proxy: run `roksbnkargoctl flp up`, or set flp.external`.
2. Write Secret `f5-utils/licenseserver-rootca` with the CA under `licenseserver-rootca.txt`.
3. Add `<flp-ip>:8443` to the node-probe targets (TLS handshake, not verified) and require
   the CA Secret in `check pre-install`.
4. Pass the URL to the `check license` hook, which builds the `License` with
   `operationMode: f5licenseproxy`, the three `teem*Url` fields set to
   `<url>/license-proxy/v1` and `licenseProxyServerRootCaPath:
   /etc/cm20/licenseserver-rootca/licenseserver-rootca.txt`, after rolling CWC once so it
   trusts the CA.

The FLP is reached at its **private** IP over the transit gateway; the floating IP is only
for your own access. `flp status` shows the URL BNK uses; reachability from the nodes is
checked by `check pre-install` on every sync.

```text
Instance:   <ws>-flp (0717_…) running
URL:        https://10.248.0.4:8443   (what BNK uses)
Floating:   <floating-ip>
Version:    1.29.0-0.10.40
State:      /home/you/.roksbnkargoctl/<ws>/flp-outputs.json
Reachability from the ROKS nodes is checked by `check pre-install` on every sync.
```

A `CA:` line follows `State:` when the CA certificate was written to a file (`--ca-out`).

A disconnected install on a live cluster reached `License` state `Active` through an FLP at
a private IP on the transit gateway.

## Without a workspace

Every `flp` command runs without a workspace: with `--no-workspace`, or when no workspace
is selected. Pass `--name`, and the settings as flags or `ROKSBNKARGOCTL_*` variables:

```sh
export IBMCLOUD_API_KEY=…
roksbnkargoctl flp up --no-workspace --name edge1 \
  --region us-south --transit-gateway my-tgw --cidr 10.248.0.0/28 \
  --far-auth-file f5-far-auth-key.tgz --jwt-file subscription.jwt
```

| File | Default without a workspace | With a workspace |
|---|---|---|
| State (`--state`): resource ids, URL, CA and CA key, mode 0600 | `./<name>-flp.json` | `<workspace>/flp-outputs.json` |
| CA certificate (`--ca-out`), mode 0600 | `./<name>-flp-ca.pem` | none, unless `--ca-out` is given: `install` reads the CA from the state |

Without a workspace, `--name` is required (`flp status` and `flp down` also accept
`--state` alone). Keep the state file: it holds the CA private key, a re-run of `flp up`
reuses that CA, and `flp down` deletes what it records.

When `flp up` writes the CA certificate to a file, it ends by printing, on standard output,
the settings that point an install at this FLP, with the CA file's absolute path:

```text
# To install against this license proxy (bnk.mode: disconnected), set in config.yaml:
flp:
  external:
    url: https://10.248.0.4:8443
    root_ca_file: /home/you/edge1-flp-ca.pem
# or in the environment:
export ROKSBNKARGOCTL_FLP_EXTERNAL_URL='https://10.248.0.4:8443'
export ROKSBNKARGOCTL_FLP_EXTERNAL_ROOT_CA_FILE='/home/you/edge1-flp-ca.pem'
```

On Windows the last two lines are PowerShell, `$env:ROKSBNKARGOCTL_FLP_EXTERNAL_URL = '…'`,
with the value quoted literally. An install then uses the FLP as an existing one, through
`flp.external`.

## Teardown

`flp down` reads the state file and deletes, in order, waiting for each: the transit
gateway connection (if `flp up` created it), the instance, the floating IP, the subnet,
the security group, the public gateway (if created) and the VPC (if created, and only if
everything before it succeeded). Then it removes the state file and the CA file it
recorded. It is safe to re-run. If a disconnected install still licenses through this FLP,
uninstall it first.

The state file records the transit gateway connection whenever `flp up` created it: for
the VPC it created, and also for a VPC you gave it with `flp.vsi.vpc` that was not yet on
the gateway. A connection that VPC already had is never recorded, so `flp down` leaves it
attached. IBM Cloud can hold a connection in `deleting` for more than 10 minutes, so
`flp down` waits up to 30 minutes for the detach; every other deletion waits up to 10.

### When the state file is lost

If the state file does not exist and you did not name one with `--state`, `flp down`
looks the resources up by the names `flp up` gives them:

```sh
roksbnkargoctl flp down --no-workspace --name edge1 --region us-south --transit-gateway my-tgw
```

```text
⚠ edge1-flp.json does not exist; looking for the resources `flp up` names edge1-flp*
Found, by exact name:
  …
Delete these <n> resources? [y/N]:
```

It matches only the exact names (`<name>-flp-vpc`, `-subnet`, `-pgw`, `-sg`, `-fip`, the
instance `<name>-flp` and the transit gateway connection `<name>-flp`), never a prefix or a
near miss, in `--region` (required) and the zone (`--zone`, default `<region>-1`). The
connection is searched on `--transit-gateway`, or on every gateway in the account when
none is set. With `--vpc`, the FLP was built in your VPC: its subnet, security group,
floating IP and instance are searched there, and so is a gateway connection named
`<name>-flp` for that VPC, which is the one `flp up` creates; the VPC itself, not being
the FLP's, is never taken. It lists what it found and asks before deleting;
nothing found is success (`nothing named <name>-flp* in <region>`).

A `--state` you named that does not exist is an error instead, since the file may simply
be elsewhere.

## See also

- [Connected and disconnected modes](./11-modes.md)
- [install](./08-install.md)
- [The COS supply chain](./14-cos.md) for the FAR key and JWT
- [Appendix A. config.yaml reference](./appendix-a-config.md)
