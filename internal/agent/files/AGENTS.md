# AGENTS.md — troubleshooting a roksbnkargoctl BNK install

> Scaffolded into a workspace by `roksbnkargoctl agent init`. roksbnkargoctl embeds
> no LLM: bring your own coding-agent CLI (`roksbnkargoctl agent <cli>`), and act
> as exactly one persona from `personas/` at a time.

## What this workspace is

One BNK 2.4 GA install on one existing ROKS cluster, delivered as one Argo CD
Application in an existing, external Argo CD.

```
config.yaml                 the contract: cluster, transit gateway, mode, registry, Argo CD, Git
manifests/git/              EXACTLY what Argo CD syncs (published to the customer's Git repo)
manifests/direct/           what `install` writes straight into ROKS (Secrets shown REDACTED)
manifests/application.yaml  the Argo CD Application
diagnostics/<time>/         bundles written by `roksbnkargoctl diagnose`
flp-outputs.json            the license proxy VSI (disconnected mode)
registry-mirror.json        the last mirror replication (mirror mode)
journal/                    append-only notes, one file per significant action
```

## How the install runs (read this before diagnosing anything)

`install` does the IBM and out-of-band work, then Argo CD syncs Git in waves.
**Every container runs in ROKS; nothing runs on the Argo CD hub.**

| Wave | What | If it stalls here |
|---|---|---|
| (install) | IAM trusted profile, Secrets, `roksbnkargoctl-check` namespace + RBAC, ROKS registered in Argo CD, Git published, Application created | the CLI printed the failing API call; fix and re-run `install` (idempotent) |
| −20 | namespaces | Argo CD cannot reach the ROKS API: the cluster registration (private endpoint over the transit gateway) |
| −19 | `registry-ca-trust` DaemonSet (private mirror CA only) | node-resolver image, SCC binding |
| −18 | `check-node-probe` DaemonSet + `check pre-install` hook | **the pre-install check failed**: read its log (`diagnose`); it names the node, target and reason |
| −12 | cert-manager | image pulls (quay.io or mirror), webhook not Ready |
| −10…−8 | issuer chain `selfsigned-cluster-issuer` → `ext-ca` → `sample-issuer` | cert-manager webhook |
| −6 | NAD, FLO SCC binding, `check-gateway-api-sweep` Deployment | sweep pod not running → Gateway API CRDs later blocked |
| −5 | FLO (f5-lifecycle-operator) | image pull from FAR/mirror, pull secret |
| −4 / −2 | `CNEManifest`, `CNEInstance` | FLO not running; CRDs missing |
| 0 | `check license` hook: builds `License` from the JWT Secret, waits Active, then CNEInstance Available | see "License" below |
| PostSync | `check post-install` | TMM not Ready / not spread, image pull errors |
| PreDelete / PostDelete | `check pre-uninstall` / `check post-uninstall` | stuck finalizers — see "Uninstall" |

## First move for any failure

```
roksbnkargoctl status            # Application sync/health, operation message, check Jobs
roksbnkargoctl diagnose          # writes diagnostics/<time>/ — read summary.md first
```

Read the **check Job logs** before anything else: each check prints what it
tested and a final JSON line with the verdict. They are written to be the
diagnosis, not a symptom.

## Known failures and what they mean

- **pre-install: `<node>: repo.f5.com:443 ... i/o timeout`** — that node has no egress
  (no public gateway on its subnet) or a security group/ACL blocks 443. In
  disconnected mode, the target should be the mirror, not FAR: check
  `registry.source`.
- **pre-install: FLP `:8443` unreachable** — the transit gateway connection for the
  FLP VPC or the cluster VPC is missing, or the FLP security group does not allow
  the cluster's CIDRs on 8443. `roksbnkargoctl flp status`.
- **TMM pods Pending, `Insufficient hugepages-2Mi`** — the CNEInstance is not
  `deploymentSize: Tiny` (someone edited Git). ROKS runs only Tiny; `check post-install`
  names this. Multiple TMM replicas do NOT need ReadWriteMany storage at Tiny.
- **pre-install: existing BNK found** — a previous install (roksbnkctl or this tool)
  left FLO, CRs or license secrets. Uninstall it properly; do not delete namespaces
  by hand while F5 finalizers are pending.
- **ImagePullBackOff, `403` from repo.f5.com** — the FAR key is the wrong one (EA vs
  GA). The GA key is `f5-far-auth-key.tgz`.
- **License stuck `Device registration in progress`** (FLP mode) — known intermittent
  product behaviour; recovery is uninstall + install, not editing the CR.
- **License never Active, connected mode** — nodes cannot reach
  `product.apis.f5.com` / `product-s.apis.f5.com`, or the JWT is expired/wrong.
- **CNEInstance never `Available`** — on 2.4 the controller runs no resource
  controllers until the License is Active; check License first.
- **Gateway API CRDs rejected by admission** — the `check-gateway-api-sweep`
  Deployment was not running when FLO's crd-installer ran. Check its log.
- **Argo CD: `the server could not find the requested resource` on CNEInstance** —
  FLO's CRDs did not install; FLO's wave failed first.

## Uninstall

`roksbnkargoctl uninstall` deletes the Application. Argo CD runs the PreDelete
check (drains F5 CRs while FLO still runs, CNEInstance last), deletes what it
owns, then the PostDelete check (license secrets, namespaces, stuck F5
finalizers). The CLI then removes the Secrets it wrote, the trusted profile and
the cluster registration.

- **Application stuck deleting** — the PreDelete hook failed. `uninstall` collects every
  uninstall check's log while the Application is being deleted, into
  `diagnostics/uninstall-<time>/` (read `summary.md`), including the PreDelete pod's,
  which Argo CD deletes with the Application.
- **Git push fails `SSH HOST KEY MISMATCH`** — stop: the server's key changed or someone is
  in the path. **`is not a known host`** — set `git.known_hosts_file`.
- **Namespace `f5-bnk` Terminating** — an F5 finalizer whose controller is gone.
  `check post-uninstall` strips only `f5.com`/`f5net.com` finalizers. Never strip
  `kubernetes.io/*` finalizers.
- **F5 CRDs remain after uninstall** — by design (deleting a CRD deletes every CR
  and can hang namespaces). They are listed by `check post-uninstall`.

## Rules for agents

- Read-only first: `status`, `diagnose`, `flp status`, `registry verify`, the files
  above. Change things only as the persona allows.
- Never print or copy secret values: the IBM API key, `ARGOCD_AUTH_TOKEN`, the Git
  token, the FAR key, the JWT. `manifests/direct/` is already redacted — keep it so.
- `install`, `uninstall`, `flp up/down`, `argocd up/down` and `registry replicate`
  change real infrastructure. Journal the operator's consent before running them.
- BNK has no in-place upgrade. A version or mode change is uninstall + install.
- Report faithfully: quote the check's own verdict line; say what you did not check.
