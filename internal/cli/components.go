package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/agent"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// ---- cos ------------------------------------------------------------------------

func newCOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cos",
		Short: "Publish and discover the FAR auth tarball and subscription JWT in COS",
		Long: `The FAR auth tarball (from MyF5) and the subscription JWT are the two supply-chain
files every install needs. Keeping them in IBM Cloud Object Storage lets every
workspace (and every operator) find them by name instead of passing files around.

  roksbnkargoctl cos list                       what the configured bucket holds
  roksbnkargoctl cos publish --far-auth f5-far-auth-key.tgz --jwt subscription.jwt
  roksbnkargoctl cos verify                     the FAR key logs in, the JWT parses`,
	}
	var farFile, jwtFile string
	var createBucket bool
	publish := &cobra.Command{
		Use:   "publish",
		Short: "Upload the FAR auth tarball and/or JWT to the configured bucket",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if farFile == "" && jwtFile == "" {
				return errors.New("nothing to publish: pass --far-auth and/or --jwt")
			}
			ctx := cmd.Context()
			cc, err := s.COS(ctx)
			if err != nil {
				return err
			}
			if createBucket {
				if created, err := cc.EnsureBucket(ctx, s.cfg.COS.Bucket); err != nil {
					return err
				} else if created {
					s.p.ok("created bucket %s", s.cfg.COS.Bucket)
				}
			}
			if farFile != "" {
				b, err := os.ReadFile(farFile)
				if err != nil {
					return err
				}
				if _, err := far.ServiceAccountFromTarball(b); err != nil {
					return fmt.Errorf("%s is not a FAR auth tarball: %w", farFile, err)
				}
				if err := cc.PutObject(ctx, s.cfg.COS.Bucket, s.cfg.COS.FARAuthObject, b); err != nil {
					return err
				}
				s.p.ok("published %s → %s/%s", farFile, s.cfg.COS.Bucket, s.cfg.COS.FARAuthObject)
			}
			if jwtFile != "" {
				b, err := os.ReadFile(jwtFile)
				if err != nil {
					return err
				}
				if strings.Count(strings.TrimSpace(string(b)), ".") != 2 {
					return fmt.Errorf("%s is not a JWT", jwtFile)
				}
				if err := cc.PutObject(ctx, s.cfg.COS.Bucket, s.cfg.COS.JWTObject, b); err != nil {
					return err
				}
				s.p.ok("published %s → %s/%s", jwtFile, s.cfg.COS.Bucket, s.cfg.COS.JWTObject)
			}
			return nil
		},
	}
	publish.Flags().StringVar(&farFile, "far-auth", "", "FAR auth tarball (.tgz from MyF5)")
	publish.Flags().StringVar(&jwtFile, "jwt", "", "subscription JWT file")
	publish.Flags().BoolVar(&createBucket, "create-bucket", false, "create the bucket if it does not exist")

	list := &cobra.Command{
		Use:   "list",
		Short: "List the objects in the configured bucket",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			cc, err := s.COS(cmd.Context())
			if err != nil {
				return err
			}
			objs, err := cc.ListObjects(cmd.Context(), s.cfg.COS.Bucket, "")
			if err != nil {
				return err
			}
			for _, o := range objs {
				mark := " "
				if o.Key == s.cfg.COS.FARAuthObject || o.Key == s.cfg.COS.JWTObject {
					mark = "*"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %-40s %8d  %s\n", mark, o.Key, o.Size, o.Modified.Format(time.RFC3339))
			}
			return nil
		},
	}
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Check the FAR key logs in to FAR and the JWT parses",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if _, err := s.JWT(ctx); err != nil {
				return err
			}
			s.p.ok("subscription JWT found and well-formed")
			pl, err := s.Puller(ctx, true)
			if err != nil {
				return err
			}
			if _, err := pl.Digest(ctx, far.ManifestChartRef(s.cfg.Registry.FARHost, s.cfg.BNK.Version)); err != nil {
				return fmt.Errorf("the FAR key cannot read BNK %s from %s (an EA key on a GA install answers 403): %w", s.cfg.BNK.Version, s.cfg.Registry.FARHost, err)
			}
			s.p.ok("FAR key reads BNK %s from %s", s.cfg.BNK.Version, s.cfg.Registry.FARHost)
			return nil
		},
	}
	cmd.AddCommand(publish, list, verify)
	return cmd
}

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
