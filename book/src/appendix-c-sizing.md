# C. ROKS sizing: Tiny, TMM replicas and storage

This appendix explains the one BNK size IBM ROKS can run, how roksbnkargoctl makes
sure nothing else is deployed, how many TMM replicas to ask for and what that
means for your workers, and which persistent volumes BNK actually creates. After
reading it you will be able to size a cluster for an install and know which
storage questions you do not need to worry about.

## Only `deploymentSize: Tiny` runs on ROKS

F5's `CNEInstance` CRD offers five values for `spec.deploymentSize`:

```text
Tiny   Small   Medium   Large   Max
```

Every size above `Tiny` requests **hugepages**. IBM ROKS workers report
`hugepages-2Mi: 0` (verified on the test cluster, OpenShift 4.21.31), and ROKS has
**no Machine Config Operator**, which is how OpenShift would normally allocate
hugepages on a node. There is no supported way to give a ROKS worker hugepages, so
a larger size leaves TMM `Pending` with `Insufficient hugepages-2Mi`
(roksbnkctl issue #203, where a hugepages setting turned out to be a no-op on ROKS).

`Tiny` is therefore not a default — it is the only value that works.

## How the tool enforces it

| Layer | What it does |
|---|---|
| Renderer | `deploymentSize: Tiny` is a literal in the CNEInstance the tool renders, pinned by a render test |
| `config.yaml` | there is no size key. Parsing is strict, so a `bnk.deployment_size` (or any other spelling) is rejected as an unknown key rather than silently ignored |
| `check post-install` | the `deployment-size` finding fails any CNEInstance that is not `Tiny`, naming the hugepage reason and telling you to restore `deploymentSize: Tiny` in Git |

The post-install check exists for the one path left open: someone edits the
CNEInstance in the Git repository. Argo CD would sync that edit without complaint;
the check turns it into a failed sync with an explanation. The rest of the rendered
CNEInstance also carries `demoMode: {enabled: false}`, as roksbnkctl's verified 2.4
install does.

## TMM replicas

| Setting | Value |
|---|---|
| Config key | `bnk.tmm_replicas` |
| Default | `3` |
| Minimum | `1` (validation) |
| Rendered as | `CNEInstance.spec.tmmReplicas` |
| Checked by | `check post-install`: exactly that many TMM pods (`app=f5-tmm`) Ready, and more than one spread over at least two zones |

The rendered CNEInstance places TMM with F5's reference data-plane placement:

```yaml
placement:
  dataPlane:
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          - labelSelector: {matchLabels: {app: f5-tmm}}
            topologyKey: kubernetes.io/hostname        # at most one TMM per node
    topologySpreadConstraints:
      - labelSelector: {matchLabels: {app: f5-tmm}}
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone       # even across zones
        whenUnsatisfiable: DoNotSchedule
```

So, for `N` TMM replicas you need at least `N` schedulable workers, spread so the
zones can stay within one replica of each other. `check pre-install` independently
requires at least 3 schedulable Ready workers spanning 3 zones. The default of 3
replicas puts one TMM in each zone of a 3-zone cluster.

### Storage: any StorageClass works for TMM

At `Tiny`, **TMM mounts no PersistentVolumeClaim**. Several TMM replicas therefore
need **no** ReadWriteMany storage, and IBM VPC block storage (ReadWriteOnce) is
fine:

- Verified with this tool: 3 TMM replicas Running on VPC block storage, one per
  node and zone, with no PVC on TMM.
- Verified by roksbnkctl on BNK 2.4 (issue #197): 3 replicas on 6-node clusters
  and 9 replicas on a 9-node cluster, one TMM per node, three per zone, on block
  storage.

The shared ReadWriteOnce `tmm-pvc` that issue #197 was originally filed about only
appears at sizes above `Tiny`, which ROKS cannot run. An earlier version of this
tool failed `tmm_replicas > 1` on block storage on the strength of that issue's
title; the gate was removed once the issue's resolution showed it would have
refused F5's reference 3-replica install on a stock cluster for no reason. Today
`check pre-install` only prints an `[INFO] tmm-storage` note. If a future BNK
release or IBM worker type makes larger sizes runnable, re-test multi-replica
scheduling rather than assuming this still holds.

## What BNK does claim

BNK's other components do create volumes, on the cluster's **default**
StorageClass unless you set `bnk.storage_class` (rendered as the CNEInstance's
`storageClassName`). On a 2.4 install at `Tiny`, the PVCs in `f5-bnk` are:

| PVC | Component |
|---|---|
| `data-f5-dssm-db-0`, `data-f5-dssm-db-1`, `data-f5-dssm-db-2` | DSSM database |
| `cluster-wide-controller` | the controller |
| `downloader-storage` | the downloader |

This is why `check pre-install` fails when the cluster has no default
StorageClass, and fails when `bnk.storage_class` names one that does not exist.
The PVC list comes from roksbnkctl's live 2.4 clusters (issue #197); use
`oc get pvc -n f5-bnk` to confirm it on yours.

## Reference worker profiles

roksbnkctl, the Terraform-based predecessor of this tool, built and verified four
reference shapes on BNK 2.4 — every one at `deploymentSize: Tiny`. The cluster-size
names are F5's; what changes between them is the worker flavour and the TMM
replica count, never the BNK size.

| | Baseline | Small cluster | Medium cluster | Large cluster |
|---|---|---|---|---|
| Workers | 3 (1 per zone) | 6 (2 per zone) | 6 (2 per zone) | 9 (3 per zone) |
| Worker flavour | `bx2.16x64` | `bx2.8x32` | `cx2.16x32` | `cx2.48x96` |
| `deploymentSize` | `Tiny` | `Tiny` | `Tiny` | `Tiny` |
| `tmm_replicas` | 1 | 3 | 3 | 9 |

In each, every component reported `Available`, the License was `Active`, and every
TMM pod was Running — on the Large cluster, nine TMM pods on nine distinct nodes,
three per zone. The Baseline shape is the one F5's BNK 2.4 IBM install guide
describes; the other three follow F5's sizing-guide cluster shapes.

roksbnkargoctl does not create or size clusters: it installs onto the cluster you
have. To reproduce one of these shapes, build the workers with that flavour and
count, and set `bnk.tmm_replicas` to match (this tool's default is 3, not the
Baseline's 1).

## See also

- [Checks reference: pre-install](./16-checks.md#pre-install) and
  [post-install](./16-checks.md#post-install)
- [Appendix A: config.yaml reference](./appendix-a-config.md)
- [Appendix E: design decisions and live findings](./appendix-e-design.md)
