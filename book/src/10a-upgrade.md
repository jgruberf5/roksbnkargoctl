# upgrade

`roksbnkargoctl upgrade` moves a running install to a newer BNK 2.4 release, in place. FLO
does the work: the Application gets the new CNEManifest and the CNEInstance's new
`manifestVersion`, and FLO rolls each component to it.

Each roksbnkargoctl build installs one BNK release (`roksbnkargoctl version` names it), and
`self update` never moves between them. To upgrade, download the build for the release you
want, then run `upgrade` with it against the same workspace.

## Usage

```
roksbnkargoctl version          # roksbnkargoctl … for BNK 2.4.3
roksbnkargoctl upgrade -w prod  # BNK 2.4.2 is running; upgrading to 2.4.3
```

| Flag | Default | Meaning |
|---|---|---|
| `--timeout` | 1h15m | How long to wait for the sync, and then for FLO to finish rolling |
| `-y`, `--yes` | | Do not ask for confirmation |

In mirror mode, replicate the new release into the mirror first, with the new build:
`roksbnkargoctl registry replicate …`. The render pulls the new manifest from the mirror.

## What it does

1. Reads the release running in the cluster: the CNEInstance's `spec.manifestVersion`.
2. Refuses unless that release is older than the build's and in the same 2.4 line.
3. Sets `bnk.version` in the workspace to the build's release.
4. Does what `install` does: renders, publishes to Git, updates the Application.
5. Syncs **with prune**, so the old release's CNEManifest (`bnk-<old>`) is removed.
6. Waits until the CNEInstance asks for the new release with every `*Available` condition
   True, and the License is Active.

## What it refuses

| Running | `install` | `upgrade` |
|---|---|---|
| nothing | installs | refuses: use `install` |
| this build's release | re-syncs | re-syncs with prune and waits |
| an older 2.4 release | **refuses: use `upgrade`** | upgrades |
| a newer release | refuses: no downgrades | refuses |
| another BNK line | refuses | refuses |

`install` never changes the running release. Before this check existed, re-running `install`
with a newer build upgraded BNK without being asked, and an older build downgraded it.

If an upgrade is interrupted (a lost connection, a timeout), run `upgrade` again with the same
build. Once the sync has applied the new CNEInstance, the new release counts as running, so
`upgrade` finishes the job: it re-syncs with prune and waits for FLO and the License.

## What to expect

Measured on a three-worker cluster, 2.4.1 → 2.4.2 with one namespace: FLO rolled the CRD
installer, fluentd, the TMM pod manager, f5ingress, coremond and qkview, then TMM one pod at a
time. The CNEInstance reported `Available: False` while it rolled, and every component
Available 3 minutes 38 seconds after the sync started. The License stayed Active throughout.

## See also

- [Running with Argo CD Web Interface](./10b-argocd-ui.md#upgrade-within-24-with-git-and-argo-cd-only): the same upgrade as a one-line Git change and a sync
- [install](./08-install.md)
- [Mirroring into a private registry](./13-registry.md) for `registry replicate`
- [status and diagnose](./09-status-and-diagnose.md) when the roll does not finish
