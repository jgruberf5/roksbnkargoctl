# Running with Argo CD Web Interface

This chapter is for operators who run BNK from the Argo CD web interface rather than from
`roksbnkargoctl`. It covers the one step that still needs the CLI, how to sync and watch an
install in the UI, how to upgrade BNK in place, how to delete the Application, and what a
delete from the UI leaves behind. The button and dialog names are those of Argo CD 3.5.1.

## What the UI can do, and what it cannot

Everything Argo CD owns can be driven from the UI: syncing, upgrading by changing Git, and
deleting. Some of what an install needs is not in Git, so Argo CD cannot create it:

| Not in Git | Why | Created by |
|---|---|---|
| The FAR pull secret, the subscription JWT, and in disconnected mode the FLP root CA | Secret values never go to Git | `roksbnkargoctl install` |
| Namespace `roksbnkargoctl-check` and its RBAC | Must outlive the Application, for the PostDelete hook | `roksbnkargoctl install` |
| The IAM trusted profile | An IBM Cloud object, not a Kubernetes one | `roksbnkargoctl install` |
| The cluster's registration on the hub, the Git repository and chart registry entries | Argo CD settings | `roksbnkargoctl install` |

[Chapter 7](./07-the-application.md#what-is-not-in-git-and-why) lists them all. So the first
install, and any re-install after the Application was deleted, starts with one command.
After that, everything in this chapter is done in the UI.

## Install: create the Application, sync it in the UI

```sh
roksbnkargoctl install --no-sync -w <workspace>
```

`--no-sync` does everything `install` does except the sync: it writes the out-of-band objects,
publishes the manifests to Git, registers the cluster, the repository and the registries,
and creates the Application. It ends with
`sync it from the Argo CD UI, or run roksbnkargoctl install without --no-sync`.

In the UI:

1. **Applications**: the Application (`argocd.application` in the workspace, `bnk-<workspace>`
   unless you named it) shows `OutOfSync` and `Missing`.
2. Open it and press **Sync**. The panel reads *Synchronizing application manifests from*
   your repository. Leave everything as it is:

   | Field | Keep | Why |
   |---|---|---|
   | Revision | as shown (`git.branch`) | |
   | **Prune**, **Dry Run**, **Apply Only**, **Force** | unchecked | Nothing to prune on a first sync |
   | **Sync Options** | as pre-filled from the Application: **Server-Side Apply**, **Respect Ignore Differences**, **Prune Last** checked | The Application needs them; FLO's CRDs are too large for a client-side apply |
   | **Replace** (if your hub allows it) | unchecked | Replace deletes and recreates objects |

3. Press **Synchronize**.

![Sync in progress](images/argocd/sync-in-progress-tree.png)

*The resource tree during the first sync: waves apply in order, and the sync waits at each hook.*

The sync runs in waves, and four checks run as hooks. The status bar names each one while
Argo CD waits for it:

| Wave | What is applied | Hook that runs |
|---|---|---|
| −20 | Namespaces `cert-manager`, `f5-bnk` | |
| −18 | The node-probe DaemonSet | `check-pre-install` (Sync): the cluster can run BNK |
| −6 | FLO's SCC binding, the Gateway API sweep, the NetworkAttachmentDefinition | |
| 0 | cert-manager and FLO charts | |
| 1 | | `check-cert-manager-ready` (Sync) |
| 2–4 | ClusterIssuers and the CA Certificate | |
| 6 | CNEManifest `bnk-<release>` | |
| 8 | CNEInstance `f5-bnk-f5-cne-controller`: FLO now deploys BNK | |
| 10 | | `check-license` (Sync): builds the License, waits for it to be Active |
| after | | `check-post-install` (PostSync) |

![check-license hook](images/argocd/check-license-hook.png)

*`check-license` is the longest wait, while FLO brings BNK up and CWC activates the License.*

The sync is done when **Last Sync** shows `Sync OK` and the Application is `Synced` and
`Healthy`. A first install on a three-worker cluster took 8 to 9 minutes. Two things look
wrong but are not: while BNK's CRDs are still being created the status bar reads
`waiting for healthy state of …CustomResourceDefinition/…` for up to a minute, and the
Application is `OutOfSync` until the CNEManifest and CNEInstance exist.

A hook that fails stops the sync with the hook's message. Its verdict is the last line of
the pod's log: click the Job in the tree, then **Logs**. The
[checks reference](./16-checks.md) explains each finding.

## Watching a running install

| Want to see | In the UI |
|---|---|
| Whether BNK is up | The CNEInstance `f5-bnk-f5-cne-controller` → **Summary** → **Live Manifest**: `status.conditions`, every `*Available` True |
| The release running | The same CNEInstance: `spec.manifestVersion` |
| The license | Is not in the tree: the License is created by the `check-license` hook. `roksbnkargoctl status` reads it |
| What differs from Git | **Diff** |
| Pick up a new commit now, not at the next poll | **Refresh**; **Hard Refresh** also re-renders the charts |

Do not change BNK objects in the cluster by hand. Argo CD shows them `OutOfSync`, and the
next sync puts Git's version back.

## Upgrade within 2.4: change the manifest, sync

FLO upgrades BNK in place when the CNEInstance asks for a newer release. Through the UI, an
upgrade is a change in Git, then a sync.

1. **Change Git.** In `git.path`, the CNEInstance file
   (`015-cneinstance-f5-bnk-f5-bnk-f5-cne-controller.yaml`) sets `spec.manifestVersion`, and
   the CNEManifest file (`014-cnemanifest-bnk-<release>.yaml`) lists every component version
   of the release. The new release's files come from the new build: set `bnk.version` in the
   workspace's `config.yaml` and run `roksbnkargoctl render`, which changes nothing in the
   cluster, Argo CD or Git. Copy `manifests/git/` into the repository, then commit and push.
   With `roksbnkargoctl export`, you get the same files as a zip.
2. **Refresh** the Application. It shows `OutOfSync`: the new CNEManifest is missing, the old
   one is extra, and the CNEInstance differs.
3. **Sync** with **Prune** checked, so the old CNEManifest `bnk-<old>` is removed. Without
   Prune it stays, and the Application stays `OutOfSync`.
4. **Watch the CNEInstance.** FLO rolls the CRD installer, fluentd, the TMM pod manager,
   f5ingress, coremond and qkview, then TMM one pod at a time. `Available` is False while it
   rolls. From 2.4.1 to 2.4.2 every component was Available again 3 minutes 38 seconds after
   the sync started, with the License Active throughout.

### The FLO chart is in the Application, not in Git

The Application installs FLO as a Helm chart from FAR. Its version is in the Application's
sources, so a Git change does not touch it:

| Release | FLO chart |
|---|---|
| 2.4.1, 2.4.2 | `f5-lifecycle-operator` `v2.30.0-0.11.4` |
| 2.4.3 | `f5-lifecycle-operator` `v2.30.0-0.11.8` |

To move FLO, open the Application → **Details** → **Manifest** → **Edit**. In
`spec.sources`, find the source with `chart: f5-lifecycle-operator`, set its
`targetRevision`, and press **Save**. Then **Sync**.

Do not use **History and rollback** to go back to an older release. It re-syncs an older
revision, which asks FLO for a downgrade; downgrades are not supported.

## Delete: what the UI does

Press **Delete**. The dialog asks you to type the Application's name, and offers three
propagation policies:

| Policy | Use | What happens |
|---|---|---|
| **Foreground** (default) | Yes | Argo CD deletes every object it synced, and the Application last |
| **Background** | No need | The same objects, but the Application can go before they do |
| **Non-cascading** | **No** | Only the Application goes; BNK keeps running with nothing managing it |

With Foreground, Argo CD runs the uninstall checks as hooks:

1. `check-pre-uninstall` (PreDelete) drains BNK while FLO still runs: F5 gateway and IPAM
   objects, then the License, then the CNEInstance, so that no F5 finalizer is left without
   the operator that clears it.
2. Argo CD deletes what it synced, in reverse wave order. The BNK namespaces are kept
   (`Delete=false`), and so are the charts' CRDs.
3. `check-post-uninstall` (PostDelete) deletes the CWC license secrets and the BNK
   namespaces, and strips F5 finalizers that stick.

While it deletes, the Application shows `DeletionError … not found` conditions, such as
`jobs.batch "check-pre-install" not found`. They are harmless: each names an object that
was already gone. On the test cluster the Application was gone 2 minutes after **Delete**,
and `f5-bnk` and `cert-manager` with it.

Argo CD 3.5.1 runs PreDelete and PostDelete hooks as best effort
([argoproj/argo-cd#29100](https://github.com/argoproj/argo-cd/issues/29100)): it can declare
a hook finished as soon as it has created it. If the pre-uninstall check is cut short, FLO
can be deleted before it has finalized BNK's objects, and `f5-bnk` stays `Terminating`. If
the post-uninstall check never runs, the BNK namespaces stay. Check **Applications** and the
cluster's namespaces after a delete.

## What a delete from the UI leaves

A delete from the UI removes everything Argo CD synced. It cannot remove what `install`
created outside the Application. Measured after a UI delete on the test cluster:

| Left | Effect |
|---|---|
| Namespace `roksbnkargoctl-check`, its ClusterRole, two ClusterRoleBindings, Secret `bnk-license-jwt` | None while it stays |
| ClusterRoleBinding `roksbnkargoctl-argocd-manager` and its ServiceAccount in `kube-system` | The hub keeps cluster-admin on the cluster |
| The cluster's registration on the hub; the Git repository and chart registry entries | The hub can still reach the cluster and FAR |
| The IAM trusted profile `<cluster>-f5-cne-controller-f5-bnk` | None while it stays |
| The rendered manifests in Git | Re-creating the Application syncs them again |
| BNK's CRDs: 102 F5 and cert-manager CRDs, and the experimental Gateway API CRDs (`backendtlspolicies`, `tcproutes`, `tlsroutes`, `udproutes` in `gateway.networking.k8s.io`, and the `gateway.networking.x-k8s.io` group) | **OpenShift's ingress operator goes Degraded** ("Unmanaged Gateway API CRDs found") and the cluster reports `Upgradeable=False`: **OpenShift updates are blocked** until the experimental CRDs are gone |

To remove all of this, run `roksbnkargoctl uninstall` after the UI delete. It finds the
Application gone, finishes the post-uninstall check if a BNK namespace remains, and removes
the rest, BNK's CRDs included ([uninstall](./10-uninstall.md)).

## Re-install after a delete from the UI

Run `roksbnkargoctl install --no-sync`, then **Sync** as for a first install. The pull
secrets were in the BNK namespaces, which the delete removed, so they must be written again
before Argo CD can pull FLO.

## See also

- [The Argo CD Application](./07-the-application.md): every object, wave and hook
- [upgrade](./10a-upgrade.md): the same upgrade done by the CLI
- [uninstall](./10-uninstall.md): what the CLI removes that the UI cannot
- [Checks reference](./16-checks.md)
