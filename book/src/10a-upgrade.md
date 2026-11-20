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
4. Does what `install` does: renders, publishes to Git, updates the Application. The render
   takes the FLO chart version from the new release's manifest, so the Application's FLO
   source moves too (2.4.2 → 2.4.3: `v2.30.0-0.11.4` → `v2.30.0-0.11.8`), and `upgrade` names
   it: `Application: chart f5-lifecycle-operator v2.30.0-0.11.4 -> v2.30.0-0.11.8`.
5. Syncs **with prune**, so the old release's CNEManifest (`bnk-<old>`) is removed. The sync
   applies the FLO chart in wave 0 and the CNEInstance in wave 8, so FLO is upgraded before
   the CNEInstance asks for the new release: the order F5 documents
   ([2.4.2 to 2.4.3 with F5 Lifecycle Operator](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/releases/bnk-upgrade-2.4.2-to-2.4.3-using-flo.html)).
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

Measured on a three-worker cluster with one namespace, 2.4.2 → 2.4.3 (the FLO chart changes),
state polled every 10 seconds. `config.yaml` still said `bnk.version: 2.4.2`; `upgrade` set it:

| After the Application update | What happened |
|---|---|
| 40s | FLO running chart `v2.30.0-0.11.8`; the CNEInstance still on 2.4.2 |
| 1m27s | The CNEInstance asks for 2.4.3; FLO rolls the CRD installer and the other components |
| 2m01s – 3m46s | TMM replaced one pod at a time (never fewer than 2 of 3 ready); CWC `0.0.6` → `0.0.9` |
| 6m17s | Every component Available |
| 6m33s | The prune removes CNEManifest `bnk-2.4.2`; `DSSMAvailable` goes False again |
| 9m35s | Argo CD: sync Succeeded |
| 10m18s | Every component Available again; `upgrade` reports `BNK 2.4.3: every component Available, License Active` |

The License stayed Active throughout. The whole command took 10 minutes 45 seconds. A sync
that has succeeded does not mean BNK is up: `upgrade` waits for the CNEInstance, so it
returned only after DSSM came back.

From 2.4.1 to 2.4.2, where the FLO chart does not change, every component was Available 3
minutes 38 seconds after the sync started.

## See also

- [Running with Argo CD Web Interface](./10b-argocd-ui.md#upgrade-within-24-with-git-and-argo-cd-only): the same upgrade as a one-line Git change and a sync
- [install](./08-install.md)
- [Mirroring into a private registry](./13-registry.md) for `registry replicate`
- [status and diagnose](./09-status-and-diagnose.md) when the roll does not finish
