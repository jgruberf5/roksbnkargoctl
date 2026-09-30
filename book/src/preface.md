# Preface

This book shows you how to install F5 BIG-IP Next for Kubernetes (BNK) 2.4 GA on an
IBM Cloud Red Hat OpenShift (ROKS) cluster with `roksbnkargoctl`, how to take it off
again, and what to do when either goes wrong. By the end you will know exactly what the
tool changes in IBM Cloud, in your cluster, in your Git repository and in your Argo CD,
and why.

## Who this book is for

You are a platform engineer. You know Kubernetes and OpenShift, you run Argo CD, and you
have been asked to put BNK on a ROKS cluster that already exists. You do not need to know
the IBM Cloud APIs or the internals of BNK: where they matter, the book explains them.

## What roksbnkargoctl is

`roksbnkargoctl` is a single Go binary that installs and uninstalls BNK 2.4 GA on an
existing ROKS cluster **as one Argo CD Application in your existing, external Argo CD**.

> **Install and uninstall only.** roksbnkargoctl is a day-0 tool: it installs BNK and it
> removes BNK. It does not upgrade BNK. BNK supports an in-place upgrade by changing the
> manifest version in its custom resources; do that by following F5's BNK
> documentation. It is beyond the scope of this tool. (`roksbnkargoctl self update`
> updates roksbnkargoctl itself, never BNK.)

- **Argo CD applies the F5 objects.** Your Git repository holds what F5's manual install
  writes: the values files of the cert-manager and FLO Helm charts, and the handful of
  custom resources and supporting objects. Argo CD installs the two charts from the
  registry with those values, as `helm install` would, and syncs everything in ordered
  waves. The tool renders the YAML, pushes it to Git, registers the cluster and the chart
  registries with Argo CD, and creates the Application.
- **No Terraform, and no other binaries.** IBM Cloud, Kubernetes, Git and Argo CD are
  driven through their APIs from inside the one process. There is no `terraform`, `helm`,
  `kubectl`, `ibmcloud` or `git` to install, and the tool builds and runs natively on
  Linux, macOS and Windows, or from its container image with only Docker.
- **Everything runs in ROKS.** The prerequisite and lifecycle checks are a small
  container, `check`, that runs inside the cluster: as Argo CD hooks during install, and
  as Jobs that `uninstall` runs around the delete. Nothing runs on the Argo CD hub.
- **No secret goes to Git.** The FAR pull credential, the subscription JWT and the
  license proxy CA are written straight into the cluster by `install`. The renderer
  refuses to publish if a secret value appears in any Git object or values file. (The
  registry login is also given to Argo CD, so that it can pull the charts.)

It is the slim successor to [roksbnkctl](https://github.com/jgruberf5/roksbnkctl), a
heavier Terraform-based tool, for teams that standardise on Argo CD and do not want
Terraform in the install path.

## How this book is organised

| Part | Chapters | Read it when |
|---|---|---|
| I. Concepts | [What it does](./01-what-it-does.md), [How an install flows](./02-how-an-install-flows.md), [Prerequisites](./03-prerequisites.md) | Before you start: scope, architecture, what you must have in place |
| II. Getting Started | [Installing roksbnkargoctl](./04-installation.md), [Workspaces and init](./05-workspaces-and-init.md), [Quick start](./06-quick-start.md) | Your first install, end to end, in connected mode |
| III. The Install Lifecycle | [The Application](./07-the-application.md), [install](./08-install.md), [status and diagnose](./09-status-and-diagnose.md), [uninstall](./10-uninstall.md) | You want the detail behind each command |
| IV. Modes and Components | [Modes](./11-modes.md), [flp](./12-flp.md), [registry](./13-registry.md), [cos](./14-cos.md), [Test hub](./15-test-hub.md), [forge](./15a-forge.md) | Disconnected installs, private registries, the supply chain, a test Argo CD, BNK Forge registration |
| V. The check Container | [Checks reference](./16-checks.md) | A check failed and you want to know what it tested |
| VI. Troubleshooting | [Troubleshooting guide](./17-troubleshooting.md), [agent](./18-agent.md) | Something is stuck |
| Appendices | [config.yaml](./appendix-a-config.md), [commands](./appendix-b-commands.md), [sizing](./appendix-c-sizing.md), [security](./appendix-d-security.md), [design](./appendix-e-design.md) | Reference |

If you only read three chapters, read [How an install flows](./02-how-an-install-flows.md),
[Prerequisites](./03-prerequisites.md) and the [Quick start](./06-quick-start.md).

## Conventions

- Commands you type are shown in `sh` blocks. Output is shown in `text` blocks and is
  abbreviated with `…` where it is long.
- `roksbnkargoctl` prints progress to standard error with a leading `→` (a step started),
  `✓` (a step succeeded) or `⚠` (a warning). The book reproduces those markers as the tool
  prints them.
- Secrets are always shown as placeholders: `…` or `<your-token>`. Never paste a real API
  key, token or JWT into a file you intend to share.
- `<workspace>` stands for your workspace name, and `<cluster>` for your ROKS cluster's
  name. Example names such as `demo`, `bnk-demo` and `my-roks` are illustrations only.
- Configuration keys are written as YAML paths: `bnk.mode` means the `mode` key under
  `bnk:` in `config.yaml`. Each key can also be overridden for one run by an environment
  variable, `ROKSBNKARGOCTL_BNK_MODE` for this one. [Appendix A](./appendix-a-config.md)
  lists every key and its variable.
- "The hub" means the Argo CD instance you install into; "ROKS" or "the cluster" means
  the OpenShift cluster BNK is installed on. They are always different clusters.
