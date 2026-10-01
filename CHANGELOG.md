# Changelog

Release assets are named for the BNK release the binary installs:
`roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>`. `roksbnkargoctl version`
prints both.

## [0.7.0] - 2026-10-01

Still installs **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)**.

### Added
- Single-certificate mode, F5's "Single Certificate for BNK" (`bnk.certificates.mode:
  single`, #30). No cert-manager: no chart, namespace, issuers or readiness gate, and the
  registry BOM drops cert-manager's 5 artifacts. FLO mounts one `kubernetes.io/tls` Secret
  (`global.certmgr.enabled: false`, `secretName`), which a new `check cert` Sync hook (wave
  −10) writes into every BNK namespace. Issuer `self-signed` (a CA generated and kept in the
  cluster), `ca` (your CA signs it) or `provided` (your certificate, checked for key match,
  chain, validity and every BNK name); your key goes into the cluster, never to Git.
  Defaults are F5's procedure (CN `f5net`, RSA 4096, 3650 days). A sync keeps the Secret
  while it is current and reissues it when it is missing, the settings change, or it is
  within 30 days of expiry. The check gets `create` on Secrets in this mode only.
- One namespace for every BNK component: set `bnk.utils_namespace` to `bnk.namespace`
  (#31). The `init` interview asks for it and for the certificate mode.
- `install` records the namespaces and certificate mode it installed
  (`resolved.installed_layout`) and refuses a different layout until `uninstall`:
  switching in place deleted the utilities namespace, with CWC, RabbitMQ and the License,
  under roksbnkctl on BNK 2.3. `init --refresh` keeps the record.
- Verified live: TODO

## [0.6.3] - 2026-09-30

Still installs **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)**.

### Fixed
- `install` saves the ID of the transit gateway connection and the trusted profile it
  creates as soon as each exists. Before, a failure between creating one and the next
  save (the wait for the connection to attach, or linking the profile) lost the ID, so
  `uninstall --detach-tgw` and `workspaces delete` did not know about it (#28).
- `flp up` saves the license proxy's state before writing the CA file (`--ca-out`). Before,
  a CA file that could not be written returned first and lost every resource ID; if the
  state cannot be saved after `install` creates a resource, the error names the resource
  and the command that removes it.

## [0.6.2] - 2026-09-30

Still installs **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)**.

### Added
- `workspaces delete <name>` (alias `ws delete`, `ws rm`) removes a workspace's directory.
  It refuses while the workspace records an install (its trusted profile), an F5 License
  Proxy or a test Argo CD hub, and names the command that removes each; a transit gateway
  connection `install` created is reported but does not block. `--force` deletes anyway
  and lists what is left without a local record. Deleting the current workspace unsets
  it (#26).

### Fixed
- `init --refresh` dropped the record of the transit gateway connection `install` created,
  so `uninstall --detach-tgw` no longer knew about it. It is kept now.

## [0.6.1] - 2026-09-30

Still installs **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)**.

### Fixed
- Ctrl-C at any question (the `init` interview, a confirmation, the BNK Forge password)
  now stops the command with exit code 130; before, it was ignored until Enter was
  pressed. At the questions other than the password, the end of input (Ctrl-D) stops it
  too, instead of silently taking the default. An interrupted password prompt restores
  the terminal's echo. Nothing is saved by an interrupted `init` (#23).
- Two collector tests left a watch running past their end; they now stop it and wait,
  and a watch that ignores cancellation fails the test (#20).

## [0.6.0] - 2026-09-30

Still installs **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)**. This release makes the tool
usable without a workspace, adds overrides for every setting, checks Argo CD and Git
before anything is changed, and reduces the Git content to what F5's manual install
writes.

### Git holds what the manual install writes
- `render` no longer expands the cert-manager and FLO charts into Git. That was 96 objects
  for one install, 32 of them CRDs. `git.path` now holds about 18 manifests (namespaces,
  the checks, the SCC binding, the NAD, the issuers, `CNEManifest`, `CNEInstance`) and
  `values/cert-manager.yaml` and `values/flo.yaml`.
- The Application has three sources (two without cert-manager): each chart as an OCI Helm
  source from FAR or the mirror (`quay.io` for cert-manager with FAR), reading its values
  file from Git, and the Git path itself, not recursed. Argo CD's resource tree still
  lists every object of both charts, as for any Helm source.
- The charts sync in wave 0. The waves before them are unchanged (−20 to −6); the ones
  after move to 1 (`cert-manager-ready`), 2–4 (issuers), 6 (`CNEManifest`),
  8 (`CNEInstance`) and 10 (`check license`).
- `manifests/charts/<chart>/` shows each chart as Argo CD renders it. It is not published.
- The charts' CRDs keep `helm.sh/resource-policy: keep` (`crds.keep: true`), which Argo CD
  3.5.1 honours on delete, so uninstall still leaves them. FLO's `external-otelsvr-secret`
  now comes from the chart instead of being written by `install`.
- `install` registers each chart registry in Argo CD as an OCI Helm repository, and a
  private-CA mirror's CA as a TLS certificate. **The registry login (the FAR service-account
  key, or the mirror user and password) is now stored in Argo CD on the hub**, on the
  registry it belongs to only; `quay.io` is anonymous. The hub must reach FAR and `quay.io`,
  or the mirror. `uninstall --remove-repo` removes the chart registries too.
- Verified live on OpenShift 4.21.31 with Argo CD 3.5.1, connected mode, charts from an
  Artifactory mirror: install (8m36s), uninstall and reinstall (7m46s), each ending Synced
  and Healthy with nothing OutOfSync and the License Active; the charts' CRDs survived the
  uninstall. A render compared with Argo CD's own had 0 differences.

### Without a workspace
- `cos instances`, `cos buckets --instance`, `cos list --instance --bucket`,
  `cos publish --instance --bucket` and `cos delete --instance --bucket` work from flags
  alone. A bucket's region is discovered.
- `flp up/status/down` take every `flp.vsi` setting as a flag (`--cidr`, `--zone`,
  `--profile`, `--vpc`, …). `registry bom/replicate/verify` take the `registry.mirror`
  settings as flags.
- `forge register` and `forge unregister` register the cluster with F5 BNK Forge. The
  Cloud Credentials Template is created from the IBM Cloud API key if it is missing.
- `--no-workspace` ignores the current workspace. Commands that need one say so.

### Settings
- Every `config.yaml` setting can be overridden by a `ROKSBNKARGOCTL_<KEY_PATH>` variable,
  for example `ROKSBNKARGOCTL_FLP_VSI_CIDR`. Appendix A lists all of them. Precedence is
  flag, then environment, then `config.yaml`, then the default. Overrides are never saved.
- `show` prints the workspace's `config.yaml`. `show --effective` prints what commands use,
  with overrides and defaults applied, and lists the overrides on stderr.

### Bring your own Git
- `export` zips the rendered manifests and the charts' values files, laid out for your
  repository, with instructions. It refuses to include a Secret.
- `install --no-publish` installs from what you committed. It reads the commit the Git
  source resolves to, checks that Argo CD renders exactly `manifests/git` and
  `manifests/charts` at that commit, then syncs that same commit.

### Checked before anything changes
- `init` asks whether you use an existing Argo CD (3.3 or later). If you do, it needs
  `ARGOCD_AUTH_TOKEN` and checks that the server accepts it. It also checks that your Git
  repository is readable. Both checks run before the workspace is saved.
- `install` step 1 checks push access to the repository without pushing anything.
  Previously a missing repository or a bad credential failed at step 7, after the cluster
  had been changed (#18). With `--no-publish`, read access and `git.branch` are enough.

### Container image
- `ghcr.io/jgruberf5/roksbnkargoctl:v0.6.0` (also `:latest`, linux/amd64 and linux/arm64)
  runs the CLI with only Docker: `docker run --rm -it -v "$HOME/.roksbnkargoctl:/work/.roksbnkargoctl" -e IBMCLOUD_API_KEY ghcr.io/jgruberf5/roksbnkargoctl:v0.6.0 status`.

### Fixed
- The linux/arm64 check image contained an x86-64 binary (#17).
- `flp down` left the transit gateway connection attached when it had made it for an
  adopted VPC (`flp.vsi.vpc`). It also gave up while IBM still held a detach in `deleting`;
  it now waits up to 30 minutes.

### Not verified live
- `forge register/unregister`: tested against a fake Forge API only.
- The charts as Helm sources with FAR (only a mirror was used), with a private-CA mirror,
  and in disconnected mode.

## [0.5.0] - 2026-09-29

First release. Installs and uninstalls **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)** on an
existing IBM Cloud ROKS cluster as one Argo CD Application in an existing Argo CD
(3.3 or later). It is a pre-1.0 release, so expect bugs; please report them as issues.

The book: https://jgruberf5.github.io/roksbnkargoctl/ (and as a PDF on this release).

### Getting it
- One-line installers:
  `curl -fsSL https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.sh | sh`
  (Linux, macOS) and
  `irm https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.ps1 | iex`
  (Windows). `ROKSBNKARGOCTL_VERSION` pins a release, `ROKSBNKARGOCTL_BNK_VERSION` picks the
  BNK release (default 2.4.0) and `ROKSBNKARGOCTL_INSTALL_DIR` where it goes; `VERSION` and
  `BNK_VERSION` also work. The checksum is mandatory.
- `self install [--dir D] [--force]` copies the running binary onto your PATH.
- `self update [--version vX.Y.Z] [--check]` updates roksbnkargoctl in place from a GitHub
  release: verified against the release's checksums, then swapped in by a rename (on
  Windows the running binary is moved aside, and restored if the swap fails). It only
  installs the archive for the binary's own BNK release, and never goes back to an older
  release unless `--version` asks for it. This updates the tool, not BNK.

### What it does
- `init` from a `config.yaml` or an interview; one workspace per cluster.
- `render` writes every manifest into the workspace: `manifests/git/` is exactly what is
  published to Git, `manifests/direct/` shows the Secrets (redacted) that go straight to
  ROKS and never to Git.
- `install` attaches the cluster VPC to the transit gateway if needed, creates the IAM
  trusted profile, writes the out-of-band objects into ROKS, registers the cluster with
  Argo CD, publishes to Git, and creates and syncs the Application.
- `uninstall` runs the pre-uninstall check, deletes the Application, runs the
  post-uninstall check, and fails unless BNK's namespaces are gone.
- `status` and `diagnose`; `agent <cli>` starts your own coding agent (`agy`, `claude`,
  `codex`, `gemini`, `aider`, `pi`, `opencode`) in the workspace as the troubleshooter or
  operator persona.
- Connected mode, and disconnected mode through an F5 License Proxy (`flp up/down`).
- FAR, or a private mirror filled by `registry replicate` (Harbor, Artifactory, ICR).
- `cos publish/list/verify` for the FAR auth tarball and the subscription JWT.
- `argocd up/down` builds a test Argo CD hub on a VSI.
- Every check runs in ROKS as the `check` container (a static binary on `scratch`):
  pre-install (including per-node reachability), cert-manager readiness, the Gateway API
  sweep, license, post-install, pre-uninstall and post-uninstall. Nothing runs on the Argo
  CD hub.
- `deploymentSize` is always `Tiny`, the only size ROKS supports; any number of TMM
  replicas runs on ordinary block storage.

### Verified live
On OpenShift 4.21.31 with Argo CD 3.5.1: connected, disconnected (FLP) and mirror-mode
installs; 3 TMM replicas; the Application ends Synced with no resource OutOfSync; uninstall
leaves the cluster clean.

### Known limitations
- **Install and uninstall only.** BNK upgrades are done by changing the manifest version in
  BNK's custom resources, as F5's BNK documentation describes; that is outside this tool.
- Argo CD can skip its own PreDelete/PostDelete hooks
  ([argoproj/argo-cd#29100](https://github.com/argoproj/argo-cd/issues/29100)).
  `roksbnkargoctl uninstall` runs the checks itself and is not affected; deleting the
  Application from the Argo CD UI is, and may leave BNK behind.
- Not yet run live: a non-default `bnk.namespace`, a private-CA or anonymous mirror,
  `flp.external`.
- Only BNK 2.4.0 GA. Later BNK releases will ship as their own binaries.
