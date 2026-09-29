# Workspaces and init

A workspace is one install: one cluster, one Application, one directory. This chapter
shows you where workspaces live, what is in one, how the tool decides which workspace a
command acts on, and how `init` creates and refreshes a workspace, by interview or from a
`config.yaml`.

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
7. the Argo CD server, whether to skip TLS verification, the project, and which cluster
   endpoint Argo CD uses (`private` or `public`);
8. the Git repository, branch and path; then a username for an `https://` URL, or an SSH
   key file and optional known_hosts file otherwise.

If the API key is not set, the interview continues without account lookups and asks for
names instead. Running `init` again on an existing workspace resumes the interview with
the saved answers as the defaults.

The interview covers the common settings. Some keys, such as `cos.local_far_auth_file`,
`cos.local_jwt_file`, `argocd.ca_file`, `argocd.application`, `bnk.cert_manager` or
`check.image`, are only set through a config file; [Appendix A](./appendix-a-config.md)
lists every key.

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
| Argo CD API token | `ARGOCD_AUTH_TOKEN` | `argocd.token_env` | `install`, `uninstall`, `status`, `diagnose` |
| Git token (HTTPS) | `ROKSBNKARGOCTL_GIT_TOKEN` | `git.token_env` | `install`, `uninstall --purge-git` |
| Mirror password | `ROKSBNKARGOCTL_MIRROR_PASSWORD` | `registry.mirror.password_env` | `render`, `install`, `registry` with a mirror username |

A missing variable produces an error that names the variable, never a value, for example
`Argo CD API token: set the environment variable ARGOCD_AUTH_TOKEN`. With
`git.ssh_key_file` set, no Git token is needed.

Other environment variables:

| Variable | Effect |
|---|---|
| `ROKSBNKARGOCTL_HOME` | The home directory for all workspaces |
| `ROKSBNKARGOCTL_WORKSPACE` | The workspace to use when `-w` is not given |
| `ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1` | Reach ROKS from the operator host through its private endpoint |
| `ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1` | Do not verify the Git server's SSH host key (warned; prefer `git.known_hosts_file`) |

## See also

- [Quick start: a connected install](./06-quick-start.md)
- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Appendix B: Command reference](./appendix-b-commands.md)
