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
with the `ROKSBNKARGOCTL_*` variables applied and every default filled in. Standard error
says so, and lists each override in force, one line per key:

```text
# workspace demo: /home/you/.roksbnkargoctl/demo/config.yaml, with overrides and defaults applied (not the file)
# registry.source set by ROKSBNKARGOCTL_REGISTRY_SOURCE
```

Use it to see what an override changes before you run the command it is meant for. Its
output is **not** `config.yaml`: overrides are never saved, and the defaults it shows are
not in your file. To copy a workspace to another host, use plain `show`.

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
7. Argo CD, starting with `Use an existing Argo CD instance (3.3 or later)?` (below),
   then, in either case, the endpoint Argo CD uses to reach ROKS (`private` or `public`);
8. the Git repository, branch and path; then a username for an `https://` URL, or an SSH
   key file and optional known_hosts file otherwise.

If the API key is not set, the interview continues without account lookups and asks for
names instead.

#### Use an existing Argo CD instance?

The Argo CD questions start with:

```text
Use an existing Argo CD instance (3.3 or later)? (y/n) [y]:
```

The default is `y`, unless the workspace is already waiting for a test hub (below).

- **Yes**: `init` asks for the Argo CD server URL (`https://…`), whether to skip TLS
  verification (a self-signed Argo CD), and the Argo CD project, and says that it checks
  `$ARGOCD_AUTH_TOKEN` against the server next. After the interview it requires the API
  token in `ARGOCD_AUTH_TOKEN` (or the variable `argocd.token_env` names) and checks it
  against the server, as `install` does (see [the checks](#the-checks-init-makes-before-it-saves)).
- **No**: you want a test hub built for you. `init` says that `roksbnkargoctl argocd up`
  builds a test Argo CD on a VSI after `init` and is not for production, then asks for
  `test_hub.cidr` (default `10.248.1.0/28`; a free /24 to /28 on the transit gateway) and
  `test_hub.allowed_cidr` (who may reach the hub's UI and API, default `0.0.0.0/0`; give
  your own address). It sets `argocd.server` to the placeholder
  `https://argocd.placeholder.invalid` and `argocd.insecure` to `true`; `.invalid` never
  resolves, so nothing can reach the placeholder by mistake. `init` does not check Argo CD
  for such a workspace and reminds you to run `roksbnkargoctl argocd up` before `install`;
  `argocd up` builds the hub and replaces the placeholder with the hub's URL
  ([chapter 15](./15-test-hub.md)).

Running `init` again on an existing workspace resumes the interview with the saved
answers as the defaults.

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

When `argocd.server` in the file names a real Argo CD, `init --config-file` makes the same
checks as the interview ([below](#the-checks-init-makes-before-it-saves)), so set
`ARGOCD_AUTH_TOKEN` and the Git credential before you run it. To defer Argo CD to a test
hub, set `argocd.server: https://argocd.placeholder.invalid` (any host ending in
`.invalid` is treated as the placeholder).

Validation also rejects a `bnk.version` other than `2.4.0`, `bnk.tmm_replicas` below 1,
a `registry.source` other than `far` or `mirror`, `registry.source: mirror` without
`registry.mirror.host` (or with a scheme in it), `flp.external.url` without
`flp.external.root_ca_file`, an `argocd.cluster_endpoint` other than `private` or
`public`, and a `git.path` that is absolute or contains `..`.

### The checks init makes before it saves

After validation and before it looks anything up in IBM Cloud or writes `config.yaml`,
`init` checks the Argo CD and the Git repository that `install` will use. If a check fails,
nothing is saved.

**Argo CD** (skipped for the test-hub placeholder):

1. The token must be set. Without it:

   ```text
   roksbnkargoctl: Argo CD API token: set the environment variable ARGOCD_AUTH_TOKEN: an API token for the Argo CD at https://… (Argo CD UI: Settings → Accounts → <account> → Tokens → Generate New)
   ```

2. The server must be 3.3 or later, and `GET /api/v1/session/userinfo` must report
   `loggedIn` for the token: the same check as `install` step 1. A failure is reported as
   `checking the Argo CD at <server>: …` with the reason ([install](./08-install.md#1-argo-cd-33-or-later-and-a-token-it-accepts)).
   On success: `✓ Argo CD <version> at <server> accepts the token in $ARGOCD_AUTH_TOKEN`.

**Git**, with the credential `install` uses (`ROKSBNKARGOCTL_GIT_TOKEN` or the variable
`git.token_env` names, or `git.ssh_key_file`):

| Finding | Result |
|---|---|
| The repository cannot be read (not found, no access, unreachable) | Error; nothing is saved |
| It can be read but the credential may not push | Warning: `… install will fail at the push; fix the credential, or commit `roksbnkargoctl export` yourself and run install --no-publish` |
| No credential is set | Warning: `no Git credential ($ROKSBNKARGOCTL_GIT_TOKEN or git.ssh_key_file): install needs one to push, unless you commit `roksbnkargoctl export` yourself and run install --no-publish`. `init` then tries to read the repository anonymously, and only warns if it cannot, since Argo CD may have its own credential for it |
| It can be read and pushed to | `✓ Git <url>: readable, branch <branch> present`, then `✓ Git <url>: the credential can push` |

Push rights are only a warning at `init` because the [export and
`install --no-publish`](./08-install.md#without-letting-roksbnkargoctl-push-to-git) flow
never pushes. Push access is proven without pushing anything: `init` asks the Git server
for the ref list a push starts with (the `git-receive-pack` advertisement), which Git hosts
serve only to a credential that may push. The error messages are listed in the
[troubleshooting guide](./17-troubleshooting.md#before-the-sync-init-render-install).

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
| Git token (HTTPS) | `ROKSBNKARGOCTL_GIT_TOKEN` | `git.token_env` | `init` (checked; a warning when unset), `install` (optional with `--no-publish`), `uninstall --purge-git` |
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
