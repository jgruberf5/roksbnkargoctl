package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the current workspace's config.yaml",
		Long: `show prints the workspace's config.yaml as it is on disk: the settings you gave
init, and the resolved section init and install filled in (cluster and VPC IDs,
endpoints, the trusted profile, the last published commit). It holds no secret
values, only the names of the environment variables that carry them.

The workspace is the current one, or the one named with -w. Its path is
printed on stderr, so stdout is the file alone:

  roksbnkargoctl show > config.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
}
