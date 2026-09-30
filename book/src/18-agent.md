# Troubleshooting with an agentic CLI (agent)

roksbnkargoctl can hand a failed install to a coding-agent CLI you already use —
Claude Code, Codex, Gemini CLI or Aider — together with a written description of
how the install works, every check, and the known failures. This chapter explains
what `agent init` puts into your workspace, how `agent <cli>` starts your CLI
against it, how the troubleshooter persona works, and the safety rules the
scaffold sets. After reading it you will be able to point an agent at a failed
install and get back a diagnosis tied to the check's own evidence, without giving
it the ability to change your infrastructure.

roksbnkargoctl **embeds no language model** and sends nothing anywhere. The agent
you choose runs locally under your own account and configuration, with whatever
model and data-handling terms you have set up for it. roksbnkargoctl only writes
files into the workspace and, if you ask it to, starts that program.

## agent init

```sh
roksbnkargoctl agent init [--force] [-w <workspace>]
```

Writes the scaffold into the workspace directory
(`~/.roksbnkargoctl/<workspace>/`):

| Path | Contents |
|---|---|
| `AGENTS.md` | the knowledge base: the workspace layout, the install waves and what a stall at each means, the first move for any failure, the known failures and their causes, the uninstall sequence, and the rules for agents |
| `CLAUDE.md` | one line, `@AGENTS.md`, so Claude Code loads the same file |
| `personas/operator.md` | the operator persona: runs the lifecycle, with consent |
| `personas/troubleshooter.md` | the troubleshooter persona: diagnoses, changes nothing |
| `journal/` | an empty directory for append-only notes, one file per significant action |

Files are written with mode `0600` and directories `0700`. Existing files are kept,
so your own edits to `AGENTS.md` survive a re-run; `--force` overwrites them with
the versions built into this release. When nothing was written, `agent init` says
the workspace is already scaffolded.

`AGENTS.md` is the name most coding agents read by convention. It describes the
same wave table and known failures as the [troubleshooting guide](./17-troubleshooting.md),
written as instructions: it tells the agent to act as exactly one persona at a
time.

## agent &lt;cli&gt;

```sh
roksbnkargoctl agent                          # lists the supported CLIs and the workspace path
roksbnkargoctl agent agy                      # or: claude, codex, gemini, aider, pi, opencode
roksbnkargoctl agent claude --persona operator
roksbnkargoctl agent agy --show               # print the command instead of running it
```

`agent <cli>` scaffolds the workspace first if `AGENTS.md` is missing, then starts
the CLI **in the workspace directory**, attached to your terminal. Its first turn tells
it to read `AGENTS.md` and take a persona: `--persona troubleshooter` (the default) or
`--persona operator`. The prompt is:

```text
Read AGENTS.md, then act as the troubleshooter persona (personas/troubleshooter.md).
Start from `roksbnkargoctl status`; run `roksbnkargoctl diagnose` before drawing conclusions.
```

Each CLI takes that first turn in its own way:

| Name | Command run | How it gets the knowledge base and persona |
|---|---|---|
| `agy` | `agy -i "<prompt>"` | `-i` runs the prompt and continues the session interactively |
| `gemini` | `gemini -i "<prompt>"` | the same |
| `claude` | `claude "<prompt>"` | the prompt is the first turn; `CLAUDE.md` also includes `AGENTS.md` |
| `codex` | `codex "<prompt>"` | the prompt is the first turn; codex reads `AGENTS.md` |
| `aider` | `aider --read AGENTS.md --read personas/<persona>.md` | both files are passed as read-only context |
| `pi` | `pi` | reads `AGENTS.md` from the directory |
| `opencode` | `opencode` | reads `AGENTS.md` from the directory |

`--show` prints the exact command (`cd <workspace> && …`) instead of running it. Use it to
see what would run, or to start the CLI yourself with extra flags.

In the container image (`ghcr.io/jgruberf5/roksbnkargoctl`), no agent CLI is installed, so
`agent <cli>` refuses before it scaffolds anything and points at `agent <cli> --show`,
which works there. The printed `cd` path is the one inside the container
(`/work/.roksbnkargoctl/<workspace>`); on the host it is `./.roksbnkargoctl/<workspace>`
in the directory you mounted. `agent init` works in the image.

`agent <cli>` refuses to start when standard output is not a terminal:
`refusing to start <cli>: stdout is not a terminal`. An agent session needs one, and
capturing it (`$(roksbnkargoctl agent claude)`, or a pipe) would send whatever the model
prints somewhere it does not belong. Use `--show` in scripts.

The CLI must already be installed and on your `PATH`; otherwise the command fails with
`<name> is not installed or not on PATH`, followed by the command it would have run.
You configure, authenticate and pay for the CLI yourself; roksbnkargoctl passes it no
credentials.

Starting your chosen agent is the one place roksbnkargoctl runs another program. It
is the feature, not a substitute for doing its own work in-process.

## How the troubleshooter works

The troubleshooter persona is built around the two read-only commands in
[status and diagnose](./09-status-and-diagnose.md), which need no `kubectl` or `oc`:
everything the agent needs to read ends up as files in the workspace.

Its method, as written in `personas/troubleshooter.md`:

1. Run `roksbnkargoctl status`; note the Application's operation phase and message.
2. Run `roksbnkargoctl diagnose`; read `diagnostics/<latest>/summary.md`.
3. Find the lowest wave that is not Healthy/Succeeded. Everything above it is a
   consequence.
4. Read that wave's check log or resource conditions, and **quote the line that
   decides**.
5. If two signals disagree (a check passes that names the property it suspects is
   broken), resolve the disagreement before concluding.
6. Journal the symptom, the evidence (file and line), the cause, the fix, and what
   it did not check.

Its goals, in order: name the failing wave and check, quoting the check's own
message; tie it to a cause in `AGENTS.md` "Known failures" or show evidence for a
new one; hand the fix to the operator persona as a journal entry with the exact
command or config change.

What each persona may do:

| | Troubleshooter | Operator |
|---|---|---|
| `status`, `diagnose`, `flp status`, `registry verify`, `render` (writes only into the workspace) | yes | yes |
| read every workspace file, including `diagnostics/` | yes | yes |
| write journal entries | yes | yes |
| `init`, `install`, `uninstall`, `cos`, `registry`, `flp`, `argocd` | no | yes, with journaled consent for anything that changes infrastructure |
| edit `config.yaml` | no | yes, with your agreement, journaled |
| delete namespaces, CRDs or finalizers by hand | no | no |
| print secret values | no | no |
| `--yes` on `uninstall`, `flp down`, `argocd down` | no | only with journaled consent |

The operator persona also carries a pre-`install` checklist: `render` succeeds, the
environment variables are set, `flp status` shows the proxy up (disconnected mode),
`registry verify` reports nothing missing (mirror mode), and you have reviewed
`manifests/git/`.

A typical session:

```text
$ roksbnkargoctl agent claude -w prod
> Act as the troubleshooter. The install failed; find out why.
  (the agent runs status and diagnose, reads summary.md and the check logs,
   names the lowest failing wave, quotes the [FAIL] line, matches it to a
   known failure, and writes a journal entry with the fix)
```

## Safety rules

The rules are written into `AGENTS.md` for every agent to follow:

- **Read-only first.** `status`, `diagnose`, `flp status`, `registry verify` and the
  workspace files come before anything else. Changes happen only as the active
  persona allows.
- **No secrets.** Never print or copy secret values: the IBM Cloud API key,
  `ARGOCD_AUTH_TOKEN`, the Git token, the FAR key, the subscription JWT.
  `manifests/direct/` is already redacted and must stay so. The tool itself never
  writes secrets into the workspace in the first place, and `diagnose` never
  collects Secret values (the License's `spec.jwt` is redacted in `license.yaml`).
- **Consent before change.** `install`, `uninstall`, `flp up/down`, `argocd up/down`
  and `registry replicate` change real infrastructure. The operator's consent is
  journaled before any of them runs.
- **Install and uninstall only.** The agent never attempts a BNK upgrade through
  roksbnkargoctl and never recommends uninstall + install as one. BNK upgrades are done outside this tool: BNK supports an in-place upgrade by changing the manifest version in its custom resources, following F5's BNK documentation.
- **Report faithfully.** Quote the check's own verdict line; say what was not
  checked.

These are instructions to the agent, not technical controls. The agent runs with
your permissions and your environment: if the environment holds
`IBMCLOUD_API_KEY` and the other variables, an agent that ignored its rules could
use them. Use your CLI's own permission and approval settings to require your
confirmation before it runs any command. When you only want a diagnosis, start the
agent from a shell that holds only what `status` and `diagnose` need — the IBM
Cloud API key (to read the cluster) and `ARGOCD_AUTH_TOKEN` (to read the
Application) — and not the Git token or the mirror password.

## See also

- [Troubleshooting guide](./17-troubleshooting.md) — the same knowledge, for a human
- [status and diagnose](./09-status-and-diagnose.md)
- [Checks reference](./16-checks.md)
- [Appendix D: security model](./appendix-d-security.md)
