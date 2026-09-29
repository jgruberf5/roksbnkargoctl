# Working agreements for this repository

These are carried over from roksbnkctl, where each was learned the hard way.

These apply to every agent session in this repo, not just the one that wrote them —
Claude Code, Codex, Cursor, Aider, Copilot, or a human following the same rules.

`AGENTS.md` is the canonical file, because it is the name the widest set of coding
agents read. `CLAUDE.md` beside it is a one-line `@AGENTS.md` include, which is how
Claude Code pulls the same content in. **Edit this file, never that one** — two
copies of a working agreement is how the agreements start disagreeing.

That mirrors what `roksbnkargoctl agent init` scaffolds into a workspace
(`internal/agent/files/`), so the repo follows the convention the tool itself ships.

## Every issue gets labelled when it is opened

Never leave an issue unlabelled — not one you file, and not one you find already
open. Apply, at minimum:

- **a kind**: `bug`, `enhancement`, `documentation`, `testing`, `tech-debt`,
  `security`, `question`
- **a severity** for anything that is a defect: `severity:high`,
  `severity:medium`, `severity:low`
- **`regression`** whenever the behaviour previously worked. Say which release
  broke it in the body; `git log -S` finds it.

Check `gh label list` before inventing a label. At the start of a session, run
`gh issue list --state open --json number,labels` and label anything bare.

## Every PR gets a complete review, posted as a comment on the PR

The review is a **comment on the pull request**, not a summary in chat. Chat is
lost when the session ends; the PR is the record, and the next person to touch
this code reads it there.

Review your own PRs to the same standard as anyone else's. In practice self-review
has caught roughly one real defect per PR in this repo, including regressions
introduced by the PR under review.

A review must:

1. **State what was found, or that nothing was.** "No defects found" is a valid
   review when it lists what was actually checked.
2. **Fix everything it finds**, in that PR, then re-review. Repeat until a cycle
   finds nothing. Do not open a follow-up issue for something the PR itself caused.
3. **Verify claims rather than assert them.** If the PR says a value is validated
   downstream, open that file and quote it. If it says a round-trip is lossless,
   run it.
4. **Record mutation results.** For each guard the PR adds, break the code it
   guards and confirm the test fails. Every mutation must COMPILE — a build
   failure proves nothing. A mutation that SURVIVES means the test is wrong, not
   that the check is redundant.
5. **Say what it did not cover.** An untested path named is worth more than a
   clean-looking review that quietly skipped it.

### Check it, do not remember it

`./scripts/pr-review-audit.sh` lists open PRs with no review comment and branches
with no PR.

The rule above was written in a session that then broke it **three times**, every
time the same way: push the work, report progress, never open the PR. Writing the
rule did not help. Writing the script did not help either, because a check that is
not run is not a control.

So it runs automatically:

```
git config core.hooksPath .githooks
```

`.githooks/pre-push` refuses a second push of a branch that still has no PR, and
warns when an open PR has no `## Review` comment. First pushes are allowed — a
branch cannot have a PR before it exists — and `--no-verify` bypasses it for
genuine work in progress.

**Announcing an intention is not doing it.** "I'll open the PR next" and "I'll run
the live test" both read as progress and are not. Do the thing in the same turn,
or say plainly that it has not been done yet.

### Do not let the blast radius grow

When a review touches a guard from an earlier cycle, change the **premise** that
turned out to be wrong, not the assertion. If a guard has to be relaxed to pass,
that is a finding about the change, not about the guard. Prefer making the value
a parameter so the original assertion survives verbatim.

## Before reporting a finding, check it is real

Three false positives were shipped-and-caught in a single session here: a residue
check that matched object names and reported 103 Calico objects as F5 residue; a
verification that dropped trailing text because `HTMLParser` buffers without
`close()`; and one that fused adjacent table cells with `''.join()` so `Title` +
`Version` read as a single token.

A new checker's first failures are as likely to be the checker's fault as the
code's. The tell is a second signal disagreeing — `mdbook` not warning about a
file your linter flags, a test passing that names the property you think is
broken. Chase the disagreement before filing.

## Tests must exercise behaviour

A test bound to a helper survives that helper being bypassed. Assert on the path
the product actually takes:

- a source-scanning guard must fail against the bug it names
- a test that cannot fail is worse than no test — prove it fails before trusting it
- when a fix depends on a property (an empty override being a no-op, say), pin
  that property too

## Commits and PR bodies

Explain **why**, and what was wrong with the previous reasoning. Prefer measured
numbers to adjectives: "FLO restores it in 375–815ms against a 3s tick" says what
"the sweep is too slow" cannot.

Do not add `Claude-Session:` trailers. No AI references in the docs except the
`agent` feature.

## Never `git add -A`

Other agent sessions work in this same checkout. `git add -A` and `git add .`
sweep up their in-progress untracked files, and the first sign is someone else's
document failing your spellcheck. It happened here:
a PRD another session was drafting landed in an unrelated commit and failed CI with
18 unknown words (roksbnkctl, 2026).

Stage by path, or `git add -u` for tracked files only. Before committing, read
`git status --porcelain` and confirm every path is yours; before pushing, check
`git diff --name-only origin/main...HEAD` for the same reason.

## Stacked PRs

`gh pr merge --delete-branch` **closes** any PR based on that branch instead of
retargeting it, and a closed PR with a deleted base cannot be reopened. Merge
stacked work by merging every branch into one integration branch and opening a
single PR to `main`; leave the individual PRs open as the review record and close
them referencing the integration PR once it lands.

## Scope guard for this repository

roksbnkargoctl exists because a customer rejected Terraform as too heavy. Keep it slim:

- **No Terraform, and no shell-outs** to `helm`, `kubectl`, `oc`, `ibmcloud`, `git`, `curl`
  or `openssl`. IBM Cloud, Kubernetes, Git, Helm templating and OCI pulls are in-process Go.
  It must build and run natively on Windows.
- **Nothing runs on the Argo CD hub.** Every container the tool deploys runs in the ROKS
  cluster. The hub only reconciles.
- **No secret values in Git.** The FAR key, subscription JWT and FLP CA go straight into ROKS.
- **BNK 2.4 GA only.** Do not add version switches for other lines.
- **The `check` image stays minimal**: one static binary on `scratch`. A new dependency in
  `cmd/check` needs a reason in the PR body.

`docs/DESIGN.md` is the contract. Change it in the same PR as the behaviour it describes.
