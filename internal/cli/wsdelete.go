package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// wsRecord is something a workspace records that deleting it would lose.
type wsRecord struct {
	what   string // what is recorded
	remove string // the command that removes it
	alt    string // when that command cannot run, what to do instead
}

// wsBlockers are the records that stop `workspaces delete`: each is a cloud
// resource the workspace holds the only local record of. warnings are recorded
// but do not block (a normal uninstall leaves the transit gateway connection
// attached, so blocking on it would block every uninstalled workspace).
func wsBlockers(ws *config.Workspace) (blockers, warnings []wsRecord, err error) {
	c, err := ws.LoadFile()
	if err != nil {
		return nil, nil, err
	}
	name := ws.Name
	if r := c.Resolved; r != nil {
		if r.TrustedProfileID != "" {
			blockers = append(blockers, wsRecord{
				"an install: trusted profile " + r.TrustedProfileID + " still exists (BNK is installed, or was uninstalled with --keep-trusted-profile)",
				"roksbnkargoctl uninstall -w " + name,
				"uninstall needs the cluster and Argo CD. If the cluster is gone, delete the profile in IBM Cloud " +
					"(Manage > Access (IAM) > Trusted profiles, or `ibmcloud iam trusted-profile-delete " + r.TrustedProfileID + "`), " +
					"then delete the workspace with --force; if you kept the profile on purpose, --force leaves it in IBM Cloud"})
		}
		if r.TGWConnectionCreatedID != "" {
			warnings = append(warnings, wsRecord{
				"transit gateway connection " + r.TGWConnectionCreatedID + ", which install created to attach the cluster VPC (uninstall leaves it attached)",
				"roksbnkargoctl uninstall -w " + name + " --detach-tgw", ""})
		}
	}
	if _, err := os.Stat(ws.FLPOutputsPath()); err == nil {
		blockers = append(blockers, wsRecord{
			"an F5 License Proxy (flp-outputs.json, with the CA key BNK trusts)",
			"roksbnkargoctl flp down -w " + name, ""})
	}
	if _, err := os.Stat(ws.HubOutputsPath()); err == nil {
		blockers = append(blockers, wsRecord{
			"a test Argo CD hub (argocd-hub.json)",
			"roksbnkargoctl argocd down -w " + name,
			"take it down last: uninstall needs it if it is this install's Argo CD"})
	}
	return blockers, warnings, nil
}

func newWorkspacesDeleteCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a workspace's directory (refuses while it records cloud resources)",
		Long: `delete removes ~/.roksbnkargoctl/<name>: its config.yaml, rendered manifests and
diagnostics. It changes nothing in IBM Cloud, the cluster, Argo CD or Git.

The workspace holds the only local record of some cloud resources, so delete
refuses while it records:

  an install         (its trusted profile)   roksbnkargoctl uninstall -w <name>
  an F5 License Proxy (with its CA key)      roksbnkargoctl flp down -w <name>
  a test Argo CD hub                         roksbnkargoctl argocd down -w <name>

A transit gateway connection install created is reported but does not block:
uninstall leaves it attached by default (--detach-tgw removes it).

--force deletes anyway and lists what is left without a record. If <name> is
the current workspace, the current workspace is unset.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspacesDelete(cmd, args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "delete even while the workspace records cloud resources (they are then left without a local record)")
	return cmd
}

func runWorkspacesDelete(cmd *cobra.Command, name string, force bool) error {
	p := out(cmd)
	ws, err := config.Open(name)
	if err != nil {
		return err
	}
	if !ws.Exists() {
		return fmt.Errorf("workspace %q does not exist (%s has no config.yaml)", name, ws.Dir)
	}
	blockers, warnings, err := wsBlockers(ws)
	if err != nil {
		if !force {
			return fmt.Errorf("reading workspace %q: %w (pass --force to delete it anyway)", name, err)
		}
		p.warn("reading workspace %q: %v; deleting it anyway (--force)", name, err)
	}
	for _, w := range warnings {
		p.warn("%s: remove it with `%s` first if you no longer need it", w.what, w.remove)
	}
	if len(blockers) > 0 {
		if !force {
			var b strings.Builder
			fmt.Fprintf(&b, "workspace %q still records cloud resources; remove them first, in this order:", name)
			for i, x := range blockers {
				fmt.Fprintf(&b, "\n  %d. %s\n     %s", i+1, x.what, x.remove)
				if x.alt != "" {
					fmt.Fprintf(&b, "\n     (%s)", x.alt)
				}
			}
			b.WriteString("\n(or pass --force to delete the workspace and leave them without a local record)")
			return errors.New(b.String())
		}
		for _, x := range blockers {
			p.warn("left without a local record (--force): %s", x.what)
		}
	}
	if !confirm(cmd, fmt.Sprintf("Delete workspace %q (%s)?", name, ws.Dir)) {
		return errors.New("not confirmed (pass --yes)")
	}
	if err := os.RemoveAll(ws.Dir); err != nil {
		return err
	}
	if currentWorkspace() == name {
		if ptr, err := currentPointer(); err == nil {
			if err := os.Remove(ptr); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("workspace deleted, but unsetting the current workspace: %w", err)
			}
		}
		p.info("it was the current workspace; select another with `roksbnkargoctl workspaces use <name>`")
	}
	if os.Getenv("ROKSBNKARGOCTL_WORKSPACE") == name {
		p.warn("ROKSBNKARGOCTL_WORKSPACE still names %q: unset it, or later commands fail with \"has no config.yaml\"", name)
	}
	p.ok("workspace %q deleted", name)
	return nil
}
