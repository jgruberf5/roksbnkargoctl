# Registering with BNK Forge (forge)

BNK Forge manages BIG-IP Next for Kubernetes clusters from one place. This chapter describes
`roksbnkargoctl forge register` and `forge unregister`, which add your ROKS cluster to a BNK
Forge project, or remove it, over Forge's REST API. After reading it you will be able to
register a cluster with or without a workspace, re-register it safely, move it between
projects deliberately, and remove it.

> **Not yet tested against a live BNK Forge.** The two commands are a port of roksbnkctl's
> BNK Forge registration and are covered by tests against a simulated Forge API only. They
> have not yet been run against a real BNK Forge. Try them on a non-production project
> first.

## Commands

```sh
roksbnkargoctl forge register   [--project P] [--endpoint public|private] [--force]
roksbnkargoctl forge unregister [--project P]
```

| Command | Does |
|---|---|
| `forge register` | Logs in; ensures the IBM Cloud credential template and the project; registers the cluster with a certificate-based admin kubeconfig. Updates an existing registration in place |
| `forge unregister` | Removes the cluster from its project. Never creates anything; absence is success |

Both run **with or without a workspace**. With one, the cluster is the one `init` resolved
(its ID, name and endpoints come from the `resolved:` section, so the workspace must have
been initialised). Without one, name the cluster: `--cluster` and `--region` are required.

```sh
export IBMCLOUD_API_KEY=…
export BNK_FORGE_URL=https://forge.example.internal
export BNK_FORGE_USER=admin
export BNK_FORGE_PASSWORD=…
roksbnkargoctl forge register --no-workspace --cluster my-roks --region us-south
```

## Settings

| Setting | Flag | `config.yaml` key | Also from | Default |
|---|---|---|---|---|
| Forge URL | `--url` | `forge.url` | `BNK_FORGE_URL` | none (required) |
| Login | `--username` | `forge.username` | `BNK_FORGE_USER` | a prompt on a terminal |
| Password | none | none | `BNK_FORGE_PASSWORD` | a no-echo prompt on a terminal |
| Project | `--project` | `forge.project` | | the cluster's name |
| Endpoint in the kubeconfig (`register` only) | `--endpoint` | `forge.endpoint` | | `public` |
| Pinned CA | `--ca-file` | `forge.ca_file` | | the system roots |
| Skip TLS verification | `--insecure` | `forge.insecure` | | `false` |
| Cluster | `--cluster` | `cluster` | | the workspace's cluster; required without one |
| Region | `--region` | `ibmcloud.region` | | the workspace's region; required without one |

Every flag also has its `ROKSBNKARGOCTL_*` variable, for example
`ROKSBNKARGOCTL_FORGE_URL` ([Appendix A](./appendix-a-config.md#forge)). For the URL and the
login, the order is: a flag or `ROKSBNKARGOCTL_FORGE_*` variable, then `BNK_FORGE_URL` or
`BNK_FORGE_USER`, then `config.yaml`. `BNK_FORGE_*` are the names roksbnkctl reads, so one
environment serves both tools.

**The password is never a flag and never stored.** It comes from `BNK_FORGE_PASSWORD`, or a
prompt when there is a terminal; without either the command stops with
`no BNK Forge password: set BNK_FORGE_PASSWORD (there is no terminal to prompt on)`. The
session is not cached, so every run logs in.

The IBM Cloud API key comes from `IBMCLOUD_API_KEY` (or the variable `ibmcloud.api_key_env`
names). `register` stores it in Forge, below.

## What register does

1. **Checks the inputs** before contacting anything: the endpoint is `public` or
   `private`, the API key is set, the Forge URL starts with `https://` (or `http://`), and
   a login and password are available.
2. **Finds the cluster.** With a workspace, from `resolved:`. Without one, it looks
   `--cluster` (name or ID) up in IBM Cloud and refuses one in another region than
   `--region`, since Forge would be told the wrong region. The resource group sent to Forge
   is the workspace's `ibmcloud.resource_group`; without a workspace it is the cluster's
   own, unless `ROKSBNKARGOCTL_IBMCLOUD_RESOURCE_GROUP` overrides it.
3. **Fetches the cluster's admin kubeconfig** for the endpoint `--endpoint` names and turns
   it into a self-contained, certificate-based kubeconfig: one cluster, one user carrying
   the admin client certificate and key, one context, no file references and no token.
   ROKS is OpenShift, whose API server accepts client certificates or OpenShift OAuth
   tokens and rejects IBM IAM tokens, so a token kubeconfig would register and then fail
   to connect. A cluster without the chosen endpoint is refused, naming the other one.
4. **Logs in** to Forge (`POST /api/auth/login`).
5. **Ensures the IBM Cloud credential template** `roksbnkargoctl-<project>`: it holds the
   IBM Cloud API key, the resource group, the region and, with a workspace or an explicit
   `cos.instance`, the COS instance name, and is marked the default. It is created if it
   is missing, and updated with the current values if it exists.
6. **Ensures the project** (default: the cluster's name), creating it if needed, and sets
   its platform to ROKS on IBM Cloud.
7. **Registers the cluster** in the project by its cluster ID and region, with the
   template and the kubeconfig. Forge connects to the cluster as soon as it is registered.

```text
→ fetching the admin kubeconfig for my-roks (public endpoint)
→ registering cluster my-roks with BNK Forge https://forge.example.internal (project my-roks)
✓ registered cluster my-roks with BNK Forge (project "my-roks", Forge cluster id 42)
```

### Re-registering is non-destructive

| The cluster (by name) is | `register` |
|---|---|
| Not registered anywhere | Creates it |
| Already in **this** project | Updates it in place; its Forge cluster ID is kept |
| In **another** project | Refuses, naming that project, unless `--force` |

The refusal reads:

```text
registering cluster my-roks with BNK Forge: cluster is registered to another BNK Forge project: "my-roks" is held by project "team-a" (id 3, cluster id 17). Re-registering would move it, changing its cluster id and removing it from that project's view. Pass --force to take it over deliberately, or unregister it there first
```

`--force` is a real move: the cluster is deleted from the other project and created in
this one, so its Forge cluster ID changes, and a warning says so. A Forge without an update
call for clusters falls back to delete-and-create only when it answers 404 or 405; any
other failure is reported, never turned into a destructive retry. A project the account
cannot read is a warning, not a refusal, and a response that cannot be parsed is an
error, never read as "nobody holds it".

## What unregister does

`unregister` logs in, finds the project (default: the cluster's name) and removes the
cluster from it. It never creates anything, and **absence is success**: no such project,
no cluster of that name in it, or a cluster already deleted all exit 0, so a teardown can
run it twice, or late.

```text
✓ no BNK Forge project "my-roks": nothing to unregister
✓ cluster my-roks is not registered in BNK Forge project "my-roks": nothing to do
✓ unregistered cluster my-roks (Forge cluster id 42) from BNK Forge project "my-roks"
```

Without a workspace, a cluster that is already gone from IBM Cloud is still unregistered,
by the name you gave, with a warning: a teardown often runs after the cluster is deleted.

## TLS

Forge's certificate is verified against `--ca-file` (`forge.ca_file`) when one is given,
otherwise against the system roots. `--insecure` skips verification. The login password
and the session token then cross a connection that is encrypted but not authenticated, so
the first request warns:

```text
⚠ BNK Forge TLS verification is DISABLED for forge.example.internal: the password and the API token are sent over a connection that is encrypted but NOT authenticated.
  Anyone able to intercept it can present any certificate for that host and read them.
  Pin the Forge CA instead (forge.ca_file, --ca-file) and drop insecure.
```

For a public IP address it adds that the connection leaves your network. With a CA file
set, `insecure` is ignored and the certificate is verified.

## What Forge holds afterwards

| In Forge | Holds |
|---|---|
| Credential template `roksbnkargoctl-<project>` | Your IBM Cloud **API key**, resource group, region, and optionally the COS instance name |
| The cluster registration | The cluster's **admin** kubeconfig, with the admin client certificate and key |

Both are credentials. Use an API key and a Forge account scoped to what Forge needs, and
remove the registration with `forge unregister` when the cluster no longer needs Forge.
`uninstall` does not unregister the cluster; run `forge unregister` yourself.

## See also

- [Workspaces and init](./05-workspaces-and-init.md#running-without-a-workspace)
- [Appendix A: forge](./appendix-a-config.md#forge)
- [Appendix B: command reference](./appendix-b-commands.md)
- [Appendix D: security model](./appendix-d-security.md)
