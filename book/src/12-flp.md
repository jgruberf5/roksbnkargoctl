# The F5 License Proxy (flp)

In disconnected mode BNK licenses through an F5 License Proxy (FLP) instead of reaching F5
directly. This chapter describes `roksbnkargoctl flp up`, `flp status` and `flp down`: what
they build in IBM Cloud, what runs on the VSI, how its CA is handled, and how `install`
connects BNK to it. After reading it you will be able to stand up an FLP, point an install
at it (or at one you already run), and remove it.

## Commands

| Command | Does |
|---|---|
| `roksbnkargoctl flp up` | Builds the FLP VSI and its network, records everything in `flp-outputs.json` |
| `roksbnkargoctl flp status` | Shows the instance, its status, the URL BNK uses, the floating IP and the proxy version |
| `roksbnkargoctl flp down [-y]` | Deletes exactly what `flp up` recorded, then removes `flp-outputs.json` |

None of them take command-specific flags. `flp down` asks for confirmation; pass `--yes`
when there is no terminal.

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

`flp up` also needs the FAR auth key and the subscription JWT (from COS or the local files
in `cos.*`), because the VSI logs in to FAR to pull the proxy images and the proxy licenses
with the JWT.

## What `flp up` builds

All names derive from `<workspace>-flp`. Each piece is found by name before it is created,
so a re-run after a failure reuses what exists.

| Resource | Name | Details |
|---|---|---|
| VPC | `<ws>-flp-vpc` | Created with manual address prefixes, unless `flp.vsi.vpc` is set. Before creating it, `flp up` checks `flp.vsi.cidr` against every VPC on the transit gateway and refuses an overlap |
| Address prefix | `<ws>-flp-prefix` | `flp.vsi.cidr` in the zone (only in a VPC it created) |
| Public gateway | `<ws>-flp-pgw` | Egress for packages, FAR image pulls and F5 licensing |
| Subnet | `<ws>-flp-subnet` | `flp.vsi.cidr`, attached to the public gateway |
| Security group | `<ws>-flp-sg` | Outbound: all. Inbound TCP `8443` from each `allowed_cidrs` entry; TCP `22` from each entry when `ssh_key` is set |
| Floating IP | `<ws>-flp-fip` | Unless `floating_ip: false`. Reserved before the VSI so its address can go into the proxy certificate |
| Instance | `<ws>-flp` | Latest public `ibm-ubuntu-24-04-*-minimal-amd64-*` image, 100 GB boot volume, cloud-init user data |
| Transit gateway connection | `<ws>-flp` | Attaches the FLP VPC to the cluster's transit gateway so the ROKS workers reach the FLP privately |

If a step fails, the partial record is still written and `flp up` ends with
`(partial state saved; `flp down` cleans it up)`. On success it prints the URL and notes
that the pod takes about 5 minutes to come up.

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
| Stored in | `<workspace>/flp-outputs.json` (`root_ca_pem`, `root_ca_key_pem`), mode 0600 |
| On re-run | Reused (`keeping the CA from the previous flp up`), because BNK may already trust it |
| After `flp down` | Gone with `flp-outputs.json`; the next `flp up` generates a new one |

`flp-outputs.json` also records the instance, private and floating IPs, the URL
(`https://<private-ip>:8443`), the proxy version and the id of every resource created. Treat
it as a secret: it holds the CA private key.

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
Reachability from the ROKS nodes is checked by `check pre-install` on every sync.
```

A disconnected install on a live cluster reached `License` state `Active` through an FLP at
a private IP on the transit gateway.

## Teardown

`flp down` reads `flp-outputs.json` and deletes, in order, waiting for each: the transit
gateway connection (only if `flp up` created the VPC), the instance, the floating IP, the
subnet, the security group, the public gateway (if created) and the VPC (if created, and
only if everything before it succeeded). It is safe to re-run. If a disconnected install still licenses
through this FLP, uninstall it first.

## See also

- [Connected and disconnected modes](./11-modes.md)
- [install](./08-install.md)
- [The COS supply chain](./14-cos.md) for the FAR key and JWT
- [Appendix A. config.yaml reference](./appendix-a-config.md)
