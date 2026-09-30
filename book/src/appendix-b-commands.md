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
| `--no-workspace` | run without a workspace, ignoring `ROKSBNKARGOCTL_WORKSPACE` and the current workspace: settings come from flags, `ROKSBNKARGOCTL_*` variables and defaults. Accepted by `cos`, `flp`, `registry` and `forge` commands; every other command refuses it. Cannot be combined with `-w` |
| `-y`, `--yes` | do not ask for confirmation (`uninstall`, `flp down`, `argocd down`, `cos delete`) |
| `-v`, `--verbose` | verbose output |
| `-h`, `--help` | help for the command |

The workspace is chosen in this order: `-w`, then `$ROKSBNKARGOCTL_WORKSPACE`, then
the current workspace recorded by `workspaces use` (the file `current` under the
workspace home). Workspace names are 1–40 lowercase letters, digits and `-`,
starting and ending alphanumeric.

Commands that ask for confirmation refuse when there is no terminal to ask on and
`--yes` was not given: `uninstall` says `not confirmed (pass --yes to skip the prompt)`,
`flp down` and `argocd down` say `not confirmed (pass --yes)`, and `cos delete` says
`not deleted: not confirmed (pass --yes to delete without asking)`.

Many component commands also take **config flags**: each overrides one `config.yaml` key
for the run, and its help ends with `(overrides <key>; env <variable>)`. A flag wins over
the key's `ROKSBNKARGOCTL_*` variable, which wins over `config.yaml`; neither is saved.
See [Appendix A](./appendix-a-config.md#overrides-flags-and-environment-variables).

## The lifecycle

| Command | Flags | Purpose |
|---|---|---|
| `init` | `-f`, `--config-file <path or URL>` — read `config.yaml` from a path or an http(s) URL instead of interviewing; `--refresh` — re-resolve the saved config without asking | Create or update a workspace: interview (or read a config file); check the Argo CD (≥ 3.3, token accepted) and the Git repository (readable; push rights only warned about) before saving anything; resolve the cluster, VPC, transit gateway, the Argo CD cluster endpoint and the COS objects against IBM Cloud. Re-running resumes the interview with saved answers as defaults. Never asks for secrets |
| `render` | `--no-cluster` — do not read the cluster (Kubernetes version and node image fall back to defaults) | Pull the BNK manifest, FLO chart and cert-manager chart, and write `manifests/git/` (the manifests Argo CD syncs from Git, and in `values/` the charts' Helm values), `manifests/direct/` (Secrets redacted), `manifests/charts/<chart>/` (each chart as Argo CD will render it; not published) and `manifests/application.yaml` (the charts as Helm sources, and Git). Changes nothing in the cluster, Argo CD or Git |
| `install` | `--no-sync` — create the Application but do not sync it; `--no-publish` — do everything except push to Git (the Application syncs what you pushed yourself, for example an `export`); `--timeout <duration>` — how long to wait for the sync (default `1h15m0s`) | Idempotent. Checks Argo CD ≥ 3.3 and the token, and that the Git credential can push (with `--no-publish`: that the repository is readable and has `git.branch`), before changing anything; attaches the cluster VPC to the transit gateway if needed; creates the IAM trusted profile; renders; writes the out-of-band objects into ROKS; registers ROKS with Argo CD; publishes `manifests/git/` to Git; registers the Git repository and each chart registry (with the registry login) in Argo CD; creates the Application and syncs it at the published commit. With `--no-publish`, syncs only if what Argo CD renders from Git, charts included, matches this render, and then exactly the commit it compared |
| `status` | — | Application sync/health and last operation, the check Jobs, and CNEInstance and License state |
| `diagnose` | — | Write `diagnostics/<time>/` in the workspace: the Application and resource tree, every check log, pods and Warning events in the BNK, cert-manager and check namespaces, and CNEInstance/License status. Read `summary.md` first. Never collects Secret values |
| `uninstall` | `--timeout <duration>` — wait for Argo CD to finish deleting (default `45m0s`); `--purge-git` — also remove the manifests from the Git repo; `--keep-trusted-profile` — keep the IAM trusted profile; `--detach-tgw` — detach the cluster VPC from the transit gateway if `install` attached it; `--remove-repo` — remove the Git repository and the chart registries, with their credentials, from Argo CD (other Applications may use them); `--force` — delete the Application even when the pre-uninstall check fails or cannot run | Run the pre-uninstall check in ROKS, delete the Application with foreground cascading, run the post-uninstall check, and fail unless the BNK namespaces are gone, saving every check log to `diagnostics/uninstall-<time>/`; then remove the out-of-band Secrets, the check namespace and RBAC, the Argo CD cluster registration and its ServiceAccount, and the trusted profile. Asks for confirmation |
| `export` | `-o`, `--output <file>` — zip file to write (default `roksbnkargoctl-<workspace>.zip`) | Zip the rendered `manifests/git/`, values files included, under `git.path`, with `ROKSBNKARGOCTL-EXPORT.md` (instructions) and `roksbnkargoctl-application.yaml` (the Application, for reference), for you to commit yourself before `install --no-publish`. Never contains a Secret; byte-identical for an unchanged render; warns when `config.yaml` changed after the last render |
| `show` | `--effective` — print the settings in force after `ROKSBNKARGOCTL_*` variables and defaults, instead of the file | Print the workspace's `config.yaml` verbatim on stdout; the workspace name and path go to stderr, so `show > config.yaml` captures the file alone. With `--effective`, the output is not the file: the overrides in force are listed on stderr |

Every command in this table needs a workspace and refuses `--no-workspace`.

See [install](./08-install.md), [status and diagnose](./09-status-and-diagnose.md),
[uninstall](./10-uninstall.md), [export](./07-the-application.md#without-letting-roksbnkargoctl-push-to-git)
and [show](./05-workspaces-and-init.md#show).

## Optional components

`cos`, `registry`, `flp` and `forge` run with or without a workspace. With one, its
settings are the defaults and the config flags override them.

### cos

| Command | Flags | Purpose |
|---|---|---|
| `cos instances` | — | List the account's COS service instances: name, GUID, resource group, state, CRN |
| `cos buckets` | `--instance` | List an instance's buckets with their regions, locations and creation times |
| `cos list` | `--instance`, `--bucket`, `--far-auth-object`, `--jwt-object` | List the objects in a bucket; the two F5 objects are marked `far-auth` and `jwt` |
| `cos publish` | `--far-auth <file>` — the FAR auth tarball (`.tgz` from MyF5); `--jwt <file>` — the subscription JWT; `--create-bucket` — create the bucket if the instance has none of that name; `--region` — where a new bucket is created (`cos.region`); `--instance`, `--bucket`, `--far-auth-object`, `--jwt-object` | Upload the FAR auth tarball and/or the JWT (at least one is required). An existing bucket is used in its own region |
| `cos delete` | `--object <key>` — object to delete (repeatable; default the FAR auth and JWT objects); `--instance`, `--bucket`, `--far-auth-object`, `--jwt-object` | Delete objects from a bucket, keeping the bucket. An absent object counts as deleted. Asks for confirmation |
| `cos verify` | `--instance`, `--bucket`, `--far-auth-object`, `--jwt-object` | Check the JWT is well-formed and the FAR key can read the BNK 2.4.0 manifest from FAR |

`--instance` takes a name, GUID or CRN (`cos.instance`); `--bucket` a name or a bucket CRN
(`cos.bucket`). A bucket's region is found from its location.

### registry

| Command | Flags | Purpose |
|---|---|---|
| `registry bom` | the BOM flags (below) | List every artifact an install pulls: the BNK manifest's charts and images, cert-manager's chart and images, and the check image |
| `registry replicate` | the BOM and mirror flags; `--concurrency <n>` — parallel copies (default `2`); `--state <file>` — write the replication record here (default `<workspace>/registry-mirror.json`; without a workspace none unless given) | Copy the bill of materials into `registry.mirror.host` under `registry.mirror.prefix`, keeping each source path; skips what is already there |
| `registry verify` | the BOM and mirror flags | Report artifacts missing from the mirror |

BOM flags: `--far-host` (`registry.far_host`), `--far-auth-file` (`cos.local_far_auth_file`),
`--cos-instance`, `--cos-bucket`, `--cos-region`, `--far-auth-object` (`cos.*`),
`--api-key-env` (`ibmcloud.api_key_env`), `--cert-manager-install`,
`--cert-manager-version` (`bnk.cert_manager.*`), `--check-image` (`check.image`).
Mirror flags: `--mirror-host`, `--mirror-prefix`, `--mirror-username`,
`--mirror-password-env`, `--mirror-ca-file` (`registry.mirror.*`). The mirror password
itself is never a flag: it is read from the variable `--mirror-password-env` names.

### flp

| Command | Flags | Purpose |
|---|---|---|
| `flp up` | `--name`, `--state`, `--ca-out <file>` — also write the CA certificate here (default without a workspace `./<name>-flp-ca.pem`); `--region`, `--resource-group`, `--transit-gateway`; `--zone`, `--vpc`, `--cidr`, `--profile`, `--allowed-cidrs`, `--ssh-key`, `--floating-ip` (`flp.vsi.*`); `--far-auth-file`, `--jwt-file`, `--cos-instance`, `--cos-bucket`, `--cos-region`, `--far-auth-object`, `--jwt-object` | Build the F5 License Proxy VSI (own VPC, public gateway for egress to F5, attached to the transit gateway, serving `:8443`) and a CA BNK will trust. With a CA file written, print the `flp.external` settings for an install |
| `flp status` | `--name`, `--state` | Show the license proxy VSI |
| `flp down` | `--name`, `--state`, `--region`, `--resource-group`, `--transit-gateway`, `--zone`, `--vpc` | Delete what `flp up` built, from its state; with the state lost, find the resources by their exact names. Asks for confirmation |

`--name` is the base name of the resources (`<name>-flp`, `<name>-flp-vpc`, …; default the
workspace name, required without one). `--state` is the state file: resource IDs, URL, CA
and CA key (default `flp-outputs.json` in the workspace, or `./<name>-flp.json` without
one).

### argocd

These need a workspace.

| Command | Flags | Purpose |
|---|---|---|
| `argocd up` | — | Build a **test** Argo CD hub (k3s and Argo CD on a VSI, on its floating IP at `test_hub.node_port`, attached to the transit gateway); points `argocd.server` at it and prints an API token |
| `argocd status` | — | Show the test hub |
| `argocd down` | — | Delete the test hub. Asks for confirmation |

### forge

| Command | Flags | Purpose |
|---|---|---|
| `forge register` | `--url`, `--username`, `--project`, `--endpoint public\|private`, `--ca-file`, `--insecure` (`forge.*`); `--cluster`, `--region`; `--force` — take over a cluster registered to another Forge project (its cluster ID changes) | Log in to BNK Forge; ensure the IBM Cloud credential template and the project; register the cluster with a certificate-based admin kubeconfig. Updates an existing registration in the same project in place |
| `forge unregister` | `--url`, `--username`, `--project`, `--ca-file`, `--insecure`, `--cluster`, `--region` | Remove the cluster from its Forge project. Absence is success |

Without a workspace, `--cluster` and `--region` are required. The password comes from
`BNK_FORGE_PASSWORD` or a prompt, never a flag.

See [The F5 License Proxy](./12-flp.md), [Mirroring into a private
registry](./13-registry.md), [The COS supply chain](./14-cos.md), [A test Argo CD
hub](./15-test-hub.md) and [Registering with BNK Forge](./15a-forge.md).

## Troubleshooting and housekeeping

| Command | Flags | Purpose |
|---|---|---|
| `agent` | — | With no argument, list the supported CLIs and the workspace path |
| `agent <cli>` | `--persona troubleshooter\|operator` — the persona its first turn takes (default `troubleshooter`); `--show` — print the command instead of running it | Start `agy`, `claude`, `codex`, `gemini`, `aider`, `pi` or `opencode` in the workspace, scaffolding it first if needed. Refuses when stdout is not a terminal, and in the container image unless `--show` ([chapter 18](./18-agent.md)) |
| `agent init` | `--force` — overwrite existing files | Scaffold the troubleshooting guide, personas and a journal directory into the workspace ([chapter 18](./18-agent.md)) |
| `workspaces list` (alias `ws list`) | — | List workspaces; the current one is marked `*` |
| `workspaces use <name>` (alias `ws use`) | — | Make a workspace current |
| `version` | — | Print the version and the BNK release this binary installs |
| `self install` | `--dir <dir>` — where to copy it (default: see [chapter 4](./04-installation.md#where-it-goes-self-install)); `--force` — copy even if the running binary is already the installed one | Copy the running binary onto `PATH`. Not the BNK `install`. Refuses in the container image |
| `self update` | `--version <vX.Y.Z>` — install that release (may go back, reinstall or take a prerelease); `--check` — list newer releases and change nothing; honours `--yes` | Replace the running binary with a release's, verified against the release's checksums. Only the archive for its own BNK release; never switches BNK releases. Updates the tool, not BNK. In the container image it refuses and names the image tag to pull; `--check` still works |
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
| `ARGOCD_AUTH_TOKEN` | `init` (existing Argo CD), `install`, `uninstall`, `status`, `diagnose` | Argo CD API token. The variable name is `argocd.token_env` (this is the default). `argocd up` prints a token for its test hub |
| `ROKSBNKARGOCTL_GIT_TOKEN` | `init` (checked; a warning when unset), `install` (optional with `--no-publish`), `uninstall --purge-git` | HTTPS token for `git.url`. The variable name is `git.token_env` (this is the default when `git.ssh_key_file` is not set) |
| `ROKSBNKARGOCTL_MIRROR_PASSWORD` | `render`, `install`, `registry` | password of `registry.mirror.username`. The variable name is `registry.mirror.password_env` or `--mirror-password-env` (this is the default) |
| `ROKSBNKARGOCTL_HOME` | all | root of all workspaces (default `~/.roksbnkargoctl`; `/work/.roksbnkargoctl` in the container image) |
| `ROKSBNKARGOCTL_WORKSPACE` | all | workspace to use when `-w` is not given; ignored with `--no-workspace` |
| `ROKSBNKARGOCTL_<KEY>` | every command that reads the key | overrides one `config.yaml` key for the run, for example `ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST`. Never saved. Every key but `bnk.version` has one; [Appendix A](./appendix-a-config.md) lists them all |
| `BNK_FORGE_URL`, `BNK_FORGE_USER` | `forge` | the BNK Forge URL and login, when neither a flag nor `ROKSBNKARGOCTL_FORGE_URL` / `ROKSBNKARGOCTL_FORGE_USERNAME` sets them; they win over `config.yaml` |
| `BNK_FORGE_PASSWORD` | `forge` | the BNK Forge password (else a prompt on a terminal). There is no flag or key for it |
| `ROKSBNKARGOCTL_CONTAINER` | `self install`, `self update`, `agent` | set to `1` by the container image; exactly `1` makes those commands behave as described in [chapter 4](./04-installation.md#run-with-docker). Do not set it yourself |
| `ROKSBNKARGOCTL_PRIVATE_ENDPOINT` | every command that reads the cluster | `1` fetches the cluster's admin kubeconfig for the **private** API endpoint, for an operator host with only private access. Otherwise the public endpoint is used. (This is independent of `argocd.cluster_endpoint`, which is how Argo CD reaches the cluster) |
| `ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY` | `install`, `uninstall --purge-git` | `1` switches off SSH host-key verification of the Git server. `install` warns every time it is set. Prefer `git.known_hosts_file`; see [Appendix D](./appendix-d-security.md) |

## See also

- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Workspaces and init](./05-workspaces-and-init.md)
