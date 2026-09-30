# Troubleshooting guide

When an install or uninstall stops, the fastest route to the cause is to find the
**lowest wave that did not finish** and read what that wave's check or resource
says about itself. This chapter shows you how to find that wave, then catalogues
every failure seen so far — on the command line, in each sync wave, and during
uninstall — with its cause and its fix. The checks are written so that their own
log line is the diagnosis; most entries below start by quoting one.

## Step 1: find the failing wave

```sh
roksbnkargoctl status      # Application sync/health, last operation, check Jobs, CNEInstance, License
roksbnkargoctl diagnose    # writes diagnostics/<time>/ in the workspace; read summary.md first
```

`status` prints the Application's sync and health, the last operation's phase and
message, every check Job in `roksbnkargoctl-check` with its phase, and a one-line
summary of the CNEInstance and License. `diagnose` saves, without needing `kubectl`
or `oc`:

| File | Contents |
|---|---|
| `summary.md` | the status output, Argo CD's last operation, the hook Jobs as Argo CD sees them, every pod not Running/Succeeded, and the **final JSON verdict line of every check pod** (plus each node-probe pod's published result) |
| `application.yaml`, `resource-tree.yaml` | the Application and its resource tree |
| `log-<pod>.txt` | every check pod's log (last 2000 lines) |
| `pods-<ns>.yaml`, `warnings-<ns>.txt` | pods and Warning events in `roksbnkargoctl-check`, the BNK namespaces and `cert-manager` |
| `cneinstance.yaml`, `license.yaml` | the two CRs (the License's `spec.jwt` is replaced with `REDACTED`) |

Then:

1. Take the **lowest** wave that is not Healthy/Succeeded. Everything in later waves
   is a consequence; do not chase it.
2. If that wave has a check, read its log. The `[FAIL]` lines name the check, and
   the last line of stdout is the JSON verdict.
3. If it has no check, read the resource's conditions and the Warning events of its
   namespace.

The waves at a glance (details in [How an install flows](./02-how-an-install-flows.md)):

| Wave | What | Typical failure |
|---|---|---|
| (install) | trusted profile, Secrets, check namespace + RBAC, cluster registration, Git publish, Application | the CLI prints the failing API call; fix and re-run `install` (it is idempotent) |
| −20 | namespaces | Argo CD cannot reach the ROKS API |
| −19 | `registry-ca-trust` (private-CA mirror only) | node image, SCC |
| −18 | `check-node-probe` + `check-pre-install` | a prerequisite or node reachability |
| −12 | cert-manager | image pulls |
| −11 | `check-cert-manager-ready` | cert-manager webhook never admits |
| −10…−8 | issuer chain | webhook |
| −6 | NAD, FLO SCC binding, `check-gateway-api-sweep` | sweep not running |
| −5 | FLO | image pull, pull secret |
| −4 / −2 | CNEManifest, CNEInstance | FLO not running, CRDs missing |
| 0 | `check-license` | License not Active, CNEInstance not Available |
| PostSync | `check-post-install` | TMM, image pulls, deploymentSize |
| PreDelete / PostDelete | `check-pre-uninstall` / `check-post-uninstall` | drains, finalizers |

## Step 2: match it below

### Before the sync: `init`, `render`, `install`

**`invalid config:` followed by a list.** `config.yaml` failed validation. Every
problem is listed at once; the messages name the key. Parsing is strict, so a
misspelt or unsupported key (including any attempt at `deployment_size`) is an error
rather than a silently ignored default. See [Appendix A](./appendix-a-config.md).

**`IBM Cloud API key: set the environment variable IBMCLOUD_API_KEY`**, **`Git
token: set the environment variable …`**, **`the mirror password: set …`.** A
secret is read only from the environment. Set the variable named in the message
(see [Appendix B](./appendix-b-commands.md#environment-variables)).

**`argocd: … token rejected (401)`** — `ARGOCD_AUTH_TOKEN` is not a valid,
unexpired Argo CD token for `argocd.server`.

**`Argo CD API token: set the environment variable ARGOCD_AUTH_TOKEN: an API token for
the Argo CD at <server> (Argo CD UI: Settings → Accounts → <account> → Tokens → Generate
New)`** (from `init`). `init` checks an existing Argo CD before it saves the workspace,
so the token must be set first. Generate one where the message says, export it and run
`init` again. To build a test hub instead, answer `n` to `Use an existing Argo CD
instance (3.3 or later)?` ([chapter 15](./15-test-hub.md)).

**`checking the Argo CD at <server>: argocd: the server at <server> did not accept the
API token (session/userinfo: not logged in); check ARGOCD_AUTH_TOKEN`.** The server
answered, but not for this token: it belongs to another Argo CD, was revoked, or has
expired. `init` and `install` both report Argo CD failures with the `checking the Argo
CD at <server>:` prefix.

**`argocd: server version … is older than the required 3.3.0 (PreDelete hooks need
Argo CD >= 3.3)`.** The uninstall checks are PreDelete/PostDelete hooks; an older
Argo CD would delete BNK without draining it. Upgrade Argo CD.

**Git access (`init`, and `install` step 1).** `init` and `install` check the Git
repository before they change anything. Every message below stops `install`. At
`init`, a repository that cannot be read with the credential stops it too, but missing
push rights and a missing credential are only warnings, because the `export` and
`install --no-publish` flow pushes nothing:

| Message | Cause | Fix |
|---|---|---|
| `git: <url> not found, or the credential cannot see it: …` | Wrong `git.url`, or the token or deploy key has no access to that repository (private repositories answer "not found" to an outsider) | Correct `git.url`, or grant the credential access |
| `git: <url> needs a credential (a token in git.token_env, ROKSBNKARGOCTL_GIT_TOKEN by default, or git.ssh_key_file): …` | No credential was sent, or it was rejected | Set the token variable, or `git.ssh_key_file` |
| `git: the credential may not push to <url> (a read-only token or deploy key?): …` | The credential can read but not push | Give it write access (a deploy key with write access, a token with push scope), or commit an `export` yourself and run `install --no-publish` |
| `git: cannot read <url>: …` / `git: cannot push to <url>: …` | Anything else: unreachable host, TLS, an SSH host-key failure (below) | Read the quoted cause |
| `git: <url> has no branch <branch>: push the export there first (roksbnkargoctl export)` | `install --no-publish`, and `git.branch` does not exist yet | Push the export to that branch first |
| `no Git credential: Git token: set the environment variable ROKSBNKARGOCTL_GIT_TOKEN` | `install` without `--no-publish` and no credential set | Set the token, or `git.ssh_key_file` |
| `gitpub: an ssh URL needs SSHKeyPEM` | A `git@…` URL without `git.ssh_key_file` | Set `git.ssh_key_file`, or use the `https://` URL |
| `git: no Git credential is set here, and Argo CD cannot read <url> with its own registration: …` | `install --no-publish` with no credential: the repository is private and not registered in Argo CD (Argo CD says `authentication required` or, for SSH, that it has no key) | Register the repository in Argo CD, or set the token variable or `git.ssh_key_file` |

At `init` the warnings read `⚠ no Git credential ($ROKSBNKARGOCTL_GIT_TOKEN or
git.ssh_key_file): install needs one to push, unless you commit `roksbnkargoctl export`
yourself and run install --no-publish` and `⚠ git: the credential may not push to <url>
(…): …: install will fail at the push; fix the credential, or commit `roksbnkargoctl
export` yourself and run install --no-publish`. See [init's
checks](./05-workspaces-and-init.md#the-checks-init-makes-before-it-saves).

**`the Application was created but NOT synced: what is in Git (revision <rev>) does not
match this workspace's render (N difference(s)): …`** (`install --no-publish`). The
repository at `git.branch`/`git.path` does not hold this render: the export was not
pushed, was pushed to another branch or path, is from before a change to `config.yaml`
or an upgrade of roksbnkargoctl, or the old contents of `git.path` were not replaced.
Each listed line says `missing in Git`, `different in Git` or `in Git but not in this
render`. Run `roksbnkargoctl export`, replace `git.path` in the repository with its
contents, push, and run `install --no-publish` again. Nothing was synced.

**`the Application was created but not synced: Argo CD could not read <url>
<branch>:<path>: … (was the export pushed there?)`** (`install --no-publish`). Argo CD
itself cannot produce manifests from that location: the path does not exist on the
branch, or Argo CD has no access to the repository. With no Git credential set, `install`
leaves Argo CD's repository registration as it is and checks at step 1 that Argo CD can
read the branch, so this usually means the path is wrong.

**`refusing to attach: the cluster VPC overlaps VPCs already on <gateway>`.** A
transit gateway silently blackholes one of two overlapping VPCs, so `install`
refuses. Move one of the prefixes, or attach the cluster VPC yourself if the
overlap is intended to be routed differently.

**`the FAR key cannot read BNK 2.4.0 from repo.f5.com (an EA key on a GA install
answers 403)`** (from `cos verify`), or a 403 from `repo.f5.com` during `render` or
as `ImagePullBackOff` in the cluster. **Cause:** the FAR auth tarball is the
Early-Access key, not the GA key. The EA key authenticates but cannot see 2.4.0 GA.
**Fix:** download the GA key from MyF5 (`f5-far-auth-key.tgz`), `cos publish
--far-auth` it (or point `cos.local_far_auth_file` at it), run `cos verify`, then
`install` again.

**`the mirror has no copy / an older copy (sha256:…) of the check image, not
sha256:…: run roksbnkargoctl registry replicate`.** **Cause:** mirror mode, and the
mirror does not hold the check image digest this build resolved upstream — a mirror
does not follow a mutable tag, so a copy replicated earlier can be stale. On a live
run, pinning `:dev` against the mirror faithfully pinned a copy replicated before a
check fix, and the cluster ran a check the code no longer contained; `render` now
refuses that. **Fix:** `roksbnkargoctl registry replicate`, then re-run.

**`disconnected mode needs an F5 License Proxy: run roksbnkargoctl flp up, or set
flp.external`.** Build the proxy first, or configure `flp.external.url` and
`flp.external.root_ca_file`.

**`flp.vsi.cidr is required` / `test_hub.cidr is required`.** Choose a /24–/28 not
used by any VPC on the transit gateway.

**`gitpub: SSH host <host> is not a known host (built in: github.com, gitlab.com,
bitbucket.org); set git.known_hosts_file …`.** **Cause:** the Git server is not one
whose host key is built in. **Fix:** obtain the server's host key from a trusted
source, put it in a known_hosts file and set `git.known_hosts_file`. `install` also
gives the file to Argo CD, whose own clone must verify the same host.

**`gitpub: SSH HOST KEY MISMATCH for <host>: … refusing to connect (possible
man-in-the-middle …)`.** **Stop.** The server presented a key that does not match
the known one. Either the server really changed its key — confirm that
out of band with the Git host's administrators, then update `git.known_hosts_file`
— or something is intercepting the connection. Do not work around this with
`ROKSBNKARGOCTL_GIT_INSECURE_HOSTKEY=1`; see [Appendix D](./appendix-d-security.md).

**Argo CD answers `415 Invalid content type` on uninstall.** Argo CD 3.5.1 rejects
a body-less DELETE without a `Content-Type` header. The current roksbnkargoctl
declares JSON on every non-GET request; if you see this, you are running an older
build — upgrade it and re-run `uninstall`.

### Wave −20: nothing applies at all

**The Application cannot reach the cluster; Argo CD reports `x509: certificate
signed by unknown authority` for the cluster.** **Cause:** Argo CD registers ROKS by
its **private** service endpoint (`argocd.cluster_endpoint: private`, the default),
reached over the transit gateway. That endpoint's certificate chains to the
cluster's own root CA, not to public roots, so Argo CD must be given that CA. The
public endpoint is the opposite: publicly trusted, and giving a cluster CA would
*replace* Argo CD's system roots and fail. `install` sends the CA only for the
private endpoint. **Fix:** re-run `install`, which re-registers the cluster. If the
error persists, check that `argocd.cluster_endpoint` matches how your hub actually
reaches the cluster.

**Argo CD times out dialling the cluster.** The hub cannot route to the private
endpoint: its VPC is not on the transit gateway, or a security group blocks it.
Attach the hub's network to the gateway, or use `argocd.cluster_endpoint: public`.

**Pods in a BNK namespace are rejected: `unable to find annotation
openshift.io/sa.scc.uid-range`.** **Cause:** a Namespace server-side-applied with an
empty `annotations: {}` map is never given its `openshift.io/sa.scc.*` annotations
by OpenShift's cluster-policy-controller, and every pod in it is refused.
roksbnkargoctl prunes empty metadata maps both in the renderer and before every
apply. **Fix:** if you see this, something else applied the namespace with an empty
map (a hand edit in Git, another tool). Re-applying the namespace *without* the
empty map repairs it: remove it from the Git copy and re-sync, or re-run `install`.

### Wave −18: `check-pre-install`

Read the `[FAIL]` lines; each names its check. See [Checks reference: pre-install](./16-checks.md#pre-install)
for every criterion.

**`secrets: missing: …`.** The out-of-band Secrets are written by `install`
before it creates the Application. If you synced from the Argo CD UI after only
`render`, or deleted a Secret by hand, re-run `install`.

**`existing-bnk: BNK is already (or partly) installed: …`.** A previous install —
by this tool or by roksbnkctl — left FLO, a CNEInstance, a License, a Terminating
BNK namespace or CWC license secrets. Uninstall it properly (for this tool:
`roksbnkargoctl uninstall`, which runs the drain and cleanup checks). Do not delete
namespaces by hand while F5 finalizers are pending; that produces the stuck
Terminating namespace described under Uninstall.

**`workers` / `zones` failures.** Fewer than 3 schedulable Ready workers, or they
span fewer than 3 zones. The `[INFO] workers: not counted:` line says which nodes
were excluded and why (cordoned, tainted, not Ready).

**`storageclass: no default StorageClass`.** BNK's DSSM, controller and downloader
claim volumes on the default class. Mark one class default, or set
`bnk.storage_class`.

**`node-probe: unreachable from N node/target pair(s): <node> -> <target>: …`.** Read
the error on each pair:

| Error | Cause | Fix |
|---|---|---|
| `name does not resolve from this node` (`dns=FAILED`) | the node's resolver cannot resolve the target | DNS on the worker subnet; for a private mirror, a private DNS zone the workers use |
| `tcp connect failed (refused, filtered, or no route)` on `repo.f5.com:443`, `ghcr.io:443`, `quay.io:443` or F5 licensing | the node has no Internet egress (no public gateway on its subnet) or a security group/ACL blocks 443 | add a public gateway to the worker subnets, or use a mirror and disconnected mode. If the target is FAR but you meant to use a mirror, check `registry.source` |
| `tcp connect failed` on the FLP `:8443` | the transit gateway connection for the FLP VPC or the cluster VPC is missing, or the FLP security group does not admit the cluster's CIDRs on 8443 | `roksbnkargoctl flp status`; check the gateway connections and `flp.vsi.allowed_cidrs` |
| `tls handshake failed` | the port answers TCP but does not speak TLS | wrong port, or a proxy in the path |
| `certificate not trusted` | the chain does not verify | set `registry.mirror.ca_file` for a private-CA mirror |
| only some nodes fail, split by zone | a subnet, ACL or security group fixed in only some zones | compare the failing nodes' zones |

A failure seconds after the cluster was attached to the transit gateway is usually
route propagation; the probe already retries each target for 180 s. Re-sync once
the routes are in.

**`node-probe: only N of M node(s) reported within 15m0s … Waiting on: …`.** A
probe pod never published: it is not scheduled, cannot pull the check image
(ghcr.io or the mirror unreachable, or a missing pull secret in mirror mode), or
cannot reach the API server to patch its own annotation. Look at the named pods in
`pods-roksbnkargoctl-check.yaml` and their logs.

**Check pods in `ImagePullBackOff`.** The check image cannot be pulled: FAR mode
needs `ghcr.io` from every node; mirror mode needs the check image in the mirror
(`registry verify`) and the `mirror-secret` in `roksbnkargoctl-check`.

### Waves −12 to −8: cert-manager

**`check-cert-manager-ready` fails: `the cert-manager webhook did not admit a
ClusterIssuer within 10m0s; last error: … x509 …`.** **Background:** Argo CD
advances once cert-manager's Deployments are healthy, but the cainjector may not
yet have written the new CA into the webhook configuration; on a live reinstall
the issuer apply failed `failed calling webhook webhook.cert-manager.io … x509:
certificate signed by unknown authority`. This hook dry-runs a ClusterIssuer until
the webhook admits one, so a short lag is absorbed. **If it still fails after 10
minutes,** the webhook or cainjector is genuinely unhealthy: look at the
`cert-manager` pods and Warning events (image pulls from `quay.io` or the mirror
are the usual cause), then re-sync.

**`… refused for a reason retrying will not fix: …`.** Not a readiness race — for
example a 403 because the check ClusterRole was altered. Re-run `install` to
re-apply the RBAC.

### Wave −6 / −5: Gateway API CRDs and FLO

**FLO's crd-installer fails; Gateway API CRDs are rejected by admission.** **Cause:**
OpenShift's ValidatingAdmissionPolicy
`openshift-ingress-operator-gatewayapi-crd-admission` refuses Gateway API CRD
writes from anything but its ingress operator. The `check-gateway-api-sweep`
Deployment removes it (and its binding) every 5 s until the CRDs exist. **Fix:**
check that the sweep pod is running and read its log. If it reports `still missing
[…] after 20m0s; is FLO installed and its crd-installer running?`, the sweep did
its part and FLO did not install the CRDs — look at the FLO pods.

**FLO pods `ImagePullBackOff`.** Pull secret or FAR key: see the 403 entry above,
and in mirror mode run `registry verify`.

**Argo CD: `the server could not find the requested resource` on the
CNEInstance.** FLO's CRDs did not install, because FLO's wave failed first. Fix FLO.

### Wave 0: `check-license`

**`license-crd: CRD licenses.k8s.f5net.com did not appear within 10m0s`.** FLO's
crd-installer has not run. Check the FLO pods and the CNEInstance.

**`License apply refused (attempt N, retrying …)` in the log.** Expected on a first
install: BNK's ResourceQuota `f5-single-license-quota` refuses License admission
until the quota controller has observed the new CRD. The hook retries for 5
minutes. Only `license-apply: … still refused after 5m0s` is a failure.

**`license-active: License did not reach state Active within 35m0s`.**

| Mode | Cause | Fix |
|---|---|---|
| connected | nodes cannot reach `product.apis.f5.com` / `product-s.apis.f5.com`, or the JWT is expired or not for this product | check node egress (pre-install probes these); republish a valid JWT with `cos publish --jwt` and reinstall |
| disconnected | the FLP is not reachable from the nodes, or Secret `licenseserver-rootca` does not hold its root CA, or CWC started before the CA existed | `flp status`; the hook's `flp-rootca` finding; the hook already rolls CWC once per CA (`cwc-restart`) |
| disconnected | License stuck in `Device registration in progress` | known intermittent product behaviour; recovery is uninstall and install, not editing the CR |

Read the CWC logs in `f5-utils` for the licensing conversation itself.

**`flp-rootca: Secret f5-utils/licenseserver-rootca has no PEM …`.** `install`
writes this Secret from the CA `flp up` created (or from
`flp.external.root_ca_file`). Re-run `install`.

**`cneinstance-available: CNEInstance … not Available`.** On 2.4 the CNE controller
runs no resource controllers until the License is Active, so check the License
first. If the License is Active, read the quoted condition and the `f5-bnk` events.

### PostSync: `check-post-install`

**`deployment-size: CNEInstance deploymentSize is "Small"; IBM ROKS supports only
Tiny …`** — or TMM pods `Pending` with `Insufficient hugepages-2Mi`. **Cause:**
someone edited `deploymentSize` in the Git repository. Every size above Tiny
requests hugepages; ROKS workers report `hugepages-2Mi: 0` and have no Machine
Config Operator to allocate them. **Fix:** restore `deploymentSize: Tiny` in Git
(or re-run `install`, which republishes the rendered manifests) and re-sync. See
[Appendix C](./appendix-c-sizing.md).

**`tmm: N TMM pod(s) Ready, M expected`.** Read the named non-Ready pods. The TMM
placement requires one TMM per node and an even spread across zones
(`DoNotSchedule`), so more replicas than workers, or than a zone can balance, stay
Pending. Multiple TMM replicas do **not** need ReadWriteMany storage.

**`tmm-zones: N Ready TMM pods all in one zone`.** A zone outage would take the data
plane down; check node zone labels and why the other zones' pods are not Ready.

**`pods: N container(s) stuck: …`.** Each stuck container is named with its reason
(`ImagePullBackOff`, `CrashLoopBackOff`, …) and message.

### Uninstall

`uninstall` saves every check pod's log and verdict into
`diagnostics/uninstall-<time>/` (with its own `summary.md`) **while** Argo CD
deletes the Application — including hook pods, which Argo CD deletes with
the Application.

**`pre-uninstall check: … the Application was NOT deleted`.** The drain failed, so
`uninstall` stopped before deleting anything — by design, FLO is left running to finalize
what remains. Read the `check-pre-uninstall-cli` log in `diagnostics/uninstall-<time>/`:

| Finding | Meaning | Fix |
|---|---|---|
| `drain: N F5 object(s) in <ns> did not finalize within 15m0s: …` | the named CRs are still present | look at why (their finalizers, FLO logs); then run `uninstall` again |
| `every delete in <ns> was refused for 45s; waiting cannot help` | deletes are refused by something other than the transient F5 webhook | read the refusal in the log |
| `ipam: fic.f5.com objects remain` | IPAM objects not cleaned up | the CNEInstance was deliberately kept so its controller can remove them |
| `cneinstance: not deleted, because the objects above have not gone` | consequence of the above | fix the above |

**Namespace `f5-bnk` or `f5-utils` stuck `Terminating`.** **Cause:** an F5 finalizer
whose controller is already gone. **Fix:** `check-post-uninstall` strips F5
finalizers (`f5.com`, `f5net.com` and the known F5 ones) after a 30-second grace
and reports `[WARN] finalizers: stripped F5 finalizers from N object(s)`. If it
still fails, it quotes the namespace's `NamespaceFinalizersRemaining` /
`NamespaceContentRemaining` messages; they name what is holding it. Never strip
`kubernetes.io/*` finalizers by hand. A stuck Terminating namespace also fails the
next install's pre-install check.

**F5 CRDs remain after uninstall.** By design: deleting a CRD deletes every CR of
that kind and can hang namespaces. `check-post-uninstall` lists them as `[INFO]`.

## If none of these match

Collect a `diagnose` bundle, note the lowest failing wave and quote its check's
`[FAIL]` line. If two signals disagree — a check passes that names the property you
think is broken — resolve that disagreement before concluding. The
[agent](./18-agent.md) chapter describes a structured way to do this with a
troubleshooting persona.

## See also

- [Checks reference](./16-checks.md)
- [status and diagnose](./09-status-and-diagnose.md)
- [uninstall](./10-uninstall.md)
- [Appendix E: design decisions and live findings](./appendix-e-design.md)
