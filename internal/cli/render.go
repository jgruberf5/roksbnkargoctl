package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"github.com/jgruberf5/roksbnkargoctl/internal/registry"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// CheckImageRepo is where release builds publish the check image.
const CheckImageRepo = "ghcr.io/jgruberf5/roksbnkargoctl-check"

func newRenderCmd() *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Write every manifest the install consists of into the workspace",
		Long: `render pulls the BNK 2.4 GA manifest, the FLO chart and the cert-manager chart
(from FAR or the mirror), templates them in-process, and writes:

  manifests/git/            exactly what is published to Git and synced by Argo CD
  manifests/direct/         what install writes straight into ROKS (Secrets REDACTED)
  manifests/application.yaml

Nothing in the cluster, Argo CD or Git changes. Review manifests/git/ before
install: it is the whole of what the customer's repo will contain.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if err := s.cfg.Validate(); err != nil {
				return err
			}
			o, err := renderAll(cmd.Context(), s, !offline)
			if err != nil {
				return err
			}
			if err := render.Write(o, s.ws.ManifestsDir()); err != nil {
				return err
			}
			s.p.ok("rendered %d Git objects and %d direct objects into %s", len(o.Git), len(o.Direct), s.ws.ManifestsDir())
			for _, l := range render.Summary(o.Git) {
				s.p.info("%s", l)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "no-cluster", false, "do not read the cluster (Kubernetes version and node image fall back to defaults)")
	return cmd
}

// releaseTag matches the tags CI publishes a check image for (vX.Y.Z).
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// checkImageTag is the check image tag this build uses: its own version for a
// release build, otherwise :dev (what main publishes). A `git describe` version
// like "2ed6902" or "v0.1.0-3-gabc" has no image, and rendering it would leave
// every check pod in ImagePullBackOff.
func checkImageTag(version string) string {
	if releaseTag.MatchString(version) {
		return version
	}
	return "dev"
}

// pinDigest resolves a tag to repo@sha256:… so what ROKS runs is exactly what
// was rendered. With a mutable tag (:dev) and IfNotPresent, nodes that cached an
// older image keep running it — a re-sync after a check fix would silently run
// the unfixed check. A new image also changes the render, re-rolling the probes.
func pinDigest(ctx context.Context, pl *far.Puller, ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		return ref, nil
	}
	d, err := pl.Digest(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolving the check image %s: %w", ref, err)
	}
	return repoOf(ref) + "@" + d, nil
}

// resolveCheckImage returns the check image ROKS will run, pinned by digest.
//
// The digest comes from UPSTREAM (the source image), not from the mirror. Found
// live: a mirror does not follow a mutable tag, and resolving :dev against
// Artifactory pinned — faithfully — a copy replicated before the check was fixed,
// so the cluster ran a check the code no longer contained. In mirror mode the
// upstream digest must therefore already be in the mirror; if it is not, install
// stops and says to replicate. If upstream is unreachable (an operator host with
// no internet), the mirror's own digest is used, with a warning that its
// currency could not be checked.
func resolveCheckImage(ctx context.Context, pl *far.Puller, c *config.Config, warn func(string, ...any)) (string, error) {
	src := c.Check.Image
	if src == "" {
		src = CheckImageRepo + ":" + checkImageTag(Version)
	}
	upstream, upErr := pinDigest(ctx, pl, src)
	if c.Registry.Source != config.SourceMirror {
		return upstream, upErr
	}
	mirrorRef := registry.Artifact{Source: src}.Dest(c.ImageHost())
	if upErr != nil {
		warn("could not resolve %s upstream (%v); using the mirror's copy without checking it is current", src, upErr)
		return pinDigest(ctx, pl, mirrorRef)
	}
	// The mirror holds the linux/amd64 manifest (replicate narrows multi-arch
	// images to it), so compare that platform's digest — found live: comparing
	// the upstream INDEX digest refused every mirror install.
	digest, err := pl.PlatformDigest(ctx, src)
	if err != nil {
		return "", fmt.Errorf("resolving %s for linux/amd64: %w", src, err)
	}
	mirrorPinned := repoOf(mirrorRef) + "@" + digest
	if _, err := pl.Digest(ctx, mirrorPinned); err != nil {
		have := "no copy"
		if d, err := pl.Digest(ctx, mirrorRef); err == nil {
			have = "an older copy (" + d + ")"
		}
		return "", fmt.Errorf("the mirror has %s of the check image, not %s (%s, linux/amd64): run `roksbnkargoctl registry replicate` so the cluster runs the current check", have, digest, src)
	}
	return mirrorPinned, nil
}

// repoOf strips a :tag or @digest from an image reference.
func repoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// flpOutputs is what `flp up` records, and what an external FLP config supplies.
type flpOutputs struct {
	Name       string `json:"name"`
	InstanceID string `json:"instance_id"`
	PrivateIP  string `json:"private_ip"`
	FloatingIP string `json:"floating_ip,omitempty"`
	URL        string `json:"url"`
	RootCAPEM  string `json:"root_ca_pem"`
	VPCID      string `json:"vpc_id"`
	VPCCreated bool   `json:"vpc_created"`
	// Everything flp up created, for flp down (ids by kind).
	Created map[string][]string `json:"created"`
	// TGW connection flp up made for a VPC it created.
	TGWConnectionID string `json:"tgw_connection_id,omitempty"`
}

func readFLP(s *session) (url, caPEM string, err error) {
	if s.cfg.BNK.Mode != config.ModeDisconnected {
		return "", "", nil
	}
	if e := s.cfg.FLP.External; e.URL != "" {
		b, err := os.ReadFile(e.RootCAFile)
		if err != nil {
			return "", "", fmt.Errorf("flp.external.root_ca_file: %w", err)
		}
		return e.URL, string(b), nil
	}
	b, err := os.ReadFile(s.ws.FLPOutputsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("disconnected mode needs an F5 License Proxy: run `roksbnkargoctl flp up`, or set flp.external")
		}
		return "", "", err
	}
	var o flpOutputs
	if err := json.Unmarshal(b, &o); err != nil {
		return "", "", fmt.Errorf("%s: %w", s.ws.FLPOutputsPath(), err)
	}
	return o.URL, o.RootCAPEM, nil
}

// renderAll does the network I/O a render needs and calls the pure renderer.
func renderAll(ctx context.Context, s *session, useCluster bool) (*render.Output, error) {
	c := s.cfg
	if _, err := s.resolved(); err != nil {
		return nil, err
	}
	needFAR := c.Registry.Source == config.SourceFAR
	pl, err := s.Puller(ctx, needFAR)
	if err != nil {
		return nil, err
	}
	s.p.step("fetching the BNK %s manifest from %s", c.BNK.Version, c.ChartHost())
	m, err := pl.FetchManifest(ctx, c.ChartHost(), c.BNK.Version)
	if err != nil {
		return nil, err
	}
	floVer, ok := m.Chart("f5-lifecycle-operator")
	if !ok {
		return nil, errors.New("the BNK manifest lists no f5-lifecycle-operator chart")
	}
	flo, err := pl.PullChart(ctx, c.ChartHost()+"/charts/f5-lifecycle-operator:"+floVer)
	if err != nil {
		return nil, err
	}
	var cm []byte
	if c.CertManagerInstall() {
		ref := "quay.io/jetstack/charts/cert-manager:" + c.BNK.CertManager.Version
		if c.Registry.Source == config.SourceMirror {
			ref = registry.Artifact{Source: ref}.Dest(c.ImageHost())
		}
		if cm, err = pl.PullChart(ctx, ref); err != nil {
			return nil, err
		}
	}
	mirrorCA, err := s.MirrorCA()
	if err != nil {
		return nil, err
	}
	kubeVersion, nodeImage := "v1.34.0", ""
	if useCluster {
		k, err := s.Kube(ctx)
		if err != nil {
			return nil, err
		}
		if v, err := k.ServerVersion(); err == nil {
			kubeVersion = v
		}
		if mirrorCA != "" {
			if nodeImage, err = k.NodeResolverImage(ctx); err != nil {
				return nil, err
			}
		}
	} else if mirrorCA != "" {
		nodeImage = c.Resolved.NodeResolverImage
	}
	if nodeImage != "" {
		c.Resolved.NodeResolverImage = nodeImage
	}
	flpURL, flpCA, err := readFLP(s)
	if err != nil {
		return nil, err
	}
	sec := render.Secrets{FLPCAPEM: flpCA}
	if needFAR {
		sa, err := s.FARServiceAccount(ctx)
		if err != nil {
			return nil, err
		}
		sec.PullHost, sec.PullUsername, sec.PullPassword = c.Registry.FARHost, far.FARUsername, sa
	} else if c.Registry.Mirror.Username != "" {
		pw := s.MirrorPassword()
		if pw == "" {
			return nil, fmt.Errorf("the mirror password: set %s", c.Registry.Mirror.PasswordEnv)
		}
		sec.PullHost, sec.PullUsername, sec.PullPassword = mirrorRegistryHost(c), c.Registry.Mirror.Username, pw
	}
	if sec.JWT, err = s.JWT(ctx); err != nil {
		return nil, err
	}
	// The run id hashes the config WITHOUT its resolved/bookkeeping section:
	// recording a published commit must not change the next render.
	stable := *c
	stable.Resolved = nil
	cfgYAML, _ := config.Marshal(&stable)
	img, err := resolveCheckImage(ctx, pl, c, s.p.warn)
	if err != nil {
		return nil, err
	}
	runID := render.RunID(string(cfgYAML), m.Version, img, flpURL, mirrorCA, nodeImage)
	return render.Render(render.Inputs{
		Config: c, Workspace: s.ws.Name, Manifest: m, CertManagerChart: cm, FLOChart: flo,
		KubeVersion: kubeVersion, CheckImage: img, NodeResolverImage: nodeImage, MirrorCAPEM: mirrorCA,
		FLPURL: flpURL, RunID: runID, Secrets: sec,
	})
}
