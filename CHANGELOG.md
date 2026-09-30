# Changelog

Release assets are named for the BNK release the binary installs:
`roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>`. `roksbnkargoctl version`
prints both.

## [0.6.0] - 2026-09-30

Still installs **F5 BIG-IP Next for Kubernetes 2.4.0 (GA)**. This release makes the tool
usable without a workspace, adds overrides for every setting, and checks Argo CD and Git
before anything is changed.

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
- `export` zips the rendered manifests, laid out for your repository, with instructions.
  It refuses to include a Secret.
- `install --no-publish` installs from what you committed. It first checks that Argo CD
  renders exactly the manifests in `manifests/git`, then syncs that same revision.

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
