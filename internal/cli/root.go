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
	flagWorkspace   string
	flagNoWorkspace bool
	flagYes         bool
	flagVerbose     bool
)

// Execute runs the CLI.
func Execute() int {
	ctx, stop := signalContext()
	defer stop()
	// Best-effort: remove a <self>.old left by an earlier Windows self update
	// (see installByMoveAside). A no-op elsewhere and when there is none.
	sweepStaleBinary()
	root := newRoot()
	if err := executeRoot(ctx, root); err != nil {
		fmt.Fprintln(os.Stderr, "roksbnkargoctl:", err)
		var ee exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		return 1
	}
	return 0
}

// signalContext is cancelled by Ctrl-C or SIGTERM, and is what prompts wait on
// (promptCtx): catching SIGINT turns off Go's own exit on Ctrl-C.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	promptCtx = ctx
	return ctx, stop
}

// executeRoot runs the command tree; a prompt interrupted by Ctrl-C or the end
// of input ends it with errInterrupted (exit 130).
func executeRoot(ctx context.Context, root *cobra.Command) (err error) {
	defer recoverInterrupted(&err)
	return root.ExecuteContext(ctx)
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
agent (troubleshooting with an agentic CLI), forge (register the cluster with
BNK Forge).

The binary itself: self install (copy it onto PATH), self update (from the
GitHub releases, for the same BNK version).`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&flagWorkspace, "workspace", "w", "", "workspace name (default: the current workspace)")
	root.PersistentFlags().BoolVar(&flagNoWorkspace, "no-workspace", false, "run without a workspace, ignoring ROKSBNKARGOCTL_WORKSPACE and the current workspace: settings come from flags, ROKSBNKARGOCTL_* variables and defaults (commands that need a workspace refuse)")
	root.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "do not ask for confirmation")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")
	root.AddCommand(
		newInitCmd(), newRenderCmd(), newExportCmd(), newInstallCmd(), newUninstallCmd(), newStatusCmd(), newDiagnoseCmd(),
		newCOSCmd(), newRegistryCmd(), newFLPCmd(), newArgoCDCmd(), newAgentCmd(), newForgeCmd(),
		newShowCmd(), newWorkspacesCmd(), newVersionCmd(), newSelfCmd(),
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

// errNoWorkspace is returned when nothing selects a workspace.
var errNoWorkspace = errors.New("no workspace selected: pass -w <name> or run `roksbnkargoctl init -w <name>`")

// errWorkspaceRefused is returned to a command that needs a workspace when
// --no-workspace was given.
var errWorkspaceRefused = errors.New("this command needs a workspace: drop --no-workspace and pass -w <name>")

func workspaceName() (string, error) {
	if flagNoWorkspace {
		if flagWorkspace != "" {
			return "", errors.New("--workspace and --no-workspace cannot be used together")
		}
		return "", errWorkspaceRefused
	}
	if flagWorkspace != "" {
		return flagWorkspace, nil
	}
	if v := os.Getenv("ROKSBNKARGOCTL_WORKSPACE"); v != "" {
		return v, nil
	}
	if c := currentWorkspace(); c != "" {
		return c, nil
	}
	return "", errNoWorkspace
}

// selectWorkspace returns the selected workspace. With required false, no
// selection (or --no-workspace) is a nil workspace rather than an error; a
// selected workspace that does not exist is still an error, since silently
// running without the workspace the operator named would be worse.
func selectWorkspace(required bool) (*config.Workspace, error) {
	name, err := workspaceName()
	if err != nil {
		if !required && (errors.Is(err, errNoWorkspace) || errors.Is(err, errWorkspaceRefused)) {
			return nil, nil
		}
		return nil, err
	}
	return config.Open(name)
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
	line := strings.ToLower(strings.TrimSpace(readLine()))
	return line == "y" || line == "yes"
}
