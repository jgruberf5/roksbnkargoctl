package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"github.com/jgruberf5/roksbnkargoctl/internal/registry"
)

// mirrorRecord is what `registry replicate` writes after a run: a report for
// the operator and the agent personas (AGENTS.md lists it). Nothing in the tool
// reads it back, so a run that writes none loses no behaviour.
type mirrorRecord struct {
	Mirror    string    `json:"mirror"`
	Version   string    `json:"bnk_version"`
	Artifacts int       `json:"artifacts"`
	Copied    int       `json:"copied"`
	Skipped   int       `json:"skipped"`
	Failed    []string  `json:"failed,omitempty"`
	When      time.Time `json:"when"`
}

// bindBOMFlags registers the flags for every input the BOM is built from: the
// FAR host, the FAR key (a local tarball, or COS), whether and which
// cert-manager is included, and the check image.
func bindBOMFlags(c *cobra.Command) {
	bindConfigFlag(c, "far-host", "registry.far_host")
	bindConfigFlag(c, "far-auth-file", "cos.local_far_auth_file")
	bindConfigFlag(c, "cos-instance", "cos.instance")
	bindConfigFlag(c, "cos-bucket", "cos.bucket")
	bindConfigFlag(c, "cos-region", "cos.region")
	bindConfigFlag(c, "far-auth-object", "cos.far_auth_object")
	bindConfigFlag(c, "api-key-env", "ibmcloud.api_key_env")
	bindConfigFlag(c, "cert-manager-install", "bnk.cert_manager.install")
	bindConfigFlag(c, "cert-manager-version", "bnk.cert_manager.version")
	bindConfigFlag(c, "check-image", "check.image")
}

// bindMirrorFlags registers one flag per registry.mirror key, named
// --mirror-<key>. The prefix keeps them apart from --far-host and reads as the
// key they set (registry.mirror.host is --mirror-host).
func bindMirrorFlags(c *cobra.Command) []string {
	return bindConfigFlags(c, "registry.mirror", "mirror-")
}

// needFARKey fails early, naming the flags, when nothing says where the FAR key is.
func needFARKey(s *session) error {
	if s.cfg.COS.LocalFARAuthFile == "" && s.cfg.COS.Bucket == "" {
		return errors.New("no FAR key: pass --far-auth-file <f5-far-auth-key.tgz>, or --cos-bucket (with --cos-instance) to read it from COS")
	}
	return nil
}

// needMirror fails early when no usable mirror host is configured.
func needMirror(s *session) error {
	h := s.cfg.Registry.Mirror.Host
	if h == "" {
		return fmt.Errorf("registry.mirror.host is not set: pass --mirror-host (or set %s, or registry.mirror.host in config.yaml)", config.EnvName("registry.mirror.host"))
	}
	if strings.Contains(h, "://") {
		return fmt.Errorf("registry.mirror.host %q must be a bare host[:port], without a scheme", h)
	}
	return nil
}

// bomFor builds the BOM from the effective config.
func bomFor(ctx context.Context, s *session) ([]registry.Artifact, *far.Puller, error) {
	if err := needFARKey(s); err != nil {
		return nil, nil, err
	}
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
  roksbnkargoctl registry verify      report anything missing

None of them needs a workspace: every setting they read is also a flag (and a
ROKSBNKARGOCTL_* variable), and a flag wins over config.yaml. The mirror
password is never a flag; it is read from the variable --mirror-password-env
names (default ROKSBNKARGOCTL_MIRROR_PASSWORD).

  roksbnkargoctl registry verify --no-workspace --far-auth-file f5-far-auth-key.tgz \
      --mirror-host registry.example.com --mirror-prefix bnk --mirror-username robot`,
	}
	bom := &cobra.Command{Use: "bom", Short: "List the artifacts an install pulls",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
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
	bindBOMFlags(bom)

	var concurrency int
	var statePath string
	replicate := &cobra.Command{Use: "replicate", Short: "Copy the BOM into the mirror",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
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
			if err := writeMirrorRecord(s, statePath, rec); err != nil {
				return err
			}
			if len(rec.Failed) > 0 {
				return fmt.Errorf("%d of %d artifacts failed to copy", len(rec.Failed), len(b))
			}
			s.p.ok("mirror complete: %d copied, %d already present", rec.Copied, rec.Skipped)
			return nil
		}}
	replicate.Flags().IntVar(&concurrency, "concurrency", 2, "parallel copies")
	replicate.Flags().StringVar(&statePath, "state", "", "write the replication record (JSON) here (default: <workspace>/registry-mirror.json; without a workspace none is written unless this is given)")
	bindBOMFlags(replicate)
	bindMirrorFlags(replicate)

	verify := &cobra.Command{Use: "verify", Short: "Report artifacts missing from the mirror",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSessionOptional(cmd)
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
	bindBOMFlags(verify)
	bindMirrorFlags(verify)

	cmd.AddCommand(bom, replicate, verify)
	return cmd
}

// writeMirrorRecord writes the replication record to --state, else to the
// workspace, else nowhere. A --state the operator asked for that cannot be
// written is an error; the workspace copy is best effort, as it always was.
func writeMirrorRecord(s *session, statePath string, rec mirrorRecord) error {
	path, explicit := statePath, statePath != ""
	if !explicit && s.ws != nil {
		path = s.ws.MirrorRecordPath()
	}
	if path == "" {
		return nil
	}
	jb, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path, jb, 0o600); err != nil {
		if explicit {
			return fmt.Errorf("--state: %w", err)
		}
		s.p.warn("could not write %s: %v", path, err)
	}
	return nil
}

// checkImageSource is the upstream check image for this build (before any
// mirror redirect).
func checkImageSource() string { return CheckImageRepo + ":" + checkImageTag(Version) }
