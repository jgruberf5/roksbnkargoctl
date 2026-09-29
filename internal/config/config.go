// Package config holds the workspace model: one directory per install under
// ~/.roksbnkargoctl/<name>/ with a config.yaml, the rendered manifests, and the
// outputs of the flp and argocd components.
//
// No secret value is ever stored here. The IBM API key, the Argo CD token and the
// Git token are read from environment variables named in the config; the FAR key
// and the subscription JWT live in COS (or a local file the operator owns).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// BNKVersion is the only BNK line this tool installs (DESIGN.md C10). FAR tags
// 2.4.0 and 2.4.0-3.3175.0-0.0.380 are the same artifact.
const BNKVersion = "2.4.0"

// Modes and registry sources.
const (
	ModeConnected    = "connected"
	ModeDisconnected = "disconnected"
	SourceFAR        = "far"
	SourceMirror     = "mirror"
)

// Config is config.yaml. Field names are the YAML keys an operator writes.
type Config struct {
	IBMCloud       IBMCloud  `yaml:"ibmcloud"`
	Cluster        string    `yaml:"cluster"`
	TransitGateway string    `yaml:"transit_gateway"`
	BNK            BNK       `yaml:"bnk"`
	Registry       Registry  `yaml:"registry"`
	COS            COS       `yaml:"cos"`
	FLP            FLP       `yaml:"flp,omitempty"`
	ArgoCD         ArgoCD    `yaml:"argocd"`
	Git            Git       `yaml:"git"`
	Check          Check     `yaml:"check,omitempty"`
	TestHub        TestHub   `yaml:"test_hub,omitempty"`
	Resolved       *Resolved `yaml:"resolved,omitempty"`
}

// IBMCloud selects the account context. The key itself is only ever read from the
// environment variable named by APIKeyEnv.
type IBMCloud struct {
	Region        string `yaml:"region"`
	ResourceGroup string `yaml:"resource_group,omitempty"`
	APIKeyEnv     string `yaml:"api_key_env,omitempty"`
}

// BNK is the install shape. Everything not listed here is fixed by DESIGN.md.
type BNK struct {
	Version        string `yaml:"version"`
	Mode           string `yaml:"mode"`
	TMMReplicas    int    `yaml:"tmm_replicas,omitempty"`
	Namespace      string `yaml:"namespace,omitempty"`
	UtilsNamespace string `yaml:"utils_namespace,omitempty"`
	// CertManager: install the pinned cert-manager (default) or adopt an existing one.
	CertManager CertManager `yaml:"cert_manager,omitempty"`
	// NADAddress is the static address on the ens3 ipvlan attachment (roksbnkctl default).
	NADAddress string `yaml:"nad_address,omitempty"`
	// StorageClass for BNK components that request one; empty uses the default class.
	StorageClass string `yaml:"storage_class,omitempty"`
}

// CertManager controls the cert-manager dependency.
type CertManager struct {
	Install *bool  `yaml:"install,omitempty"`
	Version string `yaml:"version,omitempty"`
}

// Registry says where ROKS pulls BNK images from.
type Registry struct {
	Source string `yaml:"source"`
	// FARHost is FAR's registry host.
	FARHost string `yaml:"far_host,omitempty"`
	Mirror  Mirror `yaml:"mirror,omitempty"`
}

// Mirror is a replicated private registry (Harbor, Artifactory, registry:2, ICR).
type Mirror struct {
	Host        string `yaml:"host,omitempty"`
	Prefix      string `yaml:"prefix,omitempty"`
	Username    string `yaml:"username,omitempty"`
	PasswordEnv string `yaml:"password_env,omitempty"`
	CAFile      string `yaml:"ca_file,omitempty"`
}

// COS locates the FAR auth tarball and the subscription JWT. LocalFARAuthFile /
// LocalJWTFile bypass COS for operators who hold the files directly.
type COS struct {
	Instance         string `yaml:"instance,omitempty"`
	Bucket           string `yaml:"bucket,omitempty"`
	Region           string `yaml:"region,omitempty"`
	FARAuthObject    string `yaml:"far_auth_object,omitempty"`
	JWTObject        string `yaml:"jwt_object,omitempty"`
	LocalFARAuthFile string `yaml:"local_far_auth_file,omitempty"`
	LocalJWTFile     string `yaml:"local_jwt_file,omitempty"`
}

// FLP is the F5 License Proxy for disconnected mode. Either `flp up` builds a VSI
// (VSI block) or the operator points at an existing one (External block).
type FLP struct {
	VSI      FLPVSI      `yaml:"vsi,omitempty"`
	External FLPExternal `yaml:"external,omitempty"`
}

// FLPVSI parameterises `flp up`.
type FLPVSI struct {
	Zone         string   `yaml:"zone,omitempty"`
	VPC          string   `yaml:"vpc,omitempty"` // existing VPC name/id; empty creates one
	CIDR         string   `yaml:"cidr,omitempty"`
	Profile      string   `yaml:"profile,omitempty"`
	AllowedCIDRs []string `yaml:"allowed_cidrs,omitempty"` // may reach :8443
	SSHKey       string   `yaml:"ssh_key,omitempty"`       // existing VPC SSH key name
	FloatingIP   *bool    `yaml:"floating_ip,omitempty"`
}

// FLPExternal points at an FLP this tool did not build.
type FLPExternal struct {
	URL        string `yaml:"url,omitempty"`
	RootCAFile string `yaml:"root_ca_file,omitempty"`
}

// ArgoCD is the customer's existing, external Argo CD.
type ArgoCD struct {
	Server      string `yaml:"server"`
	TokenEnv    string `yaml:"token_env,omitempty"`
	Insecure    bool   `yaml:"insecure,omitempty"`
	CAFile      string `yaml:"ca_file,omitempty"`
	Project     string `yaml:"project,omitempty"`
	Application string `yaml:"application,omitempty"`
	// ClusterEndpoint selects the ROKS API endpoint Argo CD dials: private (over the
	// transit gateway, default) or public.
	ClusterEndpoint string `yaml:"cluster_endpoint,omitempty"`
}

// Git is the customer repo Argo CD syncs from.
type Git struct {
	URL         string `yaml:"url"`
	Branch      string `yaml:"branch,omitempty"`
	Path        string `yaml:"path,omitempty"`
	Username    string `yaml:"username,omitempty"`
	TokenEnv    string `yaml:"token_env,omitempty"`
	SSHKeyFile  string `yaml:"ssh_key_file,omitempty"`
	AuthorName  string `yaml:"author_name,omitempty"`
	AuthorEmail string `yaml:"author_email,omitempty"`
}

// Check pins the check image.
type Check struct {
	Image string `yaml:"image,omitempty"`
}

// TestHub parameterises `argocd up`, the test Argo CD VSI.
type TestHub struct {
	Region       string `yaml:"region,omitempty"`
	Zone         string `yaml:"zone,omitempty"`
	CIDR         string `yaml:"cidr,omitempty"`
	Profile      string `yaml:"profile,omitempty"`
	Version      string `yaml:"version,omitempty"`
	AllowedCIDR  string `yaml:"allowed_cidr,omitempty"`
	SSHKey       string `yaml:"ssh_key,omitempty"`
	NodePort     int    `yaml:"node_port,omitempty"`
	K3sChannel   string `yaml:"k3s_channel,omitempty"`
	AttachToTGW  *bool  `yaml:"attach_to_tgw,omitempty"`
	ResourceName string `yaml:"name,omitempty"`
}

// Resolved caches what `init` looked up, so later commands and the rendered
// manifests do not depend on a live lookup. `init --refresh` rewrites it.
type Resolved struct {
	ClusterID              string   `yaml:"cluster_id"`
	ClusterName            string   `yaml:"cluster_name"`
	ClusterCRN             string   `yaml:"cluster_crn"`
	ClusterVersion         string   `yaml:"cluster_version,omitempty"`
	VPCID                  string   `yaml:"vpc_id"`
	VPCName                string   `yaml:"vpc_name"`
	VPCCRN                 string   `yaml:"vpc_crn"`
	PrivateEndpoint        string   `yaml:"private_endpoint,omitempty"`
	PublicEndpoint         string   `yaml:"public_endpoint,omitempty"`
	Zones                  []string `yaml:"zones"`
	TransitGatewayID       string   `yaml:"transit_gateway_id"`
	TransitGatewayName     string   `yaml:"transit_gateway_name"`
	ClusterAttachedToTGW   bool     `yaml:"cluster_attached_to_tgw"`
	COSInstanceCRN         string   `yaml:"cos_instance_crn,omitempty"`
	ResourceGroupID        string   `yaml:"resource_group_id,omitempty"`
	NodeResolverImage      string   `yaml:"node_resolver_image,omitempty"`
	TrustedProfileID       string   `yaml:"trusted_profile_id,omitempty"`
	ArgoCDClusterServer    string   `yaml:"argocd_cluster_server,omitempty"`
	LastPublishedCommitSHA string   `yaml:"last_published_commit,omitempty"`
	// TGWConnectionCreatedID is set when install attached the cluster VPC to the
	// gateway itself, so uninstall --detach-tgw removes only what install added.
	TGWConnectionCreatedID string `yaml:"tgw_connection_created_id,omitempty"`
}

// Defaults fills every optional field with the value DESIGN.md specifies.
func (c *Config) Defaults(workspace string) {
	if c.IBMCloud.APIKeyEnv == "" {
		c.IBMCloud.APIKeyEnv = "IBMCLOUD_API_KEY"
	}
	if c.IBMCloud.ResourceGroup == "" {
		c.IBMCloud.ResourceGroup = "default"
	}
	b := &c.BNK
	if b.Version == "" {
		b.Version = BNKVersion
	}
	if b.Mode == "" {
		b.Mode = ModeConnected
	}
	if b.TMMReplicas == 0 {
		b.TMMReplicas = 3
	}
	if b.Namespace == "" {
		b.Namespace = "f5-bnk"
	}
	if b.UtilsNamespace == "" {
		b.UtilsNamespace = "f5-utils"
	}
	if b.CertManager.Version == "" {
		b.CertManager.Version = "v1.17.3"
	}
	if b.CertManager.Install == nil {
		t := true
		b.CertManager.Install = &t
	}
	if b.NADAddress == "" {
		b.NADAddress = "10.10.1.1/24"
	}
	if c.Registry.Source == "" {
		c.Registry.Source = SourceFAR
	}
	if c.Registry.FARHost == "" {
		c.Registry.FARHost = "repo.f5.com"
	}
	if c.Registry.Mirror.PasswordEnv == "" {
		c.Registry.Mirror.PasswordEnv = "ROKSBNKARGOCTL_MIRROR_PASSWORD"
	}
	o := &c.COS
	if o.Instance == "" && o.LocalFARAuthFile == "" {
		o.Instance = "bnk-supply-chain"
	}
	if o.Region == "" {
		o.Region = "us-south"
	}
	if o.FARAuthObject == "" {
		o.FARAuthObject = "f5-far-auth-key.tgz"
	}
	if o.JWTObject == "" {
		o.JWTObject = "subscription.jwt"
	}
	if c.ArgoCD.TokenEnv == "" {
		c.ArgoCD.TokenEnv = "ARGOCD_AUTH_TOKEN"
	}
	if c.ArgoCD.Project == "" {
		c.ArgoCD.Project = "default"
	}
	if c.ArgoCD.Application == "" {
		c.ArgoCD.Application = "bnk-" + workspace
	}
	if c.ArgoCD.ClusterEndpoint == "" {
		c.ArgoCD.ClusterEndpoint = "private"
	}
	g := &c.Git
	if g.Branch == "" {
		g.Branch = "main"
	}
	if g.Path == "" {
		g.Path = "bnk/" + workspace
	}
	if g.TokenEnv == "" && g.SSHKeyFile == "" {
		g.TokenEnv = "ROKSBNKARGOCTL_GIT_TOKEN"
	}
	if g.AuthorName == "" {
		g.AuthorName = "roksbnkargoctl"
	}
	if g.AuthorEmail == "" {
		g.AuthorEmail = "roksbnkargoctl@users.noreply.github.com"
	}
	f := &c.FLP.VSI
	if f.Profile == "" {
		f.Profile = "bx2-4x16"
	}
	if len(f.AllowedCIDRs) == 0 {
		f.AllowedCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	}
	h := &c.TestHub
	if h.Profile == "" {
		h.Profile = "bx2-4x16"
	}
	if h.Version == "" {
		h.Version = "3.5.1"
	}
	if h.NodePort == 0 {
		h.NodePort = 30443
	}
	if h.K3sChannel == "" {
		h.K3sChannel = "stable"
	}
	if h.AllowedCIDR == "" {
		h.AllowedCIDR = "0.0.0.0/0"
	}
	if h.ResourceName == "" {
		h.ResourceName = workspace + "-argocd"
	}
	if h.AttachToTGW == nil {
		t := true
		h.AttachToTGW = &t
	}
}

// CertManagerInstall reports whether the pinned cert-manager is part of the install.
func (c *Config) CertManagerInstall() bool {
	return c.BNK.CertManager.Install == nil || *c.BNK.CertManager.Install
}

// Validate rejects configs the tool cannot install. It is deliberately strict: a
// wrong value found here costs a second; found by a failed sync it costs an hour.
func (c *Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if c.IBMCloud.Region == "" {
		add("ibmcloud.region is required")
	}
	if c.Cluster == "" {
		add("cluster (name or id of the existing ROKS cluster) is required")
	}
	if c.TransitGateway == "" {
		add("transit_gateway (name or id of the existing transit gateway) is required")
	}
	if c.BNK.Version != BNKVersion {
		add("bnk.version %q is not supported: this tool installs BNK %s GA only", c.BNK.Version, BNKVersion)
	}
	switch c.BNK.Mode {
	case ModeConnected:
	case ModeDisconnected:
		if c.FLP.External.URL != "" && c.FLP.External.RootCAFile == "" {
			add("flp.external.root_ca_file is required with flp.external.url")
		}
	default:
		add("bnk.mode %q: must be %q or %q", c.BNK.Mode, ModeConnected, ModeDisconnected)
	}
	if c.BNK.TMMReplicas < 1 {
		add("bnk.tmm_replicas must be >= 1")
	}
	switch c.Registry.Source {
	case SourceFAR:
	case SourceMirror:
		if c.Registry.Mirror.Host == "" {
			add("registry.mirror.host is required with registry.source: mirror")
		}
		if strings.Contains(c.Registry.Mirror.Host, "://") {
			add("registry.mirror.host %q must be a bare host[:port], without a scheme", c.Registry.Mirror.Host)
		}
	default:
		add("registry.source %q: must be %q or %q", c.Registry.Source, SourceFAR, SourceMirror)
	}
	if c.COS.LocalFARAuthFile == "" && (c.COS.Instance == "" || c.COS.Bucket == "") {
		add("cos.instance and cos.bucket are required unless cos.local_far_auth_file is set")
	}
	if c.ArgoCD.Server == "" {
		add("argocd.server (https URL of the existing Argo CD) is required")
	} else if !strings.HasPrefix(c.ArgoCD.Server, "https://") && !strings.HasPrefix(c.ArgoCD.Server, "http://") {
		add("argocd.server %q must start with https://", c.ArgoCD.Server)
	}
	switch c.ArgoCD.ClusterEndpoint {
	case "private", "public":
	default:
		add("argocd.cluster_endpoint %q: must be private or public", c.ArgoCD.ClusterEndpoint)
	}
	if c.Git.URL == "" {
		add("git.url (the repo Argo CD syncs from) is required")
	}
	if strings.HasPrefix(c.Git.Path, "/") || strings.Contains(c.Git.Path, "..") {
		add("git.path %q must be a relative path inside the repo", c.Git.Path)
	}
	if len(errs) > 0 {
		return errors.New("invalid config:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// Parse decodes config.yaml strictly: an unknown key is an error, because a typo
// in an optional key otherwise silently installs the default.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config.yaml: %w", err)
	}
	return &c, nil
}

// Marshal renders the config for writing.
func Marshal(c *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- workspaces -------------------------------------------------------------

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidateName keeps workspace names usable as Kubernetes and IBM resource names.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("workspace name %q: use 1-40 lowercase letters, digits and '-', starting and ending alphanumeric", name)
	}
	return nil
}

// Home is the root of all workspaces.
func Home() (string, error) {
	if h := os.Getenv("ROKSBNKARGOCTL_HOME"); h != "" {
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".roksbnkargoctl"), nil
}

// Workspace is one install's directory.
type Workspace struct {
	Name string
	Dir  string
}

// Open returns the workspace handle without requiring it to exist.
func Open(name string) (*Workspace, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	h, err := Home()
	if err != nil {
		return nil, err
	}
	return &Workspace{Name: name, Dir: filepath.Join(h, name)}, nil
}

// Paths inside a workspace.
func (w *Workspace) ConfigPath() string     { return filepath.Join(w.Dir, "config.yaml") }
func (w *Workspace) ManifestsDir() string   { return filepath.Join(w.Dir, "manifests") }
func (w *Workspace) CacheDir() string       { return filepath.Join(w.Dir, "cache") }
func (w *Workspace) FLPOutputsPath() string { return filepath.Join(w.Dir, "flp-outputs.json") }
func (w *Workspace) HubOutputsPath() string { return filepath.Join(w.Dir, "argocd-hub.json") }
func (w *Workspace) MirrorRecordPath() string {
	return filepath.Join(w.Dir, "registry-mirror.json")
}
func (w *Workspace) ChecksDir() string { return filepath.Join(w.Dir, "checks") }

// Exists reports whether the workspace has a config.
func (w *Workspace) Exists() bool {
	_, err := os.Stat(w.ConfigPath())
	return err == nil
}

// Load reads, defaults and returns config.yaml (without validating: `init` and
// `status` must work on a half-filled config).
func (w *Workspace) Load() (*Config, error) {
	data, err := os.ReadFile(w.ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workspace %q has no config.yaml; run `roksbnkargoctl init -w %s`", w.Name, w.Name)
		}
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	c.Defaults(w.Name)
	return c, nil
}

// Save writes config.yaml owner-only.
func (w *Workspace) Save(c *Config) error {
	if err := os.MkdirAll(w.Dir, 0o700); err != nil {
		return err
	}
	data, err := Marshal(c)
	if err != nil {
		return err
	}
	return WriteFileAtomic(w.ConfigPath(), data, 0o600)
}

// List returns the workspace names under Home.
func List() ([]string, error) {
	h, err := Home()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(h)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(h, e.Name(), "config.yaml")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	return out, nil
}

// WriteFileAtomic writes via a temp file and rename, so a crash never leaves a
// half-written config or output file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !isWindows() {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func isWindows() bool { return os.PathSeparator == '\\' }

// Env reads a required secret from the environment variable name, with a message
// that names the variable (never the value).
func Env(name, what string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("%s: set the environment variable %s", what, name)
	}
	return v, nil
}

// APIKey returns the IBM Cloud API key: the configured variable, then IC_API_KEY.
func (c *Config) APIKey() (string, error) {
	if v := strings.TrimSpace(os.Getenv(c.IBMCloud.APIKeyEnv)); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("IC_API_KEY")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("IBM Cloud API key: set the environment variable %s", c.IBMCloud.APIKeyEnv)
}

// ImageHost is where ROKS pulls BNK images from: FAR or the mirror.
func (c *Config) ImageHost() string {
	if c.Registry.Source == SourceMirror {
		return c.MirrorBase()
	}
	return c.Registry.FARHost
}

// MirrorBase is <mirror-host>/<prefix>, whatever registry.source says (the
// registry commands fill the mirror before the install switches to it).
func (c *Config) MirrorBase() string {
	return strings.TrimSuffix(c.Registry.Mirror.Host+"/"+strings.Trim(c.Registry.Mirror.Prefix, "/"), "/")
}

// ChartHost is where FLO's chart is pulled from at render time.
func (c *Config) ChartHost() string { return c.ImageHost() }

// PullSecretName is the imagePullSecret every BNK workload references.
func (c *Config) PullSecretName() string {
	if c.Registry.Source == SourceMirror {
		return "mirror-secret"
	}
	return "far-secret"
}
