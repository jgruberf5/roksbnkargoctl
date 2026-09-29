# Book style guide (for writers; not published)

- Audience: an F5/IBM customer platform engineer who knows Kubernetes and Argo CD, not
  necessarily IBM Cloud APIs or BNK internals. Write for them, in second person.
- Accuracy over polish. Every command, flag, config key, default, file path, wave number,
  object name and error message must come from the SOURCE in /mnt/d/project/roksbnkargoctl
  (branch fix/issues-2-and-5 is checked out there and is the current code). Read the code;
  run `go run ./cmd/roksbnkargoctl <cmd> --help` there for exact help text. Never invent a
  flag or key. If unsure, leave it out.
- The contract is docs/DESIGN.md; the agent guide is internal/agent/files/AGENTS.md.
- Verified live facts you may state (bnkargo, OpenShift 4.21.31, Argo CD 3.5.1): connected,
  disconnected (FLP) and mirror (artifactory, 93 artifacts) installs and uninstalls; 3 TMM
  replicas on VPC block storage with no PVC on TMM; install ~8-10 minutes of sync.
- No mention of AI, Claude, LLMs, or who wrote the code, EXCEPT chapter 18 (agent), which
  describes the agentic-CLI feature factually. No emojis.
- Show secrets only as placeholders (…). Never paste a real key, token, JWT, IP of a real
  customer host. The test hub/cluster names may appear only as generic examples.
- Use mdBook markdown: headings, tables, fenced code blocks (```sh / ```yaml / ```text).
  Diagrams as ASCII art in ```text blocks (no mermaid). Cross-link chapters with relative
  links like [install](./08-install.md).
- Each chapter: open with one paragraph saying what the reader will be able to do; end with
  "See also" links where useful. Be concise; tables beat prose for reference material.
