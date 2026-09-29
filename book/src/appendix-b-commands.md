# B. Command reference

This appendix lists every roksbnkargoctl command and subcommand with its flags and
purpose, the global flags, and every environment variable the tool reads. It is
taken from the tool's own `--help`; run `roksbnkargoctl <command> --help` for the
same text on your build.

## Global flags

Accepted by every command.

| Flag | Meaning |
|---|---|
| `-w`, `--workspace <name>` | workspace to act on (default: the current workspace) |
| `-y`, `--yes` | do not ask for confirmation (`uninstall`, `flp down`, `argocd down`) |
| `-v`, `--verbose` | verbose output |
| `-h`, `--help` | help for the command |

The workspace is chosen in this order: `-w`, then `$ROKSBNKARGOCTL_WORKSPACE`, then
the current workspace recorded by `workspaces use` (the file `current` under the
workspace home). Workspace names are 1–40 lowercase letters, digits and `-`,
starting and ending alphanumeric.

Commands that ask for confirmation refuse when there is no terminal to ask on and
`--yes` was not given: `uninstall` says `not confirmed (pass --yes to skip the prompt)`,
`flp down` and `argocd down` say `not confirmed (pass --yes)`.

## The lifecycle

| Command | Flags | Purpose |
|---|---|---|
| `init` | `-f`, `--config-file <path or URL>` — read `config.yaml` from a path or an http(s) URL instead of interviewing; `--refresh` — re-resolve the saved config without asking | Create or update a workspace: interview (or read a config file) and resolve the cluster, VPC, transit gateway, COS objects, Argo CD and Git repo against IBM Cloud. Re-running resumes the interview with saved answers as defaults. Never asks for secrets |
| `render` | `--no-cluster` — do not read the cluster (Kubernetes version and node image fall back to defaults) | Pull the BNK manifest, FLO chart and cert-manager chart, template them in-process, and write `manifests/git/`, `manifests/direct/` (Secrets redacted) and `manifests/application.yaml`. Changes nothing in the cluster, Argo CD or Git |
| `install` | `--no-sync` — create the Application but do not sync it; `--timeout <duration>` — how long to wait for the sync (default `1h15m0s`) | Idempotent. Checks Argo CD ≥ 3.3 and the token; attaches the cluster VPC to the transit gateway if needed; creates the IAM trusted profile; renders; writes the out-of-band objects into ROKS; registers ROKS with Argo CD; publishes `manifests/git/` to Git; creates the Application and syncs it |
| `status` | — | Application sync/health and last operation, the check Jobs, and CNEInstance and License state |
| `diagnose` | — | Write `diagnostics/<time>/` in the workspace: the Application and resource tree, every check log, pods and Warning events in the BNK, cert-manager and check namespaces, and CNEInstance/License status. Read `summary.md` first. Never collects Secret values |
| `uninstall` | `--timeout <duration>` — wait for Argo CD to finish deleting (default `45m0s`); `--purge-git` — also remove the manifests from the Git repo; `--keep-trusted-profile` — keep the IAM trusted profile; `--detach-tgw` — detach the cluster VPC from the transit gateway if `install` attached it; `--remove-repo` — remove the Git repo credential from Argo CD (other Applications may use it); `--force` — delete the Application even when the pre-uninstall check fails or cannot run | Run the pre-uninstall check in ROKS, delete the Application with foreground cascading, run the post-uninstall check, and fail unless the BNK namespaces are gone, saving every check log to `diagnostics/uninstall-<time>/`; then remove the out-of-band Secrets, the check namespace and RBAC, the Argo CD cluster registration and its ServiceAccount, and the trusted profile. Asks for confirmation |

See [install](./08-install.md), [status and diagnose](./09-status-and-diagnose.md)
and [uninstall](./10-uninstall.md).

## Optional components

| Command | Flags | Purpose |
|---|---|---|
| `cos list` | — | List the objects in the configured bucket |
| `cos publish` | `--far-auth <file>` — the FAR auth tarball (`.tgz` from MyF5); `--jwt <file>` — the subscription JWT; `--create-bucket` — create the bucket if it does not exist | Upload the FAR auth tarball and/or the JWT to the configured bucket (at least one of the two files is required) |
| `cos verify` | — | Check the JWT is well-formed and the FAR key can read the BNK 2.4.0 manifest from FAR |
| `registry bom` | — | List every artifact an install pulls: the BNK manifest's charts and images, cert-manager's chart and images, and the check image |
| `registry replicate` | `--concurrency <n>` — parallel copies (default `2`) | Copy the bill of materials into `registry.mirror.host` under `registry.mirror.prefix`, keeping each source path; skips what is already there |
| `registry verify` | — | Report artifacts missing from the mirror |
| `flp up` | — | Build the F5 License Proxy VSI (own VPC, public gateway for egress to F5, attached to the transit gateway, serving `:8443`) and a CA BNK will trust |
| `flp status` | — | Show the license proxy VSI |
| `flp down` | — | Delete what `flp up` built. Asks for confirmation |
| `argocd up` | — | Build a **test** Argo CD hub (k3s and Argo CD on a VSI, on its floating IP at `test_hub.node_port`, attached to the transit gateway); points `argocd.server` at it and prints an API token |
| `argocd status` | — | Show the test hub |
| `argocd down` | — | Delete the test hub. Asks for confirmation |

See [The F5 License Proxy](./12-flp.md), [Mirroring into a private
registry](./13-registry.md), [The COS supply chain](./14-cos.md) and [A test Argo CD
hub](./15-test-hub.md).

## Troubleshooting and housekeeping

| Command | Flags | Purpose |
|---|---|---|
| `agent` | — | With no argument, list the supported CLIs and the workspace path |
| `agent <cli>` | — | Start one of the supported CLIs in the workspace, scaffolding it first if needed (the list is in [chapter 18](./18-agent.md)) |
| `agent init` | `--force` — overwrite existing files | Scaffold the troubleshooting guide, personas and a journal directory into the workspace ([chapter 18](./18-agent.md)) |
| `workspaces list` (alias `ws list`) | — | List workspaces; the current one is marked `*` |
| `workspaces use <name>` (alias `ws use`) | — | Make a workspace current |
| `version` | — | Print the version |
| `completion <shell>` | — | Generate a shell completion script (`bash`, `zsh`, `fish`, `powershell`) |
| `help [command]` | — | Help about any command |

See [Troubleshooting with an agentic CLI](./18-agent.md).

## The check binary

The in-cluster `check` binary is not run from your workstation; its modes and flags
are in the [Checks reference](./16-checks.md).

```text
check <mode> [flags]      modes: pre-install, node-probe, gateway-api-sweep,
                                 cert-manager-ready, license, post-install,
                                 pre-uninstall, post-uninstall, version
check <mode> -h           a mode's flags
```

Every check flag can also be set as `CHECK_<FLAG>` (upper case, dashes as
underscores).

## Environment variables

Secrets are read only from the environment; they are never asked for, written to
`config.yaml`, or stored in the workspace.

| Variable | Read by | Meaning |
|---|---|---|
| `IBMCLOUD_API_KEY` | every command that talks to IBM Cloud or the cluster | IBM Cloud API key. The variable name is `ibmcloud.api_key_env` (this is the default) |
| `IC_API_KEY` | same | fallback when the configured variable is empty |
| `ARGOCD_AUTH_TOKEN` | `install`, `uninstall`, `status`, `diagnose` | Argo CD API token. The variable name is `argocd.token_env` (this is the default). `argocd up` prints a token for its test hub |
| `ROKSBNKARGOCTL_GIT_TOKEN` | `install`, `uninstall --purge-git` | HTTPS token for `git.url`. The variable name is `git.token_env` (this is the default when `git.ssh_key_file` is not set) |
| `ROKSBNKARGOCTL_MIRROR_PASSWORD` | `render`, `install`, `registry` | password of `registry.mirror.username`. The variable name is `registry.mirror.password_env` (this is the default) |
| `ROKSBNKARGOCTL_HOME` | all | root of all workspaces (default `~/.roksbnkargoctl`) |
| `ROKSBNKARGOCTL_WORKSPACE` | all | workspace to use when `-w` is not given |
| `ROKSBNKARGOCTL_PRIVATE_ENDPOINT` | every command that reads the cluster | `1` fetches the cluster's admin kubeconfig for the **private** API endpoint, for an operator host with only private access. Otherwise the public endpoint is used. (This is independent of `argocd.cluster_endpoint`, which is how Argo CD reaches the cluster) |
| `ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY` | `install`, `uninstall --purge-git` | `1` switches off SSH host-key verification of the Git server. `install` warns every time it is set. Prefer `git.known_hosts_file`; see [Appendix D](./appendix-d-security.md) |

## See also

- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Workspaces and init](./05-workspaces-and-init.md)
