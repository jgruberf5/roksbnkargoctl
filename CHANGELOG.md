# Changelog

Release assets are named for the BNK release the binary installs:
`roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>`. `roksbnkargoctl version`
prints both.

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
  (Windows). `VERSION` pins a release and `BNK_VERSION` picks the BNK release (default
  2.4.0). The checksum is mandatory.
- `self install [--dir D] [--force]` copies the running binary onto your PATH.
- `self update [--version vX.Y.Z] [--check]` updates roksbnkargoctl in place from a GitHub
  release: verified against the release's checksums, replaced atomically, and rolled back
  on Windows if needed. It only installs the archive for the binary's own BNK release.
  This updates the tool, not BNK.

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
