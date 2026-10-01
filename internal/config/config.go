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
//
// Every leaf setting carries a `help` tag and, unless tagged `override:"-"`,
// can be overridden by ROKSBNKARGOCTL_<KEY_PATH> and by a command flag bound
// to it (see Keys). Resolved is machine state, not a setting: it has neither.
type Config struct {
	IBMCloud       IBMCloud  `yaml:"ibmcloud"`
	Cluster        string    `yaml:"cluster" help:"name or id of the existing ROKS cluster"`
	TransitGateway string    `yaml:"transit_gateway" help:"name or id of the existing transit gateway"`
	BNK            BNK       `yaml:"bnk"`
	Registry       Registry  `yaml:"registry"`
	COS            COS       `yaml:"cos"`
	FLP            FLP       `yaml:"flp,omitempty"`
	ArgoCD         ArgoCD    `yaml:"argocd"`
	Git            Git       `yaml:"git"`
	Check          Check     `yaml:"check,omitempty"`
	TestHub        TestHub   `yaml:"test_hub,omitempty"`
	Forge          Forge     `yaml:"forge,omitempty"`
	Resolved       *Resolved `yaml:"resolved,omitempty"`
}

// IBMCloud selects the account context. The key itself is only ever read from the
// environment variable named by APIKeyEnv.
type IBMCloud struct {
	Region        string `yaml:"region" help:"IBM Cloud region of the ROKS cluster"`
	ResourceGroup string `yaml:"resource_group,omitempty" help:"resource group name or id"`
	APIKeyEnv     string `yaml:"api_key_env,omitempty" help:"environment variable holding the IBM Cloud API key"`
}

// BNK is the install shape. Everything not listed here is fixed by DESIGN.md.
type BNK struct {
	// Version has no environment or flag override: this build accepts exactly
	// one value, and ROKSBNKARGOCTL_BNK_VERSION already means "which BNK line's
	// binary to download" to install.sh and install.ps1.
	Version        string `yaml:"version" help:"BNK release; only this build's release is accepted" override:"-"`
	Mode           string `yaml:"mode" help:"connected or disconnected"`
	TMMReplicas    int    `yaml:"tmm_replicas,omitempty" help:"TMM replicas"`
	Namespace      string `yaml:"namespace,omitempty" help:"namespace BNK is installed into"`
	UtilsNamespace string `yaml:"utils_namespace,omitempty" help:"namespace for the BNK utilities"`
	// CertManager: install the pinned cert-manager (default) or adopt an existing one.
	CertManager CertManager `yaml:"cert_manager,omitempty"`
	// Certificates: cert-manager (default) or F5's single certificate.
	Certificates Certificates `yaml:"certificates,omitempty"`
	// NADAddress is the static address on the ens3 ipvlan attachment (roksbnkctl default).
	NADAddress string `yaml:"nad_address,omitempty" help:"static address on the ens3 ipvlan attachment"`
	// StorageClass for BNK components that request one; empty uses the default class.
	StorageClass string `yaml:"storage_class,omitempty" help:"StorageClass for BNK volumes; empty uses the default class"`
}

// CertManager controls the cert-manager dependency.
type CertManager struct {
	Install *bool  `yaml:"install,omitempty" help:"install the pinned cert-manager (false adopts an existing one)"`
	Version string `yaml:"version,omitempty" help:"cert-manager version"`
}

// Certificate modes.
const (
	CertModeCertManager = "cert-manager"
	CertModeSingle      = "single"
)

// Certificates chooses how BNK's components get their mTLS certificates:
// cert-manager (the default), or F5's "Single Certificate for BNK": no
// cert-manager at all, FLO mounting one kubernetes.io/tls Secret everywhere.
// The defaults are F5's procedure.
type Certificates struct {
	Mode       string `yaml:"mode,omitempty" help:"cert-manager (default) or single: one TLS Secret for every BNK component, no cert-manager"`
	Issuer     string `yaml:"issuer,omitempty" help:"single: self-signed (a CA generated in the cluster), ca (ca_cert_file and ca_key_file sign it) or provided (cert_file, key_file, ca_file)"`
	SecretName string `yaml:"secret_name,omitempty" help:"single: the Secret FLO mounts in every BNK namespace"`
	CACertFile string `yaml:"ca_cert_file,omitempty" help:"single, issuer ca: your CA certificate (PEM)"`
	CAKeyFile  string `yaml:"ca_key_file,omitempty" help:"single, issuer ca: your CA's private key (PEM); written into the cluster, never to Git"`
	CertFile   string `yaml:"cert_file,omitempty" help:"single, issuer provided: your certificate (PEM, tls.crt)"`
	KeyFile    string `yaml:"key_file,omitempty" help:"single, issuer provided: its private key (PEM, tls.key); written into the cluster, never to Git"`
	CAFile     string `yaml:"ca_file,omitempty" help:"single, issuer provided: the CA that signed it (PEM, ca.crt)"`
	CommonName string `yaml:"common_name,omitempty" help:"single: the certificate's common name"`
	// CACommonName names a self-signed CA; F5 gives it a subject distinct from the certificate's.
	CACommonName       string   `yaml:"ca_common_name,omitempty" help:"single, issuer self-signed: the generated CA's common name"`
	Organization       string   `yaml:"organization,omitempty" help:"single: subject O"`
	OrganizationalUnit string   `yaml:"organizational_unit,omitempty" help:"single: subject OU"`
	Country            string   `yaml:"country,omitempty" help:"single: subject C"`
	State              string   `yaml:"state,omitempty" help:"single: subject ST"`
	Locality           string   `yaml:"locality,omitempty" help:"single: subject L"`
	ExtraDNSNames      []string `yaml:"extra_dns_names,omitempty" help:"single: DNS names added to F5's list"`
	IPAddresses        []string `yaml:"ip_addresses,omitempty" help:"single: IP address SANs"`
	KeyType            string   `yaml:"key_type,omitempty" help:"single: rsa or ecdsa (P-256)"`
	KeyBits            int      `yaml:"key_bits,omitempty" help:"single: RSA key size"`
	ValidityDays       int      `yaml:"validity_days,omitempty" help:"single: lifetime of the certificate (and a generated CA), in days"`
	RenewBeforeDays    int      `yaml:"renew_before_days,omitempty" help:"single: a sync reissues the certificate this many days before it expires"`
}

// Registry says where ROKS pulls BNK images from.
type Registry struct {
	Source string `yaml:"source" help:"where ROKS pulls BNK images from: far or mirror"`
	// FARHost is FAR's registry host.
	FARHost string `yaml:"far_host,omitempty" help:"FAR registry host"`
	Mirror  Mirror `yaml:"mirror,omitempty"`
}

// Mirror is a replicated private registry (Harbor, Artifactory, registry:2, ICR).
type Mirror struct {
	Host        string `yaml:"host,omitempty" help:"mirror registry host[:port]"`
	Prefix      string `yaml:"prefix,omitempty" help:"repository prefix under the mirror host"`
	Username    string `yaml:"username,omitempty" help:"mirror registry username"`
	PasswordEnv string `yaml:"password_env,omitempty" help:"environment variable holding the mirror password"`
	CAFile      string `yaml:"ca_file,omitempty" help:"PEM file with the mirror registry CA"`
}

// COS locates the FAR auth tarball and the subscription JWT. LocalFARAuthFile /
// LocalJWTFile bypass COS for operators who hold the files directly.
type COS struct {
	Instance         string `yaml:"instance,omitempty" help:"COS instance: name, GUID or CRN"`
	Bucket           string `yaml:"bucket,omitempty" help:"COS bucket: name or CRN"`
	Region           string `yaml:"region,omitempty" help:"COS region: where a new bucket is created and buckets are listed from (an existing bucket's own region is found from its location)"`
	FARAuthObject    string `yaml:"far_auth_object,omitempty" help:"object key of the FAR auth tarball"`
	JWTObject        string `yaml:"jwt_object,omitempty" help:"object key of the subscription JWT"`
	LocalFARAuthFile string `yaml:"local_far_auth_file,omitempty" help:"local FAR auth tarball, instead of COS"`
	LocalJWTFile     string `yaml:"local_jwt_file,omitempty" help:"local subscription JWT file, instead of COS"`
}

// FLP is the F5 License Proxy for disconnected mode. Either `flp up` builds a VSI
// (VSI block) or the operator points at an existing one (External block).
type FLP struct {
	VSI      FLPVSI      `yaml:"vsi,omitempty"`
	External FLPExternal `yaml:"external,omitempty"`
}

// FLPVSI parameterises `flp up`.
type FLPVSI struct {
	Zone         string   `yaml:"zone,omitempty" help:"zone for the license proxy VSI (default <region>-1)"`
	VPC          string   `yaml:"vpc,omitempty" help:"existing VPC name or id; empty creates one"` // existing VPC name/id; empty creates one
	CIDR         string   `yaml:"cidr,omitempty" help:"address prefix for the license proxy subnet (a /24 to /28)"`
	Profile      string   `yaml:"profile,omitempty" help:"license proxy VSI profile"`
	AllowedCIDRs []string `yaml:"allowed_cidrs,omitempty" help:"CIDRs allowed to reach the proxy on :8443 (comma-separated)"` // may reach :8443
	SSHKey       string   `yaml:"ssh_key,omitempty" help:"existing VPC SSH key name for the license proxy"`                   // existing VPC SSH key name
	FloatingIP   *bool    `yaml:"floating_ip,omitempty" help:"give the license proxy VSI a floating IP"`
}

// FLPExternal points at an FLP this tool did not build.
type FLPExternal struct {
	URL        string `yaml:"url,omitempty" help:"URL of an existing license proxy (https://<ip>:8443)"`
	RootCAFile string `yaml:"root_ca_file,omitempty" help:"PEM file with the existing license proxy's root CA"`
}

// ArgoCD is the customer's existing, external Argo CD.
type ArgoCD struct {
	Server      string `yaml:"server" help:"https URL of the Argo CD"`
	TokenEnv    string `yaml:"token_env,omitempty" help:"environment variable holding the Argo CD API token"`
	Insecure    bool   `yaml:"insecure,omitempty" help:"skip TLS verification of the Argo CD"`
	CAFile      string `yaml:"ca_file,omitempty" help:"PEM file with the Argo CD CA"`
	Project     string `yaml:"project,omitempty" help:"Argo CD project"`
	Application string `yaml:"application,omitempty" help:"Argo CD Application name"`
	// ClusterEndpoint selects the ROKS API endpoint Argo CD dials: private (over the
	// transit gateway, default) or public.
	ClusterEndpoint string `yaml:"cluster_endpoint,omitempty" help:"ROKS endpoint Argo CD dials: private or public"`
}

// Git is the customer repo Argo CD syncs from.
type Git struct {
	URL        string `yaml:"url" help:"Git repo Argo CD syncs from"`
	Branch     string `yaml:"branch,omitempty" help:"Git branch"`
	Path       string `yaml:"path,omitempty" help:"path inside the Git repo"`
	Username   string `yaml:"username,omitempty" help:"Git username for https"`
	TokenEnv   string `yaml:"token_env,omitempty" help:"environment variable holding the Git token"`
	SSHKeyFile string `yaml:"ssh_key_file,omitempty" help:"SSH private key file for a git@ URL"`
	// KnownHostsFile holds SSH host keys for a Git server other than
	// github.com, gitlab.com and bitbucket.org (whose verified keys are built
	// in). It is also given to Argo CD so its clone verifies the same host.
	KnownHostsFile string `yaml:"known_hosts_file,omitempty" help:"known_hosts file for a Git host other than github.com, gitlab.com or bitbucket.org"`
	AuthorName     string `yaml:"author_name,omitempty" help:"Git commit author name"`
	AuthorEmail    string `yaml:"author_email,omitempty" help:"Git commit author email"`
}

// Check pins the check image.
type Check struct {
	Image string `yaml:"image,omitempty" help:"check image (default: this build's)"`
}

// TestHub parameterises `argocd up`, the test Argo CD VSI.
type TestHub struct {
	Region       string `yaml:"region,omitempty" help:"region for the test Argo CD VSI (default ibmcloud.region)"`
	Zone         string `yaml:"zone,omitempty" help:"zone for the test Argo CD VSI (default <region>-1)"`
	CIDR         string `yaml:"cidr,omitempty" help:"address prefix for the test Argo CD subnet (a /24 to /28)"`
	Profile      string `yaml:"profile,omitempty" help:"test Argo CD VSI profile"`
	Version      string `yaml:"version,omitempty" help:"Argo CD version on the test hub"`
	AllowedCIDR  string `yaml:"allowed_cidr,omitempty" help:"CIDR allowed to reach the test Argo CD"`
	SSHKey       string `yaml:"ssh_key,omitempty" help:"existing VPC SSH key name for the test hub"`
	NodePort     int    `yaml:"node_port,omitempty" help:"NodePort the test Argo CD listens on"`
	K3sChannel   string `yaml:"k3s_channel,omitempty" help:"k3s release channel"`
	AttachToTGW  *bool  `yaml:"attach_to_tgw,omitempty" help:"attach the test hub VPC to the transit gateway"`
	ResourceName string `yaml:"name,omitempty" help:"name of the test hub VSI and its resources"`
}

// Forge is where `forge register` and `forge unregister` find BNK Forge. The
// password is never stored: it comes from BNK_FORGE_PASSWORD or a prompt. No
// field has a default written to config.yaml; an empty endpoint means public
// and an empty project means the cluster name.
type Forge struct {
	URL      string `yaml:"url,omitempty" help:"BNK Forge server URL (default: BNK_FORGE_URL)"`
	Project  string `yaml:"project,omitempty" help:"BNK Forge project the cluster is registered in (default: the cluster name)"`
	Username string `yaml:"username,omitempty" help:"BNK Forge login username (default: BNK_FORGE_USER)"`
	Endpoint string `yaml:"endpoint,omitempty" help:"ROKS endpoint in the kubeconfig given to BNK Forge: public or private (default public)"`
	Insecure bool   `yaml:"insecure,omitempty" help:"skip TLS verification of BNK Forge (the password and token are then sent unauthenticated; prefer ca_file)"`
	CAFile   string `yaml:"ca_file,omitempty" help:"PEM file with the CA BNK Forge's certificate must chain to"`
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
	// InstalledLayout is the namespace layout and certificate mode install
	// installed BNK with; uninstall records "none". install refuses a different one
	// while it is set (switching either in place destroys a running install).
	InstalledLayout string `yaml:"installed_layout,omitempty"`
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
	if b.Certificates.Mode == "" {
		b.Certificates.Mode = CertModeCertManager
	}
	if b.Certificates.Mode == CertModeSingle {
		ct := &b.Certificates
		for _, d := range []struct {
			dst *string
			def string
		}{
			{&ct.Issuer, "self-signed"}, {&ct.SecretName, "bnk-single-cert"},
			{&ct.CommonName, "f5net"}, {&ct.CACommonName, "f5net-ca"},
			{&ct.Organization, "F5 Networks"}, {&ct.OrganizationalUnit, "PD"},
			{&ct.Country, "US"}, {&ct.State, "Washington"}, {&ct.Locality, "Seattle"}, {&ct.KeyType, "rsa"},
		} {
			if *d.dst == "" {
				*d.dst = d.def
			}
		}
		if ct.KeyBits == 0 {
			ct.KeyBits = 4096
		}
		if ct.ValidityDays == 0 {
			ct.ValidityDays = 3650
		}
		if ct.RenewBeforeDays == 0 {
			ct.RenewBeforeDays = 30
		}
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
	return c.UsesCertManager() && (c.BNK.CertManager.Install == nil || *c.BNK.CertManager.Install)
}

// UsesCertManager reports whether BNK's certificates come from cert-manager
// (installed or adopted); false in single-certificate mode.
func (c *Config) UsesCertManager() bool { return c.BNK.Certificates.Mode != CertModeSingle }

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
	if ct := c.BNK.Certificates; ct.Mode != "" && ct.Mode != CertModeCertManager && ct.Mode != CertModeSingle {
		add("bnk.certificates.mode %q: must be %q or %q", ct.Mode, CertModeCertManager, CertModeSingle)
	} else if ct.Mode == CertModeSingle {
		switch ct.Issuer {
		case "", "self-signed":
		case "ca":
			if ct.CACertFile == "" || ct.CAKeyFile == "" {
				add("bnk.certificates.issuer ca needs ca_cert_file and ca_key_file")
			}
		case "provided":
			if ct.CertFile == "" || ct.KeyFile == "" || ct.CAFile == "" {
				add("bnk.certificates.issuer provided needs cert_file, key_file and ca_file")
			}
		default:
			add("bnk.certificates.issuer %q: must be self-signed, ca or provided", ct.Issuer)
		}
		if ct.KeyType != "" && ct.KeyType != "rsa" && ct.KeyType != "ecdsa" {
			add("bnk.certificates.key_type %q: must be rsa or ecdsa", ct.KeyType)
		}
		if ct.KeyBits != 0 && ct.KeyBits < 2048 {
			add("bnk.certificates.key_bits %d: at least 2048", ct.KeyBits)
		}
		if ct.ValidityDays < 0 || ct.RenewBeforeDays < 0 || (ct.ValidityDays > 0 && ct.RenewBeforeDays >= ct.ValidityDays) {
			add("bnk.certificates: renew_before_days must be less than validity_days")
		}
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
	c, err := w.LoadFile()
	if err != nil {
		return nil, err
	}
	c.Defaults(w.Name)
	return c, nil
}

// LoadFile reads config.yaml exactly as written, WITHOUT defaults, so a caller
// can apply overrides before the defaults fill what is still empty (defaults
// are the lowest precedence, and some depend on other keys).
func (w *Workspace) LoadFile() (*Config, error) {
	data, err := os.ReadFile(w.ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workspace %q has no config.yaml; run `roksbnkargoctl init -w %s`", w.Name, w.Name)
		}
		return nil, err
	}
	return Parse(data)
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
