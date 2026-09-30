# A. config.yaml reference

Each workspace has one `config.yaml` at `~/.roksbnkargoctl/<workspace>/config.yaml`
(or under `$ROKSBNKARGOCTL_HOME`). This appendix lists every key it accepts, with
its type, default and meaning, then gives a complete annotated example for a
connected install from FAR and the changes for disconnected and mirror installs.
Use it when writing a config for `init --config-file`, or when reading one `init`
wrote for you. The **Override** column names the environment variable that overrides
each key for one command run (below).

Three rules apply to the whole file:

- **Parsing is strict.** An unknown or misspelt key is an error, because a typo in
  an optional key would otherwise silently install the default. There is, for
  example, no `deployment_size` key: `Tiny` is fixed (see
  [Appendix C](./appendix-c-sizing.md)), and writing one is rejected.
- **No secret values.** Keys ending in `_env` name an environment variable that
  holds the secret; keys ending in `_file` name a local file. The config never
  contains a key, token, password or JWT.
- **Validation runs before `init` saves the workspace, and before `render` and
  `install`**, and lists every problem at once. `status`, and a resumed `init`
  interview, load a half-filled config.

## Overrides: flags and environment variables

Every key except `bnk.version` can also be set for a single run, without editing
`config.yaml`:

- **An environment variable**, named `ROKSBNKARGOCTL_` followed by the key path upper-cased,
  with `.` as `_`: `registry.mirror.host` is `ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST`, and
  `flp.vsi.allowed_cidrs` is `ROKSBNKARGOCTL_FLP_VSI_ALLOWED_CIDRS`. The tables below list
  every one.
- **A command flag**, on the commands that bind one. Each such flag's help ends with the
  key it overrides and its variable, for example
  `--mirror-host … (overrides registry.mirror.host; env ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST)`.

The order of precedence is:

```text
flag  >  ROKSBNKARGOCTL_* variable  >  config.yaml  >  default
```

| Rule | Detail |
|---|---|
| Never saved | An override applies to the run it is set for and is never written into `config.yaml`. `init` warns `<source>: <key> was used but not saved (config.yaml keeps its own value)` for each one it used |
| Empty is unset | A variable set to an empty value is ignored, so a variable cannot clear a key |
| Value syntax | Strings as written; integers in decimal; booleans as `true` or `false` (`1` and `0` also parse); lists comma-separated, with blanks trimmed and empty entries dropped |
| Bad values | A variable that does not parse stops the command, naming it: `ROKSBNKARGOCTL_BNK_TMM_REPLICAS (overrides bnk.tmm_replicas): "three" is not an integer`. Every bad variable is reported at once |
| Resolved facts | `init` records `resolved:` for the settings in force. A later command refuses a `cluster`, `transit_gateway` or `argocd.cluster_endpoint` override that does not match what was recorded, and tells you to unset it or run `init --refresh` with it set |
| `bnk.version` | Has no override. `ROKSBNKARGOCTL_BNK_VERSION` already means something else: the BNK release the one-line installers download a binary for ([chapter 4](./04-installation.md#installer-options)) |

Without a workspace (`--no-workspace`, or no workspace selected, on the commands that
allow it), there is no `config.yaml`: settings come from flags, variables and defaults
only. See [Workspaces and init](./05-workspaces-and-init.md#running-without-a-workspace).

## Top level

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `cluster` | string | **required** | `ROKSBNKARGOCTL_CLUSTER` | name or ID of the existing ROKS cluster |
| `transit_gateway` | string | **required** | `ROKSBNKARGOCTL_TRANSIT_GATEWAY` | name or ID of the existing transit gateway |
| `ibmcloud` | map | | | account context (below) |
| `bnk` | map | | | the install shape (below) |
| `registry` | map | | | where ROKS pulls BNK images from (below) |
| `cos` | map | | | where the FAR key and subscription JWT live (below) |
| `flp` | map | | | the F5 License Proxy, disconnected mode (below) |
| `argocd` | map | | | your existing Argo CD (below) |
| `git` | map | | | the Git repository Argo CD syncs from (below) |
| `check` | map | | | the check image (below) |
| `test_hub` | map | | | the test Argo CD hub built by `argocd up` (below) |
| `forge` | map | | | BNK Forge registration (below) |
| `resolved` | map | written by `init` | | cached lookups (below); do not edit |

## ibmcloud

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `region` | string | **required** | `ROKSBNKARGOCTL_IBMCLOUD_REGION` | IBM Cloud region of the cluster (`init` checks the cluster is in it) |
| `resource_group` | string | `default` | `ROKSBNKARGOCTL_IBMCLOUD_RESOURCE_GROUP` | resource group for what the tool creates |
| `api_key_env` | string | `IBMCLOUD_API_KEY` | `ROKSBNKARGOCTL_IBMCLOUD_API_KEY_ENV` | environment variable holding the API key; `IC_API_KEY` is used if this one is empty |

## bnk

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `version` | string | `2.4.0` | none | must be `2.4.0`: the tool installs BNK 2.4 GA only |
| `mode` | string | `connected` | `ROKSBNKARGOCTL_BNK_MODE` | `connected` (BNK licenses directly with F5) or `disconnected` (through an F5 License Proxy) |
| `tmm_replicas` | int | `3` | `ROKSBNKARGOCTL_BNK_TMM_REPLICAS` | TMM replicas; must be ≥ 1. One per node, spread across zones |
| `namespace` | string | `f5-bnk` | `ROKSBNKARGOCTL_BNK_NAMESPACE` | namespace of FLO and the CNEInstance |
| `utils_namespace` | string | `f5-utils` | `ROKSBNKARGOCTL_BNK_UTILS_NAMESPACE` | namespace of BNK's shared components (CWC, License) |
| `cert_manager.install` | bool | `true` | `ROKSBNKARGOCTL_BNK_CERT_MANAGER_INSTALL` | install the pinned cert-manager; `false` uses one already on the cluster |
| `cert_manager.version` | string | `v1.17.3` | `ROKSBNKARGOCTL_BNK_CERT_MANAGER_VERSION` | cert-manager chart version |
| `nad_address` | string | `10.10.1.1/24` | `ROKSBNKARGOCTL_BNK_NAD_ADDRESS` | static address on the `ens3` ipvlan NetworkAttachmentDefinition `ens3-ipvlan-l2` |
| `storage_class` | string | `""` | `ROKSBNKARGOCTL_BNK_STORAGE_CLASS` | StorageClass for BNK components that request one; empty uses the cluster default. Set on the CNEInstance as `storageClassName` |

## registry

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `source` | string | `far` | `ROKSBNKARGOCTL_REGISTRY_SOURCE` | `far` (F5 Artifact Registry) or `mirror` (a private registry filled by `registry replicate`) |
| `far_host` | string | `repo.f5.com` | `ROKSBNKARGOCTL_REGISTRY_FAR_HOST` | FAR's registry host |
| `mirror.host` | string | required with `source: mirror` | `ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST` | bare `host[:port]`, without a scheme |
| `mirror.prefix` | string | `""` | `ROKSBNKARGOCTL_REGISTRY_MIRROR_PREFIX` | path under the host the artifacts are copied to (`<host>/<prefix>/images/…`) |
| `mirror.username` | string | `""` | `ROKSBNKARGOCTL_REGISTRY_MIRROR_USERNAME` | registry user; when set, a `mirror-secret` pull secret is created |
| `mirror.password_env` | string | `ROKSBNKARGOCTL_MIRROR_PASSWORD` | `ROKSBNKARGOCTL_REGISTRY_MIRROR_PASSWORD_ENV` | environment variable holding the password |
| `mirror.ca_file` | string | `""` | `ROKSBNKARGOCTL_REGISTRY_MIRROR_CA_FILE` | PEM of the mirror's CA, if it is not publicly trusted. Installs the CA on every node (DaemonSet `registry-ca-trust`) |

The mirror credentials and CA are used whenever `mirror.host` is set, not only when
`source: mirror` — `registry replicate` fills the mirror while the install still
pulls from FAR.

## cos

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `instance` | string | `bnk-supply-chain` (unless `local_far_auth_file` is set) | `ROKSBNKARGOCTL_COS_INSTANCE` | COS service instance: its name, GUID or CRN |
| `bucket` | string | required unless `local_far_auth_file` is set | `ROKSBNKARGOCTL_COS_BUCKET` | bucket holding the two files: its name, or a bucket CRN (which also names the instance) |
| `region` | string | `us-south` | `ROKSBNKARGOCTL_COS_REGION` | the region a new bucket is created in (`cos publish --create-bucket`) and buckets are listed from. An existing bucket's own region is found from its location, so this need not match it |
| `far_auth_object` | string | `f5-far-auth-key.tgz` | `ROKSBNKARGOCTL_COS_FAR_AUTH_OBJECT` | object name of the FAR auth tarball |
| `jwt_object` | string | `subscription.jwt` | `ROKSBNKARGOCTL_COS_JWT_OBJECT` | object name of the subscription JWT |
| `local_far_auth_file` | string | `""` | `ROKSBNKARGOCTL_COS_LOCAL_FAR_AUTH_FILE` | read the FAR auth tarball from this local file instead of COS |
| `local_jwt_file` | string | `""` | `ROKSBNKARGOCTL_COS_LOCAL_JWT_FILE` | read the JWT from this local file instead of COS |

`cos.instance` and `cos.bucket` are required unless `cos.local_far_auth_file` is
set. The JWT is read from `local_jwt_file` when set, otherwise from COS, so if you
set only `local_far_auth_file`, also set `local_jwt_file` (or keep a bucket for the
JWT). See [The COS supply chain](./14-cos.md).

## flp

Used only in `bnk.mode: disconnected`. Either `flp up` builds a proxy from
`flp.vsi`, or you point at an existing one with `flp.external`.

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `vsi.zone` | string | `<ibmcloud.region>-1` | `ROKSBNKARGOCTL_FLP_VSI_ZONE` | zone of the proxy VSI |
| `vsi.vpc` | string | `""` | `ROKSBNKARGOCTL_FLP_VSI_VPC` | existing VPC name or ID; empty creates a VPC |
| `vsi.cidr` | string | required by `flp up` | `ROKSBNKARGOCTL_FLP_VSI_CIDR` | a /24–/28 not used by any VPC on the transit gateway |
| `vsi.profile` | string | `bx2-4x16` | `ROKSBNKARGOCTL_FLP_VSI_PROFILE` | VSI profile |
| `vsi.allowed_cidrs` | list | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | `ROKSBNKARGOCTL_FLP_VSI_ALLOWED_CIDRS` | sources allowed to reach `:8443` (and `:22` when `ssh_key` is set) |
| `vsi.ssh_key` | string | `""` | `ROKSBNKARGOCTL_FLP_VSI_SSH_KEY` | name of an existing VPC SSH key; opens port 22 to `allowed_cidrs` |
| `vsi.floating_ip` | bool | `true` | `ROKSBNKARGOCTL_FLP_VSI_FLOATING_IP` | give the VSI a floating IP |
| `external.url` | string | `""` | `ROKSBNKARGOCTL_FLP_EXTERNAL_URL` | base URL of an existing proxy, e.g. `https://<ip>:8443` |
| `external.root_ca_file` | string | required with `external.url` | `ROKSBNKARGOCTL_FLP_EXTERNAL_ROOT_CA_FILE` | PEM of that proxy's root CA |

See [The F5 License Proxy](./12-flp.md).

## argocd

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `server` | string | **required** | `ROKSBNKARGOCTL_ARGOCD_SERVER` | URL of your existing Argo CD; must start with `https://` (or `http://`) |
| `token_env` | string | `ARGOCD_AUTH_TOKEN` | `ROKSBNKARGOCTL_ARGOCD_TOKEN_ENV` | environment variable holding the API token |
| `insecure` | bool | `false` | `ROKSBNKARGOCTL_ARGOCD_INSECURE` | skip TLS verification of the Argo CD server (self-signed) |
| `ca_file` | string | `""` | `ROKSBNKARGOCTL_ARGOCD_CA_FILE` | PEM of the CA that signed the Argo CD server's certificate |
| `project` | string | `default` | `ROKSBNKARGOCTL_ARGOCD_PROJECT` | Argo CD project of the Application and the repository |
| `application` | string | `bnk-<workspace>` | `ROKSBNKARGOCTL_ARGOCD_APPLICATION` | Application name (created in namespace `argocd`) |
| `cluster_endpoint` | string | `private` | `ROKSBNKARGOCTL_ARGOCD_CLUSTER_ENDPOINT` | ROKS API endpoint Argo CD dials: `private` (over the transit gateway) or `public` |

## git

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `url` | string | **required** | `ROKSBNKARGOCTL_GIT_URL` | the repository Argo CD syncs from (HTTPS or SSH) |
| `branch` | string | `main` | `ROKSBNKARGOCTL_GIT_BRANCH` | branch published to |
| `path` | string | `bnk/<workspace>` | `ROKSBNKARGOCTL_GIT_PATH` | directory in the repository; must be relative and contain no `..` |
| `username` | string | `""` | `ROKSBNKARGOCTL_GIT_USERNAME` | HTTPS user (`git` when a token is set without a user); SSH user when the URL has none |
| `token_env` | string | `ROKSBNKARGOCTL_GIT_TOKEN` (unless `ssh_key_file` is set) | `ROKSBNKARGOCTL_GIT_TOKEN_ENV` | environment variable holding an HTTPS token |
| `ssh_key_file` | string | `""` | `ROKSBNKARGOCTL_GIT_SSH_KEY_FILE` | SSH private key for an SSH URL; used instead of a token |
| `known_hosts_file` | string | `""` | `ROKSBNKARGOCTL_GIT_KNOWN_HOSTS_FILE` | known_hosts entries for an SSH server other than github.com, gitlab.com and bitbucket.org (whose keys are built in); also added to Argo CD |
| `author_name` | string | `roksbnkargoctl` | `ROKSBNKARGOCTL_GIT_AUTHOR_NAME` | commit author |
| `author_email` | string | `roksbnkargoctl@users.noreply.github.com` | `ROKSBNKARGOCTL_GIT_AUTHOR_EMAIL` | commit author email |

## check

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `image` | string | `ghcr.io/jgruberf5/roksbnkargoctl-check:<this release>` (`:dev` for a non-release build) | `ROKSBNKARGOCTL_CHECK_IMAGE` | source check image. Whatever you give, the render pins it by digest; in mirror mode the digest must exist in the mirror |

## test_hub

Used only by `argocd up`, which builds a test Argo CD. See
[A test Argo CD hub](./15-test-hub.md).

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `region` | string | `ibmcloud.region` | `ROKSBNKARGOCTL_TEST_HUB_REGION` | region of the hub VSI |
| `zone` | string | `<region>-1` | `ROKSBNKARGOCTL_TEST_HUB_ZONE` | zone of the hub VSI |
| `cidr` | string | required by `argocd up` | `ROKSBNKARGOCTL_TEST_HUB_CIDR` | a /24–/28 not used by any VPC on the transit gateway |
| `profile` | string | `bx2-4x16` | `ROKSBNKARGOCTL_TEST_HUB_PROFILE` | VSI profile |
| `version` | string | `3.5.1` | `ROKSBNKARGOCTL_TEST_HUB_VERSION` | Argo CD version |
| `allowed_cidr` | string | `0.0.0.0/0` | `ROKSBNKARGOCTL_TEST_HUB_ALLOWED_CIDR` | source allowed to reach the node port (and `:22` with `ssh_key`) |
| `ssh_key` | string | `""` | `ROKSBNKARGOCTL_TEST_HUB_SSH_KEY` | name of an existing VPC SSH key |
| `node_port` | int | `30443` | `ROKSBNKARGOCTL_TEST_HUB_NODE_PORT` | port the Argo CD API/UI is exposed on, on the floating IP |
| `k3s_channel` | string | `stable` | `ROKSBNKARGOCTL_TEST_HUB_K3S_CHANNEL` | k3s release channel |
| `attach_to_tgw` | bool | `true` | `ROKSBNKARGOCTL_TEST_HUB_ATTACH_TO_TGW` | attach the hub's VPC to the transit gateway |
| `name` | string | `<workspace>-argocd` | `ROKSBNKARGOCTL_TEST_HUB_NAME` | resource name prefix |

## forge

Used only by `forge register` and `forge unregister`, which register the cluster with
BNK Forge. No default is written to `config.yaml`; the defaults below are applied when the
command runs. The password has no key: it comes from `BNK_FORGE_PASSWORD` or a prompt.
See [Registering with BNK Forge](./15a-forge.md).

| Key | Type | Default | Override | Meaning |
|---|---|---|---|---|
| `url` | string | `$BNK_FORGE_URL` | `ROKSBNKARGOCTL_FORGE_URL` | BNK Forge server URL; must start with `https://` (or `http://`) |
| `project` | string | the cluster name | `ROKSBNKARGOCTL_FORGE_PROJECT` | BNK Forge project the cluster is registered in |
| `username` | string | `$BNK_FORGE_USER` | `ROKSBNKARGOCTL_FORGE_USERNAME` | BNK Forge login |
| `endpoint` | string | `public` | `ROKSBNKARGOCTL_FORGE_ENDPOINT` | ROKS endpoint written into the kubeconfig given to BNK Forge: `public` or `private` |
| `insecure` | bool | `false` | `ROKSBNKARGOCTL_FORGE_INSECURE` | skip TLS verification of BNK Forge (the password and session token are then sent unauthenticated; prefer `ca_file`). Ignored when `ca_file` is set |
| `ca_file` | string | `""` | `ROKSBNKARGOCTL_FORGE_CA_FILE` | PEM of the CA BNK Forge's certificate must chain to |

A `forge.url` or `forge.username` in `config.yaml` is overridden by `BNK_FORGE_URL` or
`BNK_FORGE_USER`; a flag or a `ROKSBNKARGOCTL_FORGE_*` variable overrides both.

## resolved

Written by `init` (and `init --refresh`), and updated by `install`. It caches what
was looked up so later commands and the rendered manifests do not depend on a live
lookup. The render's run id ignores this section. Do not edit it by hand.

| Key | Meaning |
|---|---|
| `cluster_id`, `cluster_name`, `cluster_crn`, `cluster_version` | the ROKS cluster |
| `vpc_id`, `vpc_name`, `vpc_crn` | the cluster's VPC |
| `private_endpoint`, `public_endpoint` | the cluster's API endpoints |
| `zones` | the cluster's zones |
| `transit_gateway_id`, `transit_gateway_name` | the gateway |
| `cluster_attached_to_tgw` | whether the cluster VPC is attached |
| `tgw_connection_created_id` | set when `install` attached the VPC itself, so `uninstall --detach-tgw` removes only that |
| `cos_instance_crn`, `resource_group_id` | COS and resource group |
| `node_resolver_image` | the `openshift-dns/node-resolver` image (for `registry-ca-trust`) |
| `trusted_profile_id` | the IAM trusted profile `install` created |
| `argocd_cluster_server` | the server URL Argo CD registered the cluster under |
| `last_published_commit` | the last Git commit `install` published |

## Validation rules

`init`, `render` and `install` refuse a config that breaks any of these:

| Rule | Message names |
|---|---|
| `ibmcloud.region`, `cluster`, `transit_gateway`, `argocd.server`, `git.url` set | the key |
| `bnk.version` is `2.4.0` | "this tool installs BNK 2.4.0 GA only" |
| `bnk.mode` is `connected` or `disconnected` | `bnk.mode` |
| `flp.external.root_ca_file` set whenever `flp.external.url` is (disconnected) | `flp.external.root_ca_file` |
| `bnk.tmm_replicas` ≥ 1 | `bnk.tmm_replicas` |
| `registry.source` is `far` or `mirror`; with `mirror`, `registry.mirror.host` set and without `://` | the key |
| `cos.instance` and `cos.bucket` set, unless `cos.local_far_auth_file` is | `cos.instance and cos.bucket` |
| `argocd.server` starts with `https://` or `http://` | `argocd.server` |
| `argocd.cluster_endpoint` is `private` or `public` | `argocd.cluster_endpoint` |
| `git.path` relative, without `..` | `git.path` |

## Example: connected install from FAR

```yaml
# ~/.roksbnkargoctl/prod/config.yaml
ibmcloud:
  region: us-south                 # the cluster's region
  resource_group: default
  # api_key_env: IBMCLOUD_API_KEY  # default; the key itself is only ever in the environment

cluster: my-roks-cluster           # existing ROKS cluster (name or ID)
transit_gateway: my-tgw            # existing transit gateway (name or ID)

bnk:
  version: 2.4.0                   # the only supported value
  mode: connected                  # BNK licenses directly with F5
  tmm_replicas: 3                  # one TMM per node, spread across the three zones
  # namespace: f5-bnk
  # utils_namespace: f5-utils
  # storage_class: ""              # empty = the cluster's default StorageClass
  cert_manager:
    install: true                  # install the pinned cert-manager
    version: v1.17.3

registry:
  source: far                      # pull from the F5 Artifact Registry
  far_host: repo.f5.com

cos:
  instance: bnk-supply-chain       # COS instance holding the supply-chain files
  bucket: my-bnk-bucket
  region: us-south
  far_auth_object: f5-far-auth-key.tgz   # the GA FAR key from MyF5
  jwt_object: subscription.jwt

argocd:
  server: https://argocd.example.com     # your existing Argo CD (3.3 or later)
  # token_env: ARGOCD_AUTH_TOKEN
  project: default
  # application: bnk-prod                # default bnk-<workspace>
  cluster_endpoint: private              # Argo CD reaches ROKS over the transit gateway

git:
  url: https://github.com/example-org/platform-gitops.git
  branch: main
  path: bnk/prod                   # the directory the Application syncs
  username: example-bot
  # token_env: ROKSBNKARGOCTL_GIT_TOKEN
```

The environment for this config:

```sh
export IBMCLOUD_API_KEY=…
export ARGOCD_AUTH_TOKEN=…
export ROKSBNKARGOCTL_GIT_TOKEN=…
```

## Delta: disconnected (F5 License Proxy)

```yaml
bnk:
  mode: disconnected               # License.operationMode: f5licenseproxy

flp:
  vsi:                             # built by `roksbnkargoctl flp up`
    cidr: 10.248.0.0/28            # unused on the transit gateway
    # zone: us-south-1
    # profile: bx2-4x16
    # allowed_cidrs: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16]
  # or, for a proxy you already run:
  # external:
  #   url: https://10.0.0.5:8443
  #   root_ca_file: /path/to/flp-root-ca.pem
```

Run `roksbnkargoctl flp up` before `install` (unless you use `flp.external`).
Disconnected mode is usually paired with a mirror, below, so the workers need no
Internet egress at all.

## Delta: private registry (mirror)

```yaml
registry:
  source: mirror
  mirror:
    host: registry.example.internal:443   # bare host[:port], no scheme
    prefix: bnk                           # artifacts land under <host>/bnk/…
    username: bnk-pull
    # password_env: ROKSBNKARGOCTL_MIRROR_PASSWORD
    ca_file: /path/to/mirror-ca.pem       # only if the mirror's CA is private
```

```sh
export ROKSBNKARGOCTL_MIRROR_PASSWORD=…
roksbnkargoctl registry replicate     # fill the mirror (FAR key still needed for this)
roksbnkargoctl registry verify        # nothing missing
```

See [Mirroring into a private registry](./13-registry.md).

## See also

- [Workspaces and init](./05-workspaces-and-init.md)
- [Appendix B: command reference](./appendix-b-commands.md)
- [Appendix D: security model](./appendix-d-security.md)
