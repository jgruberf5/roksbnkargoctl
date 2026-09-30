package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/agent"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"github.com/jgruberf5/roksbnkargoctl/internal/registry"
)

// ---- registry --------------------------------------------------------------------

type mirrorRecord struct {
	Mirror    string    `json:"mirror"`
	Version   string    `json:"bnk_version"`
	Artifacts int       `json:"artifacts"`
	Copied    int       `json:"copied"`
	Skipped   int       `json:"skipped"`
	Failed    []string  `json:"failed,omitempty"`
	When      time.Time `json:"when"`
}

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Replicate BNK 2.4 GA into a private registry (for clusters without internet egress)",
		Long: `registry copies every artifact an install pulls — the BNK manifest's charts and
images from FAR, cert-manager's chart and images, and the check image — into
registry.mirror.host under registry.mirror.prefix, keeping each source path
(repo.f5.com/images/x → <mirror>/images/x). Set registry.source: mirror to
install from it.

  roksbnkargoctl registry bom         list what would be copied
  roksbnkargoctl registry replicate   copy (skips what is already there)
  roksbnkargoctl registry verify      report anything missing`,
	}
	bomFor := func(ctx context.Context, s *session) ([]registry.Artifact, *far.Puller, error) {
		pl, err := s.Puller(ctx, true)
		if err != nil {
			return nil, nil, err
		}
		m, err := pl.FetchManifest(ctx, s.cfg.Registry.FARHost, s.cfg.BNK.Version)
		if err != nil {
			return nil, nil, err
		}
		src := s.cfg.Check.Image
		if src == "" {
			src = checkImageSource()
		}
		return registry.BOM(m, s.cfg.Registry.FARHost, s.cfg.BNK.CertManager.Version, src, s.cfg.CertManagerInstall()), pl, nil
	}
	needMirror := func(s *session) error {
		if s.cfg.Registry.Mirror.Host == "" {
			return errors.New("registry.mirror.host is not set")
		}
		return nil
	}
	bom := &cobra.Command{Use: "bom", Short: "List the artifacts an install pulls",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			b, _, err := bomFor(cmd.Context(), s)
			if err != nil {
				return err
			}
			for _, a := range b {
				fmt.Fprintf(cmd.OutOrStdout(), "%-6s %s\n", a.Kind, a.Source)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%d artifacts\n", len(b))
			return nil
		}}
	var concurrency int
	replicate := &cobra.Command{Use: "replicate", Short: "Copy the BOM into the mirror",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if err := needMirror(s); err != nil {
				return err
			}
			b, pl, err := bomFor(cmd.Context(), s)
			if err != nil {
				return err
			}
			mirror := s.cfg.MirrorBase()
			s.p.step("replicating %d artifacts into %s", len(b), mirror)
			done := 0
			res := registry.Replicate(cmd.Context(), pl, mirror, b, concurrency, func(r registry.Result) {
				done++
				switch {
				case r.Err != nil:
					s.p.warn("[%d/%d] %s: %v", done, len(b), r.Artifact.Source, r.Err)
				case r.Skipped:
					s.p.info("[%d/%d] present  %s", done, len(b), r.Artifact.Path())
				default:
					s.p.info("[%d/%d] copied   %s", done, len(b), r.Artifact.Path())
				}
			})
			rec := mirrorRecord{Mirror: mirror, Version: s.cfg.BNK.Version, Artifacts: len(b), When: time.Now().UTC()}
			for _, r := range res {
				switch {
				case r.Err != nil:
					rec.Failed = append(rec.Failed, r.Artifact.Source)
				case r.Skipped:
					rec.Skipped++
				default:
					rec.Copied++
				}
			}
			jb, _ := json.MarshalIndent(rec, "", "  ")
			_ = config.WriteFileAtomic(s.ws.MirrorRecordPath(), jb, 0o600)
			if len(rec.Failed) > 0 {
				return fmt.Errorf("%d of %d artifacts failed to copy", len(rec.Failed), len(b))
			}
			s.p.ok("mirror complete: %d copied, %d already present", rec.Copied, rec.Skipped)
			return nil
		}}
	replicate.Flags().IntVar(&concurrency, "concurrency", 2, "parallel copies")
	verify := &cobra.Command{Use: "verify", Short: "Report artifacts missing from the mirror",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if err := needMirror(s); err != nil {
				return err
			}
			b, pl, err := bomFor(cmd.Context(), s)
			if err != nil {
				return err
			}
			missing, err := registry.Verify(cmd.Context(), pl, s.cfg.MirrorBase(), b)
			if err != nil {
				return err
			}
			if len(missing) > 0 {
				for _, a := range missing {
					s.p.warn("missing %s", a.Path())
				}
				return fmt.Errorf("%d of %d artifacts missing; run `roksbnkargoctl registry replicate`", len(missing), len(b))
			}
			s.p.ok("all %d artifacts present in the mirror", len(b))
			return nil
		}}
	cmd.AddCommand(bom, replicate, verify)
	return cmd
}

// checkImageSource is the upstream check image for this build (before any
// mirror redirect).
func checkImageSource() string { return CheckImageRepo + ":" + checkImageTag(Version) }

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
