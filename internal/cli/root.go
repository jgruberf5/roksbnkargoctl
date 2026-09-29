// Package cli is the roksbnkargoctl command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

// Build metadata, stamped with -ldflags.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var (
	flagWorkspace string
	flagYes       bool
	flagVerbose   bool
)

// Execute runs the CLI.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "roksbnkargoctl:", err)
		var ee exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		return 1
	}
	return 0
}

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "roksbnkargoctl",
		Short: "Install F5 BIG-IP Next for Kubernetes 2.4 on IBM Cloud ROKS with Argo CD",
		Long: `roksbnkargoctl installs and uninstalls F5 BIG-IP Next for Kubernetes (BNK) 2.4 GA
on an existing IBM Cloud ROKS cluster, as one Argo CD Application in an existing
Argo CD. No Terraform: IBM Cloud, Kubernetes, Git and Argo CD are driven through
their APIs.

  roksbnkargoctl init        gather the details (interview or --config-file)
  roksbnkargoctl render      write every manifest into the workspace
  roksbnkargoctl install     publish to Git, create the Application, sync it
  roksbnkargoctl status      Application, check and BNK state
  roksbnkargoctl uninstall   delete the Application; uninstall checks run in ROKS

Optional components: cos (FAR key + JWT), registry (private mirror),
flp (license proxy VSI for disconnected mode), argocd (a test Argo CD hub),
agent (troubleshooting with an agentic CLI).`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&flagWorkspace, "workspace", "w", "", "workspace name (default: the current workspace)")
	root.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "do not ask for confirmation")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")
	root.AddCommand(
		newInitCmd(), newRenderCmd(), newInstallCmd(), newUninstallCmd(), newStatusCmd(), newDiagnoseCmd(),
		newCOSCmd(), newRegistryCmd(), newFLPCmd(), newArgoCDCmd(), newAgentCmd(),
		newWorkspacesCmd(), newVersionCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and the BNK release this binary installs",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "roksbnkargoctl %s for BNK %s (commit %s, built %s)\n", Version, config.BNKVersion, Commit, BuildDate)
		},
	}
}

// ---- workspace selection -----------------------------------------------------

func currentPointer() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "current"), nil
}

func currentWorkspace() string {
	p, err := currentPointer()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func setCurrent(name string) error {
	p, err := currentPointer()
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(p, []byte(name+"\n"), 0o600)
}

func workspaceName() (string, error) {
	if flagWorkspace != "" {
		return flagWorkspace, nil
	}
	if v := os.Getenv("ROKSBNKARGOCTL_WORKSPACE"); v != "" {
		return v, nil
	}
	if c := currentWorkspace(); c != "" {
		return c, nil
	}
	return "", errors.New("no workspace selected: pass -w <name> or run `roksbnkargoctl init -w <name>`")
}

func openWorkspace() (*config.Workspace, *config.Config, error) {
	name, err := workspaceName()
	if err != nil {
		return nil, nil, err
	}
	ws, err := config.Open(name)
	if err != nil {
		return nil, nil, err
	}
	c, err := ws.Load()
	if err != nil {
		return nil, nil, err
	}
	return ws, c, nil
}

func newWorkspacesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "workspaces", Aliases: []string{"ws"}, Short: "List and select workspaces"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List workspaces",
		RunE: func(cmd *cobra.Command, _ []string) error {
			names, err := config.List()
			if err != nil {
				return err
			}
			cur := currentWorkspace()
			for _, n := range names {
				mark := " "
				if n == cur {
					mark = "*"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", mark, n)
			}
			return nil
		},
	}, &cobra.Command{
		Use: "use <name>", Short: "Make a workspace current", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := config.Open(args[0])
			if err != nil {
				return err
			}
			if !ws.Exists() {
				return fmt.Errorf("workspace %q does not exist", args[0])
			}
			return setCurrent(args[0])
		},
	})
	return cmd
}

// ---- output helpers ------------------------------------------------------------

type printer struct{ w io.Writer }

func out(cmd *cobra.Command) printer { return printer{cmd.ErrOrStderr()} }

func (p printer) step(f string, a ...any) { fmt.Fprintf(p.w, "→ "+f+"\n", a...) }
func (p printer) ok(f string, a ...any)   { fmt.Fprintf(p.w, "✓ "+f+"\n", a...) }
func (p printer) warn(f string, a ...any) { fmt.Fprintf(p.w, "⚠ "+f+"\n", a...) }
func (p printer) info(f string, a ...any) { fmt.Fprintf(p.w, "  "+f+"\n", a...) }

func confirm(cmd *cobra.Command, prompt string) bool {
	if flagYes {
		return true
	}
	if !isTTY() {
		return false
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", prompt)
	line, _ := stdin().ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
