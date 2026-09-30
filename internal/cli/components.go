package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/agent"
)

// ---- agent -----------------------------------------------------------------------

func newAgentCmd() *cobra.Command {
	var force, show bool
	var persona string
	cmd := &cobra.Command{
		Use:   "agent [" + strings.Join(agent.Names(), "|") + "]",
		Short: "Troubleshoot the install with your own agentic CLI",
		Long: `roksbnkargoctl embeds no LLM. ` + "`agent init`" + ` scaffolds AGENTS.md (how the
install works, every check, the known failures and their causes) and personas
(operator, troubleshooter) into the workspace. ` + "`agent <cli>`" + ` starts that CLI in
the workspace, with a first turn telling it to read AGENTS.md and take the persona
(--persona, default troubleshooter); --show prints the command instead of running it.
The troubleshooter works from ` + "`roksbnkargoctl status`" + ` and ` + "`roksbnkargoctl diagnose`" + `,
which need no kubectl or oc.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, _, err := openWorkspace()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "supported: %s\nworkspace: %s\n", strings.Join(agent.Names(), ", "), ws.Dir)
				return nil
			}
			if show {
				// Print only: no scaffolding, and the directory quoted like the args.
				argv, err := agent.Argv(args[0], persona)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "cd %s && %s\n", agent.Show([]string{ws.Dir}), agent.Show(argv))
				return nil
			}
			if inContainerImage() {
				return errAgentInContainer(args[0])
			}
			if _, err := os.Stat(filepath.Join(ws.Dir, "AGENTS.md")); err != nil {
				if _, err := agent.Init(ws.Dir, false); err != nil {
					return err
				}
			}
			c, err := agent.Command(args[0], persona, ws.Dir)
			if err != nil {
				return err
			}
			out(cmd).step("%s in %s (persona: %s)", args[0], ws.Dir, persona)
			return c.Run()
		},
	}
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold AGENTS.md, personas and a journal into the workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws, _, err := openWorkspace()
			if err != nil {
				return err
			}
			written, err := agent.Init(ws.Dir, force)
			if err != nil {
				return err
			}
			sort.Strings(written)
			for _, w := range written {
				out(cmd).ok("wrote %s", w)
			}
			if len(written) == 0 {
				out(cmd).info("already scaffolded (use --force to overwrite)")
			}
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	cmd.Flags().StringVar(&persona, "persona", "troubleshooter", "persona to start as: "+strings.Join(agent.Personas, " or "))
	cmd.Flags().BoolVar(&show, "show", false, "print the command instead of running it")
	cmd.AddCommand(initCmd)
	return cmd
}
