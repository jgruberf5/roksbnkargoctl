# Checks reference

Every gate in a roksbnkargoctl install is a container that runs in your ROKS
cluster, deployed by the same Argo CD Application that installs BNK. This chapter
is the reference for that container: what image it is, where and as whom it runs,
how it reports, and — mode by mode — every check it makes, what makes it pass or
fail, and the flags it takes. After reading it you will be able to read any check
log, tell a real failure from a transient one, and know which knob (if any) to
turn.

## The image

| Property | Value |
|---|---|
| Image | `ghcr.io/jgruberf5/roksbnkargoctl-check` (public) |
| Contents | One static Go binary, `/check`, built with the standard library only (CI fails the build if the binary imports anything else), plus the CA bundle at `/etc/ssl/certs/ca-certificates.crt` that the node probe verifies TLS chains against |
| Base | `scratch` — no shell, no package manager |
| User | `65532:65532` |
| Architectures | `linux/amd64`, `linux/arm64` |
| Tags | `:vX.Y.Z` and `:latest` for a release, `:dev` for `main`, `:pr-<N>` for a same-repository pull request |

A release build of roksbnkargoctl uses the image tag equal to its own version; any
other build uses `:dev`. You can override the source image with the `check.image`
key in `config.yaml` (for example a `:pr-<N>` image to prove a check fix on a live
cluster before it merges).

Whatever the source, `render` **resolves the tag to a digest** and writes
`ghcr.io/jgruberf5/roksbnkargoctl-check@sha256:…` into every manifest. The pods use
`imagePullPolicy: IfNotPresent`, and with a mutable tag a node keeps running
whatever it cached first; pinning the digest means ROKS runs exactly the check you
rendered and reviewed. A new check image also changes the render's run id, which
re-rolls the node probes.

In `registry.source: mirror`, the check image is part of the mirrored bill of
materials. The digest is still resolved **upstream** (on ghcr.io), and `render`
then requires that exact digest to exist in the mirror. If the mirror holds no copy
or an older one, render stops:

```text
the mirror has an older copy (sha256:…) of the check image, not sha256:… (ghcr.io/jgruberf5/roksbnkargoctl-check:…): run `roksbnkargoctl registry replicate` so the cluster runs the current check
```

If the operator host cannot reach ghcr.io at all, the mirror's own digest is used
and `render` warns that its currency could not be checked.

## Where and as whom it runs

The check container runs **only in ROKS** — never on the Argo CD hub and never on
your workstation. Everything it needs is created out of band by `install`, before
the first sync, so that it outlives the Application (the PostDelete check runs
after Argo CD has deleted everything it owns):

| Object | Name |
|---|---|
| Namespace | `roksbnkargoctl-check` |
| ServiceAccount | `check` |
| ClusterRole + ClusterRoleBinding | `roksbnkargoctl-check` (rules in [Appendix D](./appendix-d-security.md)) |
| SCC binding | `system:openshift:scc:privileged:roksbnkargoctl-check:check` (host network for the node probe, host path for the mirror CA installer) |
| Subscription JWT Secret | `bnk-license-jwt` (key `jwt`) |
| Single-certificate source Secret | `bnk-single-cert-source` (single-certificate mode, issuer `ca` or `provided` only) |

Every check pod has the same hardened container spec: `runAsNonRoot`,
`seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`,
`readOnlyRootFilesystem: true`, all capabilities dropped, requests `10m` CPU /
`32Mi` memory, limit `128Mi` memory. In mirror mode with a pull secret, the pods
reference the mirror pull secret in `roksbnkargoctl-check`.

The workloads Argo CD syncs from Git:

| Mode | Kind | Name | When |
|---|---|---|---|
| `node-probe` | DaemonSet | `check-node-probe` | Sync wave −18 |
| `pre-install` | Job (Sync hook) | `check-pre-install` | Sync wave −18 |
| `cert` | Job (Sync hook) | `check-cert` | Sync wave −10, single-certificate mode only |
| `cert-manager-ready` | Job (Sync hook) | `check-cert-manager-ready` | Sync wave 1, cert-manager mode only |
| `gateway-api-sweep` | Deployment | `check-gateway-api-sweep` | Sync wave −6 |
| `license` | Job (Sync hook) | `check-license` | Sync wave 10 |
| `post-install` | Job (PostSync hook) | `check-post-install` | after the sync |
| `pre-uninstall` | Job run by `uninstall`, and PreDelete hook | `check-pre-uninstall-cli`, `check-pre-uninstall` | before the Application is deleted |
| `post-uninstall` | Job run by `uninstall`, and PostDelete hook | `check-post-uninstall-cli`, `check-post-uninstall` | after pruning |

Every hook Job has `backoffLimit: 0`, `restartPolicy: Never`, an
`activeDeadlineSeconds`, and hook-delete policy `BeforeHookCreation`: a failed Job
and its log stay in the cluster until the next sync re-creates it, which is exactly
when you need to read it.

## The output contract

Every mode reports the same way, so one reading habit covers all nine.

- **stderr** carries human-readable progress, one finding per line, marked by
  severity:

  ```text
  [PASS] openshift-version: OpenShift 4.21.31 (>= 4.16)
  [FAIL] zones: schedulable workers span 2 zone(s) [us-south-1=2 us-south-2=1]; 3 required
  [WARN] repo.f5.com:443: UNREACHABLE: tcp connect failed (refused, filtered, or no route): …
  [INFO] secrets: no --require-secret given
  ```

  A run ends with `<mode> passed`, or `<mode> FAILED:` followed by every failing
  finding indented.

- **stdout** carries exactly **one JSON line**, printed last:

  ```json
  {"mode":"pre-install","version":"v…","ok":false,"started":"…","durationSeconds":41.2,
   "findings":[{"check":"zones","severity":"fail","detail":"…"}],"data":{…},"error":"…"}
  ```

  `findings` holds every `pass`/`fail`/`warn`/`info` line; `data` holds structured
  extras (for example `probeReports` from pre-install); `error` is set when the mode
  could not run at all (for example no in-cluster credentials). `ok` is false when
  any finding failed or `error` is set.

- **Exit status**: `0` pass, `1` fail, `2` usage error (unknown mode, bad flag).
  `check <mode> -h` prints the mode's flags and exits `0`; `check version` prints
  the version.

`roksbnkargoctl status` lists the check Jobs and their phase; `roksbnkargoctl
diagnose` saves every check pod's log and quotes each pod's final JSON line in
`summary.md` (see [status and diagnose](./09-status-and-diagnose.md)).

### Flags from the environment

Every flag of every mode can also be set from an environment variable named
`CHECK_<FLAG>` — upper case, dashes as underscores. `--bnk-namespace` is
`CHECK_BNK_NAMESPACE`, `--min-zones` is `CHECK_MIN_ZONES`. A command-line value
overrides the environment. For repeatable flags (`--require-secret`, `--target`,
`--drain-group`, `--delete-namespace`, and `cert`'s `--namespace`, `--dns` and `--ip`) the first command-line value replaces the
environment's list rather than appending to it.

Two modes also read downward-API variables directly: `RUN_ID`, `NODE_NAME`,
`POD_NAME` and `POD_NAMESPACE` provide the defaults of the flags of the same
meaning.

The defaults below are the binary's. Where the rendered manifest passes a
different value, the "Rendered" column says so — that is what actually runs.

## pre-install

**When:** Sync hook `check-pre-install`, wave −18, `activeDeadlineSeconds` 1200.
Nothing of BNK is applied until it passes: cert-manager, FLO and the CNEInstance
are all in later waves.

It runs every static check first and reports all problems in one run rather than
stopping at the first — a hook that fails five times for five reasons costs five
syncs.

| Check | Passes when | Fails when |
|---|---|---|
| `openshift-version` | `clusterversions.config.openshift.io/version` reports a version ≥ `--min-openshift` (from `status.desired.version`, else the latest history entry) | the object cannot be read ("is this OpenShift?"), the version does not parse, or it is older |
| `workers` | at least `--min-workers` nodes are workers, schedulable and Ready | fewer. A worker is a node labelled `node-role.kubernetes.io/worker`, or one carrying no role label at all (ROKS labels some workers both master and worker, so `master` is never used to exclude). Cordoned, `NoSchedule`/`NoExecute`-tainted and not-Ready nodes are listed as an `[INFO]` "not counted" line |
| `zones` | the counted workers span at least `--min-zones` zones (`topology.kubernetes.io/zone`, falling back to `failure-domain.beta.kubernetes.io/zone`) | fewer; the finding lists workers per zone |
| `existing-bnk` | no FLO Deployment, CNEInstance, License, Terminating BNK namespace or CWC license secret exists | any of those is found (see below) |
| `secrets` | every `--require-secret` Secret exists and has data | one is missing or empty: "`roksbnkargoctl install` writes these out of band; they are never in Git" |
| `storageclass` | a default StorageClass exists (annotation `storageclass.kubernetes.io/is-default-class=true`, or the beta annotation), and `--storage-class`, if given, exists | no default class, or the named class is missing |
| `tmm-storage` | informational only | never fails |
| `node-probe` | every node the probe DaemonSet intends to run on has reported a fresh verdict and every target was reachable from every node | any node/target pair unreachable, an unreadable report, or a node that never reported |

**`existing-bnk`.** A leftover from a previous install fails the *next* install, not
its own teardown: a live FLO or CNEInstance collides, a stale License or CWC
license secret makes licensing find the old activation and never re-activate, and
a Terminating namespace refuses every create. The check looks for:

- an FLO Deployment — by label `app.kubernetes.io/name=f5-lifecycle-operator`
  anywhere, or by a name containing `f5-lifecycle-operator` in the BNK namespace;
- any CNEInstance or License (a missing CRD counts as none);
- `f5-bnk` or `f5-utils` in phase `Terminating`;
- any of the 34 CWC license secrets, by exact name, in the utils namespace.

A **re-sync** of the same Application must not trip this: if the FLO found is
tracked by the Argo CD Application named by `--argocd-app` (annotation
`argocd.argoproj.io/tracking-id` starting `<app>:`, or label
`app.kubernetes.io/instance=<app>`), the whole check is skipped with an `[INFO]`.

**`tmm-storage`.** There is deliberately **no** ReadWriteMany requirement for
several TMM replicas. At `deploymentSize: Tiny` — the only size ROKS runs and the
only size this tool renders — TMM mounts no PVC, so any StorageClass serves any
replica count (see [Appendix C](./appendix-c-sizing.md)). With `--tmm-replicas`
greater than 1 the check prints an `[INFO]` saying so; it is not a gate.

**Waiting for the node probes.** This is the part that takes time, and two design
rules shape it:

1. **Only fresh verdicts count.** The probe DaemonSet's pod template carries the
   render's run id, so a changed config rolls every probe pod. But a re-sync of an
   *unchanged* config renders the same run id, so the pods would still hold the
   verdict of the previous sync — possibly from before a network fix, or before a
   network break. pre-install therefore **deletes every probe pod first**, records
   their UIDs, and afterwards counts only pods it did not see at start, created no
   earlier than its own start (with a 30-second clock-skew allowance), from the
   DaemonSet's current template generation, whose report carries this run's id.
2. **Coverage is required, not best-effort.** Two nodes pass, the third (which
   would have failed) has not reported: a summary of what exists reads "2/2
   reachable" and passes blind on exactly the per-zone break the probe exists to
   catch. So pre-install waits for a report from every node the DaemonSet reports
   in `status.desiredNumberScheduled`, and a node that never reports fails the
   check: "a node that never reports is indistinguishable from one that would have
   failed".

On failure it lists every `node -> target: error` pair and explains the split:
`dns=FAILED` is a resolver problem; `tcp failed` is routing (transit gateway,
security groups, overlapping prefixes); a per-zone split means a subnet or security
group fixed in only some zones. It always logs a per-target `n/m nodes reachable`
summary.

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--bnk-namespace` | `f5-bnk` | `bnk.namespace` | namespace of FLO and the CNEInstance |
| `--utils-namespace` | `f5-utils` | `bnk.utils_namespace` | namespace of CWC and the License |
| `--min-openshift` | `4.16` | `4.16` | minimum OpenShift version |
| `--min-zones` | `3` | — | zones the schedulable workers must span |
| `--min-workers` | `3` | — | schedulable Ready workers |
| `--require-secret` | none | see below | `namespace/name` of a Secret that must exist; repeatable or comma-separated |
| `--tmm-replicas` | `1` | `bnk.tmm_replicas` | TMM replicas (informational) |
| `--storage-class` | `""` (default class) | `bnk.storage_class` | a StorageClass that must exist |
| `--argocd-app` | `""` | `argocd.application` | Application whose own objects are a re-sync, not a stale install |
| `--probe-daemonset` | `check-node-probe` | `check-node-probe` | probe DaemonSet name |
| `--probe-namespace` | this pod's namespace | `roksbnkargoctl-check` | probe DaemonSet namespace |
| `--run-id` | `$RUN_ID` | the render's run id | accept only probe reports carrying this id |
| `--skip-probe` | `false` | — | do not wait for node probes |
| `--timeout` | `10m` | `15m` | how long to wait for every probe pod to report |

The rendered `--require-secret` list is: the pull secret (`far-secret`, or
`mirror-secret` with a mirror that needs credentials) in `f5-bnk` and `f5-utils` —
plus, in mirror mode, in `roksbnkargoctl-check` and (when cert-manager is
installed) `cert-manager`; `roksbnkargoctl-check/bnk-license-jwt`; in
disconnected mode `f5-utils/licenseserver-rootca`; and in single-certificate mode with
issuer `ca` or `provided`, `roksbnkargoctl-check/bnk-single-cert-source`. With one
namespace, `f5-utils` here reads `bnk.namespace`, and each Secret is listed once.

## node-probe

**When:** DaemonSet `check-node-probe`, wave −18, alongside pre-install. One pod on
every node (it tolerates every taint), on the **host network** with
`dnsPolicy: Default` — the node's own network and resolver, which is what the
kubelet uses to pull images. A probe from your workstation would return a
confident green for a mirror the workers cannot route to; a probe from a pod
network might differ from the kubelet's path. One pod per node on ROKS means every
zone, and a transit gateway attachment or security group that is right in two
zones and wrong in the third is exactly the failure this catches.

For each target it records, separately:

| Stage | Values | Meaning of a failure |
|---|---|---|
| DNS | `ok`, `FAILED`, `skipped-ip` | the name does not resolve from this node |
| TCP | `ok`, `FAILED`, `skipped` | refused, filtered or no route |
| TLS handshake | `ok`, `FAILED` (TLS targets only) | the port answers TCP but does not speak TLS, or rejected the client |
| Verified | `yes`, `no: …`, `skipped (insecure)` | the chain is not trusted; the leaf subject and issuer are still reported |

The handshake always completes *without* verification first, so "TLS works but the
chain is not trusted" is reported as that, with the certificate subject, rather
than collapsed into a handshake failure that hides who answered.

**It retries.** A transit gateway attachment is asynchronous: routes are programmed
some time after it reports attached, and a single probe ~73 s after attach once saw
both targets unreachable on a path that was healthy minutes later. So a failing
target is retried every `--interval` for `--window`; success ends the loop at
once, so a healthy path costs one attempt.

**It never fails its own pod.** An unreachable target is a `[WARN]`, not a `[FAIL]`:
a non-Ready DaemonSet would collapse "this node cannot reach the mirror" into "the
DaemonSet is unhealthy", losing the detail. The verdict is data: the pod writes a
JSON report to its own annotation `roksbnkargoctl.io/probe-result`
(`runId`, `node`, `pod`, `time`, `ok`, and per-target results), retrying the patch
up to 10 times, then idles. pre-install is the gate that turns the report into a
failure. Only a failure to *publish* exits the pod with status 1, so it restarts
and tries again rather than idling unseen.

The rendered targets:

| Configuration | Targets |
|---|---|
| `registry.source: far` | `<registry.far_host>:443` (TLS), `ghcr.io:443` (TLS, the check image), and `quay.io:443` (TLS) when cert-manager is installed |
| `registry.source: mirror` | `<mirror host>:<port or 443>` (TLS; unverified when `registry.mirror.ca_file` is set) |
| `bnk.mode: connected` | also `product.apis.f5.com:443` and `product-s.apis.f5.com:443` (TLS) |
| `bnk.mode: disconnected` | also the FLP `<ip>:8443` (TLS, unverified: the FLP is self-signed) |

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--target` | none | per the table above | `host:port[,tls][,insecure]`; repeatable. A bare host means port 443; `https://host:port` implies `tls`; `insecure` implies `tls`. In the environment, several targets are separated by spaces or semicolons |
| `--window` | `180s` | `180s` | keep retrying a failing target this long |
| `--interval` | `10s` | — | between attempts |
| `--dial-timeout` | `6s` | — | per-attempt DNS/TCP/TLS timeout |
| `--insecure-skip-verify` | `false` | — | do not verify any chain (subjects still reported) |
| `--ca-file` | `""` | — | extra PEM roots to trust |
| `--run-id` | `$RUN_ID` | the pod annotation `roksbnkargoctl.io/run-id` | recorded in the report |
| `--node-name` | `$NODE_NAME` | downward API | this node |
| `--pod-name` | `$POD_NAME` | downward API | this pod (the annotation target) |
| `--pod-namespace` | `$POD_NAMESPACE` | downward API | this pod's namespace |
| `--idle` | `true` | — | after publishing, sleep until terminated |

The DaemonSet uses `maxUnavailable: 100%`, so a new run id replaces every probe pod
at once.

## cert-manager-ready

**When:** Sync hook `check-cert-manager-ready`, wave 1 — after the cert-manager
chart (wave 0) and before the issuer chain (2 to 4). `activeDeadlineSeconds` 720.
Not rendered in single-certificate mode, which has no cert-manager ([cert](#cert)).

Argo CD moves to the next wave once cert-manager's Deployments are healthy, but
Deployment health is not webhook readiness: the cainjector may not yet have written
the new CA into the webhook configuration, and the ClusterIssuer apply fails
`x509: certificate signed by unknown authority`. A first install won that race on
the test cluster; a reinstall lost it. This check closes the race.

It **server-side dry-run creates** a ClusterIssuer named
`roksbnkargoctl-webhook-probe` (self-signed) every `--interval`. A dry run creates
nothing, but it passes through the admission webhook.

| Outcome | Result |
|---|---|
| admitted (or "already exists") | `[PASS] cert-manager-webhook` |
| refused with a "not yet" error: HTTP 404 or 5xx, a transport error, or a message containing `failed calling webhook`, `x509`, `connection refused`, `no endpoints available` or `context deadline exceeded` | retried |
| refused for any other reason (for example 403 from RBAC) | fails at once: "refused for a reason retrying will not fix" |
| still not admitted at `--timeout` | fails, quoting the last error |

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--timeout` | `10m` | `10m` | how long to wait for the webhook |
| `--interval` | `5s` | — | between dry-run attempts |

## cert

**When:** Sync hook `check-cert`, wave −10, `activeDeadlineSeconds` 600; only when
`bnk.certificates.mode: single`
([Single certificate](./07-the-application.md#single-certificate)). It runs after the
pre-install check (−18) and before the FLO chart (0), so the Secret FLO mounts exists in
every BNK namespace before any component starts.

It makes sure one `kubernetes.io/tls` Secret (`tls.crt`, `tls.key`, `ca.crt`) named
`--secret-name` is current in every `--namespace`:

1. **Gets the signer.** `self-signed`: reads the CA kept in
   `<--state-namespace>/<--secret-name>-ca`, or generates one (subject from the flags, CN
   `--ca-common-name`) when it is missing, unreadable, not a CA, or within
   `--renew-before` of expiry. `ca`: reads `tls.crt` and `tls.key` from `--source-secret`
   and requires a CA certificate whose key matches, valid now and for longer than
   `--renew-before` (`[FAIL] ca` otherwise, and nothing is written). `provided`: reads `tls.crt`, `tls.key`
   and `ca.crt` from `--source-secret` and verifies them (below).
2. **Decides whether to keep the current Secret.** It keeps it when, in every namespace,
   the annotation `roksbnkargoctl.io/single-cert-spec` matches the digest of the current
   settings, the key matches the certificate, it is not within `--renew-before` of expiry,
   and it is signed by the current CA with that CA as `ca.crt` (`provided`: it is exactly
   the source certificate). Otherwise it records why. A kept copy is written into every
   namespace, so they end up identical.
3. **Issues or copies.** A new certificate gets a fresh key, the subject from the flags with
   CN `--common-name`, F5's DNS names for every namespace plus `--dns`, the `--ip`
   addresses, server and client auth, and a lifetime of `--validity` capped at the CA's
   expiry. `provided` copies your certificate unchanged.
4. **Writes** the Secret into each namespace by server-side apply, with the digest
   annotation.
5. **Restarts** every pod in those namespaces that mounts the Secret (directly or through
   a projected volume), when a certificate already there was replaced: components load it
   at start, and a component left on the old CA fails mTLS against the others (DSSM's
   Redis did, live). Nothing restarts on the first issue or an unchanged sync.

The `provided` verification fails the check (`[FAIL] provided`) when the key does not
match `tls.crt`, `tls.crt` does not verify against `ca.crt` at the current time for both
server and client auth (every certificate in `ca.crt` is a trust anchor, as it is to the
components that read it; any after the first in `tls.crt` serve as intermediates), or it
does not cover every DNS name and `--ip` address BNK uses;
the message lists up to eight missing names and how many more.

| Finding | Meaning |
|---|---|
| `[PASS] ca` | `self-signed CA <CN> kept (…)` or `generated (…)`, with its expiry |
| `[PASS] certificate` | `kept: valid until <date>, issued for these settings`; `issued <CN>, valid until <date>, <n> DNS names (<why>)`; or `copying the provided certificate (<why>)` |
| `[PASS] secret` | `<ns>/<name> is current`, one per namespace |
| `[FAIL] ca` | issuer `ca`: the source is not a CA, its key does not match, or it is not valid for longer than `--renew-before` |
| `[FAIL] secret` | the Secret could not be written into that namespace |
| `[FAIL] provided` | the provided certificate failed verification |
| `[PASS] restart` | `<ns>: restarted <n> pods that mount the replaced certificate` |
| `[FAIL] restart` | a pod could not be listed or deleted; restart it by hand |

`<why>` is one of: `<ns>/<name> missing`, `the settings changed`, `<ns>/<name>: <parse
error>`, `it expires <date>, within the renewal window`, `<ns>/<name> is not signed by the
current CA`, `<ns>/<name> is not the provided certificate`.

It needs `create` on Secrets, which the check ClusterRole carries only in this mode.

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--issuer` | `self-signed` | `bnk.certificates.issuer` | `self-signed`, `ca` or `provided` |
| `--secret-name` | `bnk-single-cert` | `bnk.certificates.secret_name` | the Secret FLO mounts (`global.certmgr.secretName`) |
| `--namespace` | none (required) | `bnk.namespace`, plus `bnk.utils_namespace` when it differs | a BNK namespace to write the Secret into; repeatable or comma-separated |
| `--source-secret` | none | `roksbnkargoctl-check/bnk-single-cert-source` for `ca` and `provided` | `namespace/name` of your CA or certificate |
| `--state-namespace` | `$POD_NAMESPACE` (`roksbnkargoctl-check`) | — | where a self-signed CA is kept |
| `--common-name` | `f5net` | `bnk.certificates.common_name` | the certificate's CN |
| `--ca-common-name` | `f5net-ca` | `bnk.certificates.ca_common_name` | a generated CA's CN |
| `--organization`, `--organizational-unit` | `F5 Networks`, `PD` | `bnk.certificates.organization`, `.organizational_unit` | subject O, OU |
| `--country`, `--state`, `--locality` | `US`, `Washington`, `Seattle` | `bnk.certificates.country`, `.state`, `.locality` | subject C, ST, L |
| `--dns` | none | each of `bnk.certificates.extra_dns_names` | an extra DNS name; repeatable or comma-separated |
| `--ip` | none | each of `bnk.certificates.ip_addresses` | an IP address SAN; repeatable or comma-separated |
| `--key-type` | `rsa` | `bnk.certificates.key_type` | `rsa` or `ecdsa` (P-256) |
| `--key-bits` | `4096` | `bnk.certificates.key_bits` | RSA key size; at least 2048 |
| `--validity` | `87600h` (3650 days) | `bnk.certificates.validity_days` × 24h | lifetime of an issued certificate and of a generated CA |
| `--renew-before` | `720h` (30 days) | `bnk.certificates.renew_before_days` × 24h | reissue when this close to expiry |

## gateway-api-sweep

**When:** Deployment `check-gateway-api-sweep`, wave −6, one replica.

OpenShift ships a ValidatingAdmissionPolicy,
`openshift-ingress-operator-gatewayapi-crd-admission`, that refuses writes to
Gateway API CRDs from anything but its ingress operator. FLO's crd-installer
installs Gateway API CRDs, so without removing the policy the install stalls. The
ingress operator re-creates the policy, hence a loop: every `--interval`, the sweep
deletes the policy's **binding first** (without it the policy is inert, which keeps
the window in which a half-removed pair still blocks as short as possible), then
the policy, until both of these CRDs exist:

- `gateways.gateway.networking.k8s.io`
- `gatewaysettings.gateway.k8s.f5.com`

Then it passes, reporting how many deletions it made, and idles.

**Why a Deployment, not a hook.** The CRDs it waits for are installed by FLO in a
*later* wave (0, the charts' wave), and Argo CD will not start a wave until the previous wave's hooks
have finished. A blocking hook would deadlock the sync. A Deployment is healthy as
soon as it runs, so Argo CD moves on, and the sweep keeps working in the background.
It idles only on success: a failed sweep exits 1 and the Deployment restarts it,
which keeps sweeping — the right thing while FLO is late.

| Outcome | Result |
|---|---|
| both CRDs exist | `[PASS] gateway-api-crds` |
| still missing at `--timeout` | fails: "is FLO installed and its crd-installer running?" |

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--interval` | `5s` | — | between sweeps |
| `--timeout` | `20m` | — | give up after this long |
| `--idle-after` | `true` | `true` | after the CRDs exist, sleep until terminated instead of exiting |

## license

**When:** Sync hook `check-license`, wave 10 — after the CNEInstance (8).
`activeDeadlineSeconds` 4200 (70m): the sum of its four waits plus 5 minutes, so the
check reports which wait ran out rather than dying of `DeadlineExceeded`. This is the gate that decides whether the sync
succeeds: without custom health checks in your Argo CD, F5 custom resources look
healthy the moment they are created, and this hook is what waits for BNK to
actually come up.

**Why the License is built here and not stored in Git.** On BNK 2.4 the License CRD
requires the subscription JWT **inline** in `spec.jwt`; there is no Secret
reference. A License in Git would publish the JWT. So `install` writes the JWT into
Secret `roksbnkargoctl-check/bnk-license-jwt`, and this hook builds the License
from it at sync time. The License CRD is also installed at runtime by FLO's
crd-installer rather than by the chart, which a synced object could not wait for.

Steps, each a finding:

| Check | Passes when | Fails when |
|---|---|---|
| `jwt-secret` | the Secret and key exist and are non-empty (the length is logged; the value never is) | missing Secret, missing key, bad base64, or empty |
| `license-spec` | the License renders | unknown mode, or `f5licenseproxy` without `--flp-url` |
| `license-crd` | CRD `licenses.k8s.f5net.com` is `Established` within `--crd-timeout` | it never appears: "FLO's crd-installer has not run" |
| `flp-rootca` (disconnected only) | Secret `licenseserver-rootca` in the License namespace holds a PEM certificate under `licenseserver-rootca.txt` | it does not, within `--crd-timeout` |
| `cwc-restart` (disconnected only) | informational/warning | never fails |
| `license-apply` | the License server-side applies | refused for a non-retryable reason, or still refused after `--apply-retry` |
| `license-active` | `status.state` is `Active`, or condition `LicenseActive=True`, within `--timeout` | not Active in time; the hint names CWC logs and node egress to F5 licensing (connected) or the FLP, its root CA and CWC logs (disconnected) |
| `cneinstance-available` | CNEInstance `<cne-namespace>-f5-cne-controller` has `Available=True` within `--cne-timeout` | not Available in time; the last condition is quoted |

The License it builds (`k8s.f5net.com/v1`, name `bnk-license`, in `f5-utils`):

```yaml
spec:
  jwt: "…"                       # from the Secret, never logged
  operationMode: connected        # or f5licenseproxy
  # disconnected mode only:
  teemCertUrl: https://<flp>:8443/license-proxy/v1
  teemEntitlementUrl: https://<flp>:8443/license-proxy/v1
  teemInitialConfigUrl: https://<flp>:8443/license-proxy/v1
  licenseProxyServerRootCaPath: /etc/cm20/licenseserver-rootca/licenseserver-rootca.txt
```

Design notes:

- **The apply is retried.** BNK installs a ResourceQuota
  `f5-single-license-quota` counting `count/licenses.k8s.f5net.com`. Admission is
  refused while the quota's status is uncomputed, and the quota controller only
  sees a new CRD on its resync, so the first install on a cluster gets
  `… is forbidden: status unknown for quota` for up to a few minutes. HTTP 403,
  404, 409, 429, 5xx and "failed calling webhook" are retried until
  `--apply-retry`; anything else is a real error.
- **The FLP CA must be in place before the License points at the proxy.** A License
  pointing at the proxy with an empty `licenseserver-rootca` never reaches Active
  while the apply looks fine. And CWC builds its trust store at pod start, so a CA
  written after CWC started is ignored until it restarts. With `--restart-cwc`
  (default), the hook stamps the CA's hash into the pod-template annotation
  `roksbnkargoctl.io/flp-ca-hash` of Deployment `f5-spk-cwc`, which rolls CWC
  **once per CA** — a re-sync with the same CA does not bounce it.
- **CNEInstance comes after the License.** On 2.4 the CNE controller runs no
  resource controllers until the License is Active, so the order of the two waits
  is the order in which BNK actually comes up.

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--namespace` | `f5-utils` | `bnk.utils_namespace` | License namespace |
| `--cne-namespace` | `f5-bnk` | `bnk.namespace` | CNEInstance namespace |
| `--name` | `bnk-license` | — | License name |
| `--jwt-secret` | `bnk-license-jwt` | `bnk-license-jwt` | Secret holding the JWT |
| `--jwt-secret-namespace` | this pod's namespace | `roksbnkargoctl-check` | namespace of the JWT Secret |
| `--jwt-key` | `jwt` | — | key of the JWT in the Secret |
| `--mode` | `connected` | `connected` or `f5licenseproxy` | also accepts `disconnected` and `flp` as `f5licenseproxy` |
| `--flp-url` | `""` | the FLP URL (disconnected) | F5 License Proxy base URL; `/license-proxy/v1` is appended unless already present |
| `--flp-ca-path` | `/etc/cm20/licenseserver-rootca/licenseserver-rootca.txt` | same | `licenseProxyServerRootCaPath` |
| `--restart-cwc` | `true` | — | roll CWC once per CA so it trusts the FLP CA |
| `--crd-timeout` | `10m` | `10m` | wait for the License CRD (and, disconnected, the CA Secret and CWC Deployment) |
| `--apply-retry` | `5m` | `5m` | keep retrying a refused apply |
| `--timeout` | `15m` | `35m` | wait for License Active |
| `--cne-timeout` | `15m` | `15m` | wait for CNEInstance Available |

## post-install

**When:** PostSync hook `check-post-install`, after every wave has synced (so after
the license hook passed). `activeDeadlineSeconds` 900.

TMM may still be settling when PostSync starts, so the mode re-runs the whole
check quietly every `--interval` until it passes or `--timeout` is near, logging a
one-line "attempt N not yet healthy" each time, and then reports the final attempt
in full.

| Check | Passes when | Fails when |
|---|---|---|
| `deployment-size` | `CNEInstance.spec.deploymentSize` is `Tiny` | anything else (see below) |
| `flo` | each FLO Deployment is `Available` with `availableReplicas` ≥ desired (and > 0) | not found, or not available |
| `cneinstance` | `<bnk-namespace>-f5-cne-controller` is `Available=True` | unreadable or not Available (status, reason and message quoted) |
| `license` | `bnk-license` is Active | unreadable, or its state is quoted |
| `tmm` | Ready TMM pods (`--tmm-selector` in `--tmm-namespace`) equal `--tmm-replicas`, or at least one when that is 0 | a different count (non-Ready pods listed), or none |
| `tmm-zones` | more than one Ready TMM pod spread over at least two zones | several Ready TMM pods all in one zone: "a zone outage takes the data plane down". A single replica is `[INFO]` |
| `pods` | no container in the BNK namespaces is waiting with `ImagePullBackOff`, `ErrImagePull`, `CrashLoopBackOff`, `InvalidImageName` or `CreateContainerConfigError` | any is; each is listed with its message |

**Why post-install requires Tiny.** IBM ROKS can run only `deploymentSize: Tiny`:
every larger size requests hugepages, ROKS workers report `hugepages-2Mi: 0`, and
ROKS has no Machine Config Operator to allocate them. roksbnkargoctl renders Tiny
as a literal, and `config.yaml` has no key to change it. The one way a different
size can reach the cluster is a hand edit in the Git repository — which Argo CD
would sync without complaint. This check catches that, and says to restore
`deploymentSize: Tiny` in Git. See [Appendix C](./appendix-c-sizing.md).

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--bnk-namespace` | `f5-bnk` | `bnk.namespace` | FLO and CNEInstance namespace |
| `--utils-namespace` | `f5-utils` | `bnk.utils_namespace` | License namespace |
| `--tmm-replicas` | `0` (at least one) | `bnk.tmm_replicas` | expected Ready TMM pods |
| `--tmm-selector` | `app=f5-tmm` | — | label selector of TMM pods |
| `--tmm-namespace` | `--bnk-namespace` | — | namespace of TMM pods |
| `--timeout` | `10m` | `10m` | keep re-checking until everything passes or this expires |
| `--interval` | `15s` | — | between attempts |

## pre-uninstall

**When:** `uninstall` runs it as Job `check-pre-uninstall-cli` and waits for it
**before** it deletes the Application — so FLO is still running. The same check is the
PreDelete hook `check-pre-uninstall`, for a delete started from the Argo CD UI; there it
is best effort, because Argo CD 3.5.1 can count the hook finished the moment it creates
it ([argoproj/argo-cd#29100](https://github.com/argoproj/argo-cd/issues/29100)).
`activeDeadlineSeconds` 1200.

**The operator must outlive the objects it has to finalize.** Deleting FLO and the
CNEInstance together leaves the CNEInstance's children with F5 finalizers that
nothing can clear, and the namespace hangs Terminating. So this check drains BNK
while FLO runs, in a fixed order, and deletes the CNEInstance **last**:

```text
1. drain gateway.k8s.f5.com + fic.f5.com CRs   (per namespace, --timeout each)
2. wait until no fic.f5.com IPAM/IPAMRange remains
3. delete the License                           (--license-timeout)
4. delete the CNEInstance and wait for it       (--cne-timeout)
```

| Check | Passes when | Fails / warns when |
|---|---|---|
| `flo` | `[INFO]` FLO is running | `[WARN]` no available FLO: finalizers may not clear |
| `discovery` | `[INFO]` names the resource types to drain | `[WARN]` per API group that could not be discovered |
| `drain` | every drainable F5 CR in the namespace is gone | objects remain after the budget, or every delete was refused for `--refusal-grace` |
| `ipam` | no `fic.f5.com` object remains | some remain |
| `license` | the License is deleted | `[WARN]` only: not gone in time; post-uninstall strips its finalizer |
| `cneinstance` | deleted and finalized while FLO ran | not gone in time, or deliberately not deleted because an earlier step failed |
| `webhooks` | `[INFO]` how many times F5 webhooks were removed | — |

Design notes:

- **Which groups are drained.** Not every F5 group. `k8s.f5.com` holds the
  CNEInstance and the components FLO derives from it; FLO re-creates each within
  seconds while the CNEInstance exists, so they go with the CNEInstance.
  `k8s.f5net.com` on 2.4 is product defaults the webhook refuses to delete or
  re-creates on sight, plus the License, which has its own step. `metrics.f5.com`
  is left to the namespace delete. What remains — `gateway.k8s.f5.com` and
  `fic.f5.com` — covers F5's guide order (use-case CRs, GatewaySettings, Infra,
  License, verify IPAM gone, CNEInstance).
- **Owned objects are not deleted.** A child deleted under a live owner is
  reconciled back, a fight the drain cannot win; it goes with its owner. An object
  re-created by its controller after deletion (a new UID) is left to the namespace
  delete.
- **Deletes are re-issued every poll.** A delete refused once and never retried
  waits out its whole budget for nothing.
- **The F5 validating webhooks are swept continuously.** FLO re-creates
  `f5validate-<ns>` about ten seconds into a teardown, while its backend is already
  going, so every delete it guards is refused with `failed calling webhook`. FLO
  restores the webhook 375–815 ms after removal. So webhooks named `f5validate-*`,
  or served from a BNK namespace, are deleted every 3 seconds **and** on demand by a
  refused delete, which is then retried at once.
- **A refusal is not a denial.** "failed calling webhook" (the backend is gone) is
  transient and retried. "admission webhook … denied the request" (the product
  states a rule, such as "Default TCP parameters CR cannot be deleted!") is
  respected: the object is left to the namespace delete.
- **Giving up.** The drain stops early only when every delete has been refused, with
  nothing finalizing, for the whole `--refusal-grace` — FLO's webhook re-creation
  window is tens of seconds, so one refused pass proves nothing.
- **On failure the CNEInstance is left in place.** Deleting it would orphan exactly
  the objects still waiting. When the check fails, `uninstall` does not delete the
  Application, so FLO keeps running; fix what the check named and run `uninstall`
  again.

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--bnk-namespace` | `f5-bnk` | `bnk.namespace` | FLO and CNEInstance namespace |
| `--utils-namespace` | `f5-utils` | `bnk.utils_namespace` | utils namespace |
| `--drain-group` | `gateway.k8s.f5.com,fic.f5.com` | — | API groups drained before the CNEInstance; repeatable; replaces the default |
| `--timeout` | `4m` | `15m` | drain budget per namespace |
| `--refusal-grace` | `45s` | — | how long every-delete-refused must persist before giving up |
| `--license-timeout` | `3m` | — | wait for the License to go |
| `--cne-timeout` | `10m` | — | wait for the CNEInstance to go |

## post-uninstall

**When:** `uninstall` runs it as Job `check-post-uninstall-cli` after Argo CD has
pruned everything it synced, while any BNK namespace remains (also the PostDelete hook
`check-post-uninstall`). `activeDeadlineSeconds` 1200. It runs in
`roksbnkargoctl-check`, which `install` created out of band precisely so that it is
still there at this point.

| Check | Passes when | Fails / warns when |
|---|---|---|
| `license-secrets` | the 34 CWC license secrets are deleted from the utils namespace, by exact name (absent ones are fine) | any delete errors |
| `namespaces` | the BNK namespaces (and any `--delete-namespace`) are gone within `--timeout` | still present; the namespace's `NamespaceFinalizersRemaining` / `NamespaceContentRemaining` messages are quoted |
| `finalizers` | — | `[WARN]` F5 finalizers were stripped from N objects |
| `crds` | `[INFO]` lists F5 CRDs kept by design | — |
| `cluster-objects` | `[INFO]` lists cluster-scoped F5 objects and the ClusterIssuers `sample-issuer` / `selfsigned-cluster-issuer` left behind | — |

Design notes:

- **This check deletes the BNK namespaces, not Argo CD.** The namespaces carry
  Argo CD's `Delete=false` sync option, so a namespace stuck on an F5 finalizer can
  never hang the Application's deletion before this hook runs.
- **Secrets by exact name.** The CWC secrets survive a teardown and stop the next
  install from licensing. They sit in a namespace with unrelated secrets, so a
  prefix sweep would be a wildcard delete against someone else's data.
- **Finalizers are stripped narrowly, and late.** A namespace may terminate on its
  own for `--finalizer-grace` first (FLO's pods may still be finishing). After that,
  only in `f5-bnk` and `f5-utils`, and only **F5** finalizers are removed: the known
  ones (`k8s.f5.com/CNEInstanceFinalizer`, `k8s.f5net.com/uninstall`,
  `handletmmconfig_inconsistency`) and any whose domain is `f5.com`, `f5net.com` or
  a subdomain of either. Every other finalizer is written back, and the patch
  carries `resourceVersion` so a concurrent writer wins. The scan covers the F5 API
  groups, widening to every namespaced type when the namespace's condition names an
  F5 finalizer on some other kind. An extra namespace such as `cert-manager` is
  waited for, but its finalizers are never touched.
- **CRDs stay.** Deleting a CRD deletes every CR with it, and F5 CRs whose
  finalizer's controller is gone hang their namespace. The FLO and cert-manager charts
  are installed with `crds.keep: true`, so their CRDs carry
  `helm.sh/resource-policy: keep`, which Argo CD honours on delete.

| Flag | Default | Rendered | Meaning |
|---|---|---|---|
| `--bnk-namespace` | `f5-bnk` | `bnk.namespace` | always deleted |
| `--utils-namespace` | `f5-utils` | `bnk.utils_namespace` | always deleted; the CWC secrets live here |
| `--delete-namespace` | none | `cert-manager` when this tool installed cert-manager | extra namespace to delete and wait for; repeatable |
| `--timeout` | `5m` | `15m` | wait for the namespaces to go |
| `--finalizer-grace` | `30s` | — | let a namespace terminate on its own this long before stripping F5 finalizers |

## Running a mode by hand

The binary needs in-cluster credentials, so it only works inside a pod with the
`check` ServiceAccount. You rarely need to: every mode is already a Job, DaemonSet
or Deployment in `roksbnkargoctl-check`, and re-syncing the Application re-runs the
Sync hooks. If you do build a debug pod from one of the rendered Jobs in
`manifests/git/`, keep its image digest and ServiceAccount, and pass flags (or
`CHECK_*` variables) to change only what you are testing.

## See also

- [How an install flows](./02-how-an-install-flows.md) — the waves in context
- [status and diagnose](./09-status-and-diagnose.md) — reading check results
- [Troubleshooting guide](./17-troubleshooting.md) — failures by symptom
- [Appendix C](./appendix-c-sizing.md) — Tiny, TMM replicas and storage
- [Appendix D](./appendix-d-security.md) — the check ClusterRole in full
