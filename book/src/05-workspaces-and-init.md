# Workspaces and init

A workspace is one install: one cluster, one Application, one directory. This chapter
shows you where workspaces live, what is in one, how the tool decides which workspace a
command acts on, how `init` creates and refreshes a workspace, by interview or from a
`config.yaml`, how to override a setting for one run, and which commands run without a
workspace at all.

## Where workspaces live

Every workspace is a directory under the **home**, `~/.roksbnkargoctl/` by default
(`%USERPROFILE%\.roksbnkargoctl\` on Windows). Set `ROKSBNKARGOCTL_HOME` to put the home
somewhere else.

```text
~/.roksbnkargoctl/
  current                          name of the current workspace
  <workspace>/
    config.yaml                    the contract: your settings + the resolved: section
    manifests/
      git/NNN-<kind>-<ns>-<name>.yaml     exactly what is published to Git
      direct/NNN-<kind>-<ns>-<name>.yaml  what install writes into ROKS (Secrets REDACTED)
      application.yaml                    the Argo CD Application
    diagnostics/
      <time>/                      bundles written by `diagnose` (start with summary.md)
      uninstall-<time>/            uninstall check logs captured by `uninstall`
    flp-outputs.json               the license proxy built by `flp up` (disconnected mode)
    argocd-hub.json                the test hub built by `argocd up`
    registry-mirror.json           the last `registry replicate`
```

Only `config.yaml` is created by `init`; the rest appears as you run the commands that
produce it. `render` and `install` replace `manifests/` wholesale, so an object that is
no longer rendered does not linger. `agent init` adds `AGENTS.md`, `personas/` and
`journal/` (see [chapter 18](./18-agent.md)).

The workspace directory is created owner-only (`0700`) and `config.yaml` is written
owner-only (`0600`), atomically.

### Workspace names

A workspace name is 1 to 40 characters: lowercase letters, digits and `-`, starting and
ending with a letter or digit. The name is used in resource names, which is why it is
restricted:

| Default derived from the workspace name | Value |
|---|---|
| `argocd.application` | `bnk-<workspace>` |
| `git.path` | `bnk/<workspace>` |
| `test_hub.name` | `<workspace>-argocd` |

## Choosing the workspace a command acts on

Each command resolves the workspace in this order:

1. `-w <name>` / `--workspace <name>`;
2. the `ROKSBNKARGOCTL_WORKSPACE` environment variable;
3. the **current** workspace, recorded in `<home>/current`.

If none is set, the command stops:

```text
roksbnkargoctl: no workspace selected: pass -w <name> or run `roksbnkargoctl init -w <name>`
```

`init` makes the workspace it saves current. To switch:

```sh
roksbnkargoctl workspaces list      # the current one is marked with *
roksbnkargoctl workspaces use demo
```

`ws` is an alias for `workspaces`. `workspaces list` shows every directory under the home
that contains a `config.yaml`.

## Overriding a setting for one run

`config.yaml` is where settings are kept, but it is not the only place they come from.
Every key except `bnk.version` can be overridden for one command run by an environment
variable named after it, and many commands also bind flags to the keys they use:

```text
flag  >  ROKSBNKARGOCTL_* variable  >  config.yaml  >  default
```

```sh
# this run only: install from the mirror, whatever config.yaml says
ROKSBNKARGOCTL_REGISTRY_SOURCE=mirror roksbnkargoctl render
```

The variable is `ROKSBNKARGOCTL_` followed by the key path upper-cased, with `.` as `_`:
`registry.mirror.host` is `ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST`. An override is never
written into `config.yaml`. [Appendix A](./appendix-a-config.md#overrides-flags-and-environment-variables)
lists the variable for every key and the rules (empty means unset, how lists and
booleans are written, and why `bnk.version` has none).

Because `init` records the `resolved:` facts for the settings in force when it ran, an
override of `cluster`, `transit_gateway` or `argocd.cluster_endpoint` that does not match
the recorded cluster, gateway or endpoint is refused by the commands that use them:

```text
roksbnkargoctl: ROKSBNKARGOCTL_CLUSTER overrides cluster to "other-roks", but workspace "demo" was resolved for my-roks; unset it, or run `roksbnkargoctl init -w demo --refresh` with it set
```

## Running without a workspace

Some commands do not need a workspace: they take every setting they read as a flag or a
`ROKSBNKARGOCTL_*` variable. The global `--no-workspace` flag makes that explicit: the
command ignores `ROKSBNKARGOCTL_WORKSPACE` and the current workspace, and its settings
come from flags, variables and defaults only.

| Command | Without a workspace |
|---|---|
| `cos instances`, `cos buckets`, `cos list`, `cos publish`, `cos delete`, `cos verify` | Yes ([chapter 14](./14-cos.md)) |
| `flp up`, `flp status`, `flp down` | Yes, with `--name` ([chapter 12](./12-flp.md#without-a-workspace)) |
| `registry bom`, `registry replicate`, `registry verify` | Yes ([chapter 13](./13-registry.md#without-a-workspace)) |
| `forge register`, `forge unregister` | Yes, with `--cluster` and `--region` ([BNK Forge](./15a-forge.md)) |
| `init`, `render`, `install`, `status`, `diagnose`, `uninstall`, `export`, `show`, `argocd *`, `agent *` | No |
| `version`, `self *`, `workspaces *` | Not applicable: they read no workspace settings |

These commands still use a workspace when one is selected (`-w`,
`ROKSBNKARGOCTL_WORKSPACE`, or the current workspace): its settings are the defaults, and
flags and variables override them. Without `--no-workspace`, and with no workspace
selected, they run without one. `--no-workspace` and `-w` together are an error.

A command that needs a workspace refuses `--no-workspace`:

```text
roksbnkargoctl: this command needs a workspace: drop --no-workspace and pass -w <name>
```

and `init` says `init creates a workspace: drop --no-workspace`.

## show

```sh
roksbnkargoctl show                 # config.yaml, verbatim
roksbnkargoctl show > config.yaml   # stdout is the file alone
roksbnkargoctl show --effective     # the settings in force after overrides
```

`show` prints the workspace's `config.yaml` exactly as it is on disk, on standard output:
the settings you gave `init` and the `resolved:` section `init` and `install` filled in.
The workspace and the file's path go to standard error, as
`# workspace <name>: <path>`, so redirecting standard output gives you the file alone,
ready for `init --config-file` on another host. It holds no secret values, only the names
of the environment variables that carry them.

`show --effective` prints instead the settings a command would use now: `config.yaml`
with the `ROKSBNKARGOCTL_*` variables and the defaults applied. Use it to see what an
override changes before you run the command it is meant for.

## init

```sh
roksbnkargoctl init -w demo                        # interview
roksbnkargoctl init -w demo --config-file config.yaml   # from a file (or -f)
roksbnkargoctl init -w demo --refresh              # re-resolve the saved config
```

| Flag | Effect |
|---|---|
| `-f`, `--config-file <path-or-url>` | Read `config.yaml` from a file or an `http(s)://` URL and ask nothing. |
| `--refresh` | Re-resolve the saved config without asking. Fails if the workspace has no config. |

Without `-w` and without `ROKSBNKARGOCTL_WORKSPACE`, `init` asks for a workspace name
(default `bnk`).

### The interview

With no `--config-file`, `init` asks for each setting an operator must supply, and offers
choices from your account where it can list them: the clusters in the region and the
transit gateways. Press Enter to accept the value in brackets. In order:

1. IBM Cloud region of the cluster (default `us-east`) and resource group;
2. the cluster and the transit gateway;
3. the mode (`connected` or `disconnected`); for `disconnected`, whether to use an
   existing license proxy (then its URL and root CA file) instead of `flp up`;
4. the image source (`far` or `mirror`); for `mirror`, its host, repository prefix,
   username and CA file;
5. TMM replicas and the StorageClass;
6. the COS instance, bucket, bucket region and the two object names;
7. Argo CD, starting with `Use an existing Argo CD? [Y/n]` (below);
8. the Git repository, branch and path; then a username for an `https://` URL, or an SSH
   key file and optional known_hosts file otherwise.

If the API key is not set, the interview continues without account lookups and asks for
names instead.

#### Use an existing Argo CD?

The Argo CD questions start with `Use an existing Argo CD? [Y/n]`.

- **Yes** (the default): `init` asks for the Argo CD server URL, then whether to skip TLS
  verification, the project, and which cluster endpoint Argo CD uses (`private` or
  `public`). It then requires the API token in `ARGOCD_AUTH_TOKEN` (or the variable
  `argocd.token_env` names) and checks it against the server during `init`, as `install`
  does: the server must be 3.3 or later, and `GET /api/v1/session/userinfo` must report
  `loggedIn`. A wrong URL or token fails now rather than at `install`.
- **No**: you want a test hub built for you. `init` asks for `test_hub.cidr` (a /24 to
  /28 not used by any VPC on the transit gateway) and `test_hub.allowed_cidr` (the source
  allowed to reach the hub), and points you at `roksbnkargoctl argocd up`, which builds
  the hub and sets `argocd.server` ([chapter 15](./15-test-hub.md)). Running `init` again on an existing workspace resumes the interview with
the saved answers as the defaults.

The interview covers the common settings. Some keys, such as `cos.local_far_auth_file`,
`cos.local_jwt_file`, `argocd.ca_file`, `argocd.application`, `bnk.cert_manager` or
`check.image`, are only set through a config file (or overridden for a run, below);
[Appendix A](./appendix-a-config.md) lists every key.

### From a config file

`--config-file` is the repeatable, reviewable way: keep the file in version control
(it holds no secrets) and run `init` from it.

Parsing is **strict**. An unknown key is an error rather than being ignored, because a
typo in an optional key would otherwise silently install the default:

```text
roksbnkargoctl: config.yaml: yaml: unmarshal errors:
  line 4: field deployment_size not found in type config.BNK
```

The file replaces the workspace's configuration, including any previous `resolved:`
section, which `init` then rebuilds. Defaults are filled in and the whole config is
validated before anything is looked up. Every problem is reported at once:

```text
roksbnkargoctl: invalid config:
  - cluster (name or id of the existing ROKS cluster) is required
  - transit_gateway (name or id of the existing transit gateway) is required
  - bnk.mode "offline": must be "connected" or "disconnected"
  - cos.instance and cos.bucket are required unless cos.local_far_auth_file is set
  - argocd.server (https URL of the existing Argo CD) is required
  - git.url (the repo Argo CD syncs from) is required
```

When `argocd.server` in the file names an Argo CD, `init --config-file` checks the token
against it the same way the interview does (version 3.3 or later, `loggedIn`), so set
`ARGOCD_AUTH_TOKEN` before you run it.

Validation also rejects a `bnk.version` other than `2.4.0`, `bnk.tmm_replicas` below 1,
a `registry.source` other than `far` or `mirror`, `registry.source: mirror` without
`registry.mirror.host` (or with a scheme in it), `flp.external.url` without
`flp.external.root_ca_file`, an `argocd.cluster_endpoint` other than `private` or
`public`, and a `git.path` that is absolute or contains `..`.

### What init resolves and records

After validation, `init` looks everything up in IBM Cloud, so that a typo fails now and
not in the middle of an install. The results go into the `resolved:` section of
`config.yaml`, and `render`, `install` and `uninstall` work from those recorded facts.

| Looked up | Checked | Recorded in `resolved:` |
|---|---|---|
| The cluster, by name or ID | It is OpenShift; it is in `ibmcloud.region`; it is in exactly one VPC. Fewer than 3 worker zones is a warning. | `cluster_id`, `cluster_name`, `cluster_crn`, `cluster_version`, `private_endpoint`, `public_endpoint`, `zones`, `resource_group_id` |
| The cluster's VPC | - | `vpc_id`, `vpc_name`, `vpc_crn` |
| The transit gateway, by name or ID | Whether the cluster VPC is attached. If not, a warning: `install` will attach it. | `transit_gateway_id`, `transit_gateway_name`, `cluster_attached_to_tgw` |
| The endpoint Argo CD will use | The cluster has the endpoint `argocd.cluster_endpoint` selects | `argocd_cluster_server` |
| The COS instance and bucket (unless `cos.local_far_auth_file` is set) | Both `cos.far_auth_object` and `cos.jwt_object` exist in the bucket | `cos_instance_crn` |

`install` adds to the same section: `trusted_profile_id`, `last_published_commit`,
`node_resolver_image` (private-CA mirror only) and `tgw_connection_created_id` (only when
it attached the VPC itself, so that `uninstall --detach-tgw` removes only what `install`
added). Re-running `init` on an existing workspace, by interview or `--refresh`, keeps
`trusted_profile_id` and `last_published_commit` and re-resolves everything else;
`init --config-file` starts the section afresh.

A command that needs the resolved facts on a workspace that has none stops:

```text
roksbnkargoctl: workspace "<name>" is not resolved; run `roksbnkargoctl init -w <name> --refresh`
```

Run `init --refresh` whenever the cluster, gateway or bucket may have changed.

## Secrets come from the environment

No secret is asked for by `init` or stored in the workspace. Each command reads the
secrets it needs from environment variables, whose names you can change in the config:

| Secret | Default variable | Config key that renames it | Needed by |
|---|---|---|---|
| IBM Cloud API key | `IBMCLOUD_API_KEY` (then `IC_API_KEY`) | `ibmcloud.api_key_env` | every command that calls IBM Cloud |
| Argo CD API token | `ARGOCD_AUTH_TOKEN` | `argocd.token_env` | `init` (existing Argo CD), `install`, `uninstall`, `status`, `diagnose` |
| Git token (HTTPS) | `ROKSBNKARGOCTL_GIT_TOKEN` | `git.token_env` | `install`, `uninstall --purge-git` |
| Mirror password | `ROKSBNKARGOCTL_MIRROR_PASSWORD` | `registry.mirror.password_env` | `render`, `install`, `registry` with a mirror username |

A missing variable produces an error that names the variable, never a value, for example
`Argo CD API token: set the environment variable ARGOCD_AUTH_TOKEN`. With
`git.ssh_key_file` set, no Git token is needed.

Other environment variables:

| Variable | Effect |
|---|---|
| `ROKSBNKARGOCTL_HOME` | The home directory for all workspaces |
| `ROKSBNKARGOCTL_WORKSPACE` | The workspace to use when `-w` is not given (ignored with `--no-workspace`) |
| `ROKSBNKARGOCTL_<KEY>` | Overrides one `config.yaml` key for the run ([Appendix A](./appendix-a-config.md#overrides-flags-and-environment-variables)) |
| `ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1` | Reach ROKS from the operator host through its private endpoint |
| `ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1` | Do not verify the Git server's SSH host key (warned; prefer `git.known_hosts_file`) |

## See also

- [Quick start: a connected install](./06-quick-start.md)
- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Appendix B: Command reference](./appendix-b-commands.md)
