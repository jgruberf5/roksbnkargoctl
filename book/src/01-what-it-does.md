# What roksbnkargoctl does (and doesn't)

This chapter sets the boundaries. After reading it you will be able to say whether
`roksbnkargoctl` fits your situation, which of its two modes and two image sources you
need, and what you will still have to do yourself.

## In one sentence

`roksbnkargoctl` installs and uninstalls **F5 BIG-IP Next for Kubernetes 2.4 GA** on an
**existing** IBM Cloud ROKS cluster, as **one Argo CD Application** in an **existing,
external** Argo CD.

## Scope

It is a day-0 tool: it puts BNK on, and it takes BNK off.

| It does | Details |
|---|---|
| Install BNK 2.4 GA | The only version is `2.4.0` (`bnk.version`; any other value is rejected). The FAR tag `2.4.0` is the same artifact as `2.4.0-3.3175.0-0.0.380`. |
| Uninstall BNK | Deletes the Application; Argo CD runs the uninstall checks in ROKS, then the tool removes what it wrote outside Git. |
| Use what you already have | The ROKS cluster and the transit gateway are taken by name or ID. The Argo CD hub is yours and runs outside the cluster. |
| Prepare IBM Cloud | Creates the IAM trusted profile BNK's CNE controller uses; attaches the cluster's VPC to the transit gateway if it is not attached already. |
| Prepare the cluster | Writes the pull secret, the subscription JWT, the license proxy CA and the check namespace and RBAC directly into ROKS; registers ROKS with Argo CD. |
| Publish to Git | Renders every manifest on your workstation and commits it to one path in your repository. Argo CD's only source is that repository. |
| Check before and after | Argo CD runs the `check` container as hooks in ROKS: before anything of BNK is applied, while it comes up, after it is healthy, and around uninstall. |

### Two modes

`bnk.mode` says how BNK is licensed.

| Mode | Licensing | Cluster egress |
|---|---|---|
| `connected` (default) | BNK licenses directly with F5 | Every node reaches F5 licensing (`product.apis.f5.com`, `product-s.apis.f5.com`) on 443 |
| `disconnected` | Through an F5 License Proxy (FLP) on a VSI attached to the transit gateway | Every node reaches the FLP on 8443; no F5 licensing egress |

`roksbnkargoctl flp up` builds the license proxy for you, or you point `flp.external` at
one you already run. See [Connected and disconnected modes](./11-modes.md).

### Two image sources

`registry.source` says where ROKS pulls BNK's images and charts from.

| Source | Pulls from | Pull secret in ROKS |
|---|---|---|
| `far` (default) | F5 Artifact Registry (`registry.far_host`, default `repo.f5.com`) | `far-secret` |
| `mirror` | Your private registry (Harbor, Artifactory, a plain registry, ICR), filled by `roksbnkargoctl registry replicate` | `mirror-secret` |

A disconnected install is usually paired with `mirror`, so that the cluster needs no
internet egress at all. See [Mirroring into a private registry](./13-registry.md).

### Size

IBM ROKS runs BNK at `deploymentSize: Tiny` only. Every larger size requests hugepages,
and ROKS workers have none. The size is therefore fixed in the renderer, `config.yaml` has
no key for it, and the post-install check fails any `CNEInstance` that is not `Tiny`. You
do choose the number of TMM replicas (`bnk.tmm_replicas`, default `3`). See
[Appendix C](./appendix-c-sizing.md).

## What it deliberately does not do

| Not done | Why |
|---|---|
| Create or delete a ROKS cluster | The cluster already exists. `install` never creates it and `uninstall` never deletes it. |
| Create or delete a transit gateway | Same. `install` only adds a VPC connection when the cluster's VPC is not attached, and `uninstall --detach-tgw` removes only a connection that `install` itself created. |
| Install or run Argo CD for you in production | Argo CD is yours. (`roksbnkargoctl argocd up` builds a **test** hub for trying the tool; see [chapter 15](./15-test-hub.md).) |
| Upgrade BNK | roksbnkargoctl does **install and uninstall only**. BNK itself supports an in-place upgrade by changing the manifest version in its custom resources; follow F5's BNK documentation for that. It is beyond the scope of this tool. (`roksbnkargoctl self update` updates the tool, not BNK.) |
| Configure gateways or traffic | No `Infra`, `GatewaySettings` or `Gateway` objects and no traffic tests. The install ends when BNK is licensed and its `CNEInstance` is available. |
| Install BNK 2.3 | Only 2.4 GA. 2.4 replaces the 2.3 object model, and the tool is built for 2.4 alone. |
| Store secrets | No secret value is written to Git or to the workspace. The IBM API key, Argo CD token, Git token and mirror password are read from environment variables each time. |

## How it differs from roksbnkctl

[roksbnkctl](https://github.com/jgruberf5/roksbnkctl) is the predecessor: a broader tool
built on Terraform. `roksbnkargoctl` exists because a customer rejected Terraform as too
heavy and standardises on Argo CD.

| | roksbnkctl | roksbnkargoctl |
|---|---|---|
| Install mechanism | Terraform | Plain YAML in your Git repo, synced by your Argo CD |
| Who applies the F5 objects | The tool | Argo CD |
| External binaries | Terraform and friends | None: IBM Cloud, Kubernetes, Git and Argo CD APIs are called in-process |
| Scope | Wider lifecycle, including cluster creation and later phases | Day-0 install and uninstall of BNK 2.4 GA only |

The checks are the other visible difference. In `roksbnkargoctl` the prerequisite and
lifecycle checks are not something the tool runs from your workstation: they are the
`check` container, deployed by the same YAML as BNK and run by Argo CD in ROKS, as hook
Jobs, a per-node probe DaemonSet and one Deployment. What they verify is therefore what
the cluster itself can see, from every node.

The two tools agree on names where it matters. The trusted profile, the SCC binding for
FLO, the issuer chain and the `CNEInstance` name match what roksbnkctl creates, so a
cluster moved between the tools keeps one set of objects.

## See also

- [How an install flows](./02-how-an-install-flows.md)
- [Prerequisites](./03-prerequisites.md)
- [Appendix E: Design decisions and live findings](./appendix-e-design.md)
