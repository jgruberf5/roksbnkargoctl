// Package far reads the F5 Artifact Registry (FAR) supply chain in-process: the
// service-account key inside F5's auth tarball, the BNK manifest chart, and any OCI
// Helm chart (FAR, a mirror, or quay.io for cert-manager) — all through
// go-containerregistry, never the helm binary.
package far

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"gopkg.in/yaml.v3"
)

// FARUsername is the fixed username FAR expects with a service-account key.
const FARUsername = "_json_key_base64"

// ServiceAccountFromTarball returns the base64 service-account key F5 ships as the
// first *.json entry in the auth .tgz. That string is the FAR password as-is.
func ServiceAccountFromTarball(tgz []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return "", fmt.Errorf("FAR auth file is not a gzip tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("FAR auth tarball has no .json service-account entry")
		}
		if err != nil {
			return "", fmt.Errorf("reading FAR auth tarball: %w", err)
		}
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(strings.ToLower(h.Name), ".json") {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return "", err
		}
		sa := strings.TrimSpace(string(b))
		if sa == "" {
			return "", fmt.Errorf("FAR auth tarball entry %s is empty", h.Name)
		}
		return sa, nil
	}
}

// DockerConfigJSON builds a kubernetes.io/dockerconfigjson payload for one host.
func DockerConfigJSON(host, username, password string) ([]byte, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return json.Marshal(map[string]any{
		"auths": map[string]any{
			host: map[string]string{"username": username, "password": password, "auth": auth},
		},
	})
}

// Credential is one registry's login, applied to that host only.
type Credential struct {
	Host     string
	Username string
	Password string
}

// Puller pulls OCI artifacts with per-host credentials and optional private CAs.
type Puller struct {
	creds     []Credential
	transport http.RoundTripper
}

// NewPuller returns a Puller. caPEM, when set, is trusted in addition to the system
// roots (a Harbor mirror with a self-signed CA).
func NewPuller(creds []Credential, caPEM []byte) (*Puller, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if len(caPEM) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("registry CA: no PEM certificates found")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Puller{creds: creds, transport: tr}, nil
}

// Resolve implements authn.Keychain: a credential is sent only to its own host,
// so the FAR key never reaches quay.io (which rejects it outright).
func (p *Puller) Resolve(r authn.Resource) (authn.Authenticator, error) {
	for _, c := range p.creds {
		if strings.EqualFold(r.RegistryStr(), c.Host) {
			return authn.FromConfig(authn.AuthConfig{Username: c.Username, Password: c.Password}), nil
		}
	}
	return authn.Anonymous, nil
}

func (p *Puller) opts(ctx context.Context) []crane.Option {
	return []crane.Option{crane.WithContext(ctx), crane.WithAuthFromKeychain(p), crane.WithTransport(p.transport)}
}

// ListTags lists a repository's tags.
func (p *Puller) ListTags(ctx context.Context, repo string) ([]string, error) {
	return crane.ListTags(repo, p.opts(ctx)...)
}

// Digest returns an artifact's manifest digest.
func (p *Puller) Digest(ctx context.Context, ref string) (string, error) {
	return crane.Digest(ref, p.opts(ctx)...)
}

// PullChart returns a Helm OCI chart's .tgz bytes (its single content layer).
func (p *Puller) PullChart(ctx context.Context, ref string) ([]byte, error) {
	if _, err := name.ParseReference(ref); err != nil {
		return nil, fmt.Errorf("chart reference %q: %w", ref, err)
	}
	img, err := crane.Pull(ref, p.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("pulling chart %s: %w", ref, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		mt, _ := l.MediaType()
		if strings.Contains(string(mt), "helm.chart.content") || len(layers) == 1 {
			rc, err := l.Compressed()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("chart %s has no helm chart content layer", ref)
}

// Options are crane options for callers that copy artifacts (registry replicate).
func (p *Puller) Options(ctx context.Context) []crane.Option { return p.opts(ctx) }

// ---- the BNK manifest -------------------------------------------------------

// Artifact is one chart or image the manifest lists. Name keeps its category
// prefix (charts/…, images/…, utils/…).
type Artifact struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

// Short is the name without the category prefix (what CNEManifest lists).
func (a Artifact) Short() string { return path.Base(a.Name) }

// Manifest is bigip-k8s-manifest-<version>.yaml.
type Manifest struct {
	Version    string     `json:"version"`
	HelmRepo   string     `json:"helm_repo"`
	DockerRepo string     `json:"docker_repo"`
	Charts     []Artifact `json:"charts"`
	Images     []Artifact `json:"images"`
}

// ParseManifest reads the manifest YAML inside the manifest chart.
func ParseManifest(data []byte) (*Manifest, error) {
	var raw struct {
		HelmRepo   string `yaml:"f5_helm_repo"`
		DockerRepo string `yaml:"f5_docker_repo"`
		Releases   []struct {
			Version string     `yaml:"version"`
			Charts  []Artifact `yaml:"helm_charts"`
			Images  []Artifact `yaml:"docker_images"`
		} `yaml:"releases"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("BNK manifest: %w", err)
	}
	if len(raw.Releases) != 1 {
		return nil, fmt.Errorf("BNK manifest: expected exactly one release, found %d", len(raw.Releases))
	}
	r := raw.Releases[0]
	m := &Manifest{Version: r.Version, HelmRepo: raw.HelmRepo, DockerRepo: raw.DockerRepo, Charts: r.Charts, Images: r.Images}
	if m.Version == "" || len(m.Charts) == 0 || len(m.Images) == 0 {
		return nil, errors.New("BNK manifest: release has no version, charts or images")
	}
	return m, nil
}

// Chart returns a chart's version by short name (e.g. "f5-lifecycle-operator").
func (m *Manifest) Chart(short string) (string, bool) {
	for _, c := range m.Charts {
		if c.Short() == short {
			return c.Version, true
		}
	}
	return "", false
}

// ManifestChartRef is where the manifest chart lives under a chart host.
func ManifestChartRef(host, version string) string {
	return strings.TrimSuffix(host, "/") + "/release/f5-bigip-k8s-manifest:" + version
}

// FetchManifest pulls the manifest chart from host (FAR or a mirror) and parses it.
func (p *Puller) FetchManifest(ctx context.Context, host, version string) (*Manifest, error) {
	tgz, err := p.PullChart(ctx, ManifestChartRef(host, version))
	if err != nil {
		return nil, err
	}
	files, err := ChartFiles(tgz)
	if err != nil {
		return nil, err
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := path.Base(n)
		if strings.HasPrefix(b, "bigip-k8s-manifest") && strings.HasSuffix(b, ".yaml") {
			return ParseManifest(files[n])
		}
	}
	return nil, fmt.Errorf("manifest chart %s has no bigip-k8s-manifest-*.yaml", version)
}

// ChartFiles unpacks a chart .tgz into path → bytes.
func ChartFiles(tgz []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, err
		}
		out[h.Name] = b
	}
}
