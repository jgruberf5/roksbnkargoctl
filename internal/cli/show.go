package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
)

func newShowCmd() *cobra.Command {
	var effective bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the current workspace's config.yaml",
		Long: `show prints the workspace's config.yaml as it is on disk: the settings you gave
init, and the resolved section init and install filled in (cluster and VPC IDs,
endpoints, the trusted profile, the last published commit). It holds no secret
values, only the names of the environment variables that carry them.

The workspace is the current one, or the one named with -w. Its path is
printed on stderr, so stdout is the file alone:

  roksbnkargoctl show > config.yaml

--effective prints what a command would use instead: config.yaml with the
ROKSBNKARGOCTL_* environment overrides applied and the defaults filled in. The
overrides in force are listed on stderr. Overrides are never saved, so this
output is not config.yaml.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if effective {
				return showEffective(cmd)
			}
			ws, _, err := openWorkspace()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(ws.ConfigPath())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "# workspace %s: %s\n", ws.Name, ws.ConfigPath())
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().BoolVar(&effective, "effective", false, "print the settings in effect: config.yaml plus environment overrides and defaults")
	return cmd
}

// showEffective prints the effective config: the one every other command
// builds from config.yaml, the ROKSBNKARGOCTL_* variables and the defaults.
func showEffective(cmd *cobra.Command) error {
	s, err := newSession(cmd)
	if err != nil {
		return err
	}
	b, err := config.Marshal(s.cfg)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	fmt.Fprintf(stderr, "# workspace %s: %s, with overrides and defaults applied (not the file)\n", s.ws.Name, s.ws.ConfigPath())
	for _, o := range s.overrides {
		fmt.Fprintf(stderr, "# %s set by %s\n", o.Key.Path, o.Source)
	}
	_, err = cmd.OutOrStdout().Write(b)
	return err
}
