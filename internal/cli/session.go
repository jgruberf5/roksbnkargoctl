package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/cos"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"github.com/jgruberf5/roksbnkargoctl/internal/ibm"
	"github.com/jgruberf5/roksbnkargoctl/internal/kube"
)

// session is one command's view of a workspace and the services it talks to.
// Clients are built lazily, so a command only needs the credentials it uses.
type session struct {
	ws  *config.Workspace
	cfg *config.Config
	p   printer

	ibmc *ibm.Client
	kc   *kube.Client
	farS string
}

func newSession(cmd *cobra.Command) (*session, error) {
	ws, c, err := openWorkspace()
	if err != nil {
		return nil, err
	}
	return &session{ws: ws, cfg: c, p: out(cmd)}, nil
}

func (s *session) save() error { return s.ws.Save(s.cfg) }

func (s *session) resolved() (*config.Resolved, error) {
	if s.cfg.Resolved == nil || s.cfg.Resolved.ClusterID == "" {
		return nil, fmt.Errorf("workspace %q is not resolved; run `roksbnkargoctl init -w %s --refresh`", s.ws.Name, s.ws.Name)
	}
	return s.cfg.Resolved, nil
}

// IBM returns the IBM Cloud client for the workspace region.
func (s *session) IBM() (*ibm.Client, error) {
	if s.ibmc != nil {
		return s.ibmc, nil
	}
	key, err := s.cfg.APIKey()
	if err != nil {
		return nil, err
	}
	c, err := ibm.New(key, s.cfg.IBMCloud.Region)
	if err != nil {
		return nil, err
	}
	s.ibmc = c
	return c, nil
}

// Kube returns an admin client for the ROKS cluster, over its public endpoint
// unless the operator host only has private access (ROKSBNKARGOCTL_PRIVATE_ENDPOINT=1).
func (s *session) Kube(ctx context.Context) (*kube.Client, error) {
	if s.kc != nil {
		return s.kc, nil
	}
	c, err := s.IBM()
	if err != nil {
		return nil, err
	}
	private := os.Getenv("ROKSBNKARGOCTL_PRIVATE_ENDPOINT") == "1"
	kcfg, err := c.FetchAdminKubeconfig(ctx, s.cfg.Cluster, private)
	if err != nil {
		return nil, fmt.Errorf("admin kubeconfig for cluster %s: %w", s.cfg.Cluster, err)
	}
	k, err := kube.FromKubeconfig(kcfg)
	if err != nil {
		return nil, err
	}
	s.kc = k
	return k, nil
}

// COS returns the supply-chain COS client.
func (s *session) COS(ctx context.Context) (*cos.Client, error) {
	c, err := s.IBM()
	if err != nil {
		return nil, err
	}
	crn := ""
	if s.cfg.Resolved != nil {
		crn = s.cfg.Resolved.COSInstanceCRN
	}
	if crn == "" {
		si, err := c.FindServiceInstance(ctx, s.cfg.COS.Instance, "cloud-object-storage", "")
		if err != nil {
			return nil, fmt.Errorf("COS instance %q: %w", s.cfg.COS.Instance, err)
		}
		crn = si.CRN
	}
	key, _ := s.cfg.APIKey()
	return cos.New(ctx, key, crn, s.cfg.COS.Region)
}

// FARServiceAccount returns the FAR password (the service-account key inside
// F5's auth tarball), from a local file or COS.
func (s *session) FARServiceAccount(ctx context.Context) (string, error) {
	if s.farS != "" {
		return s.farS, nil
	}
	var tgz []byte
	var err error
	if f := s.cfg.COS.LocalFARAuthFile; f != "" {
		tgz, err = os.ReadFile(f)
	} else {
		var cc *cos.Client
		if cc, err = s.COS(ctx); err == nil {
			tgz, err = cc.GetObject(ctx, s.cfg.COS.Bucket, s.cfg.COS.FARAuthObject)
		}
	}
	if err != nil {
		return "", fmt.Errorf("FAR auth tarball: %w", err)
	}
	sa, err := far.ServiceAccountFromTarball(tgz)
	if err != nil {
		return "", err
	}
	s.farS = sa
	return sa, nil
}

// JWT returns the subscription JWT from a local file or COS.
func (s *session) JWT(ctx context.Context) (string, error) {
	var b []byte
	var err error
	if f := s.cfg.COS.LocalJWTFile; f != "" {
		b, err = os.ReadFile(f)
	} else {
		var cc *cos.Client
		if cc, err = s.COS(ctx); err == nil {
			b, err = cc.GetObject(ctx, s.cfg.COS.Bucket, s.cfg.COS.JWTObject)
		}
	}
	if err != nil {
		return "", fmt.Errorf("subscription JWT: %w", err)
	}
	jwt := strings.TrimSpace(string(b))
	if strings.Count(jwt, ".") != 2 {
		return "", errors.New("subscription JWT: the object is not a JWT (expected three dot-separated parts)")
	}
	return jwt, nil
}

// MirrorPassword returns the mirror password from its environment variable ("" if unset).
func (s *session) MirrorPassword() string {
	return strings.TrimSpace(os.Getenv(s.cfg.Registry.Mirror.PasswordEnv))
}

// MirrorCA returns the mirror's CA PEM when one is configured — whatever
// registry.source says: `registry replicate` fills the mirror while the install
// still pulls from FAR.
func (s *session) MirrorCA() (string, error) {
	if s.cfg.Registry.Mirror.Host == "" || s.cfg.Registry.Mirror.CAFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(s.cfg.Registry.Mirror.CAFile)
	if err != nil {
		return "", fmt.Errorf("registry.mirror.ca_file: %w", err)
	}
	return string(b), nil
}

// Puller returns an OCI puller with FAR and mirror credentials, each scoped to
// its own host.
func (s *session) Puller(ctx context.Context, needFAR bool) (*far.Puller, error) {
	var creds []far.Credential
	if needFAR {
		sa, err := s.FARServiceAccount(ctx)
		if err != nil {
			return nil, err
		}
		creds = append(creds, far.Credential{Host: s.cfg.Registry.FARHost, Username: far.FARUsername, Password: sa})
	}
	creds = append(creds, mirrorCredentials(s.cfg, s.MirrorPassword())...)
	ca, err := s.MirrorCA()
	if err != nil {
		return nil, err
	}
	return far.NewPuller(creds, []byte(ca))
}

// mirrorCredentials is the mirror login, scoped to its host, whenever a mirror
// with a username is configured. It must not depend on registry.source: the
// mirror is replicated BEFORE the install switches to it, and gating on the
// source sent every push anonymously ("Authentication is required", found live
// against Artifactory).
func mirrorCredentials(c *config.Config, password string) []far.Credential {
	if c.Registry.Mirror.Host == "" || c.Registry.Mirror.Username == "" {
		return nil
	}
	return []far.Credential{{Host: mirrorRegistryHost(c), Username: c.Registry.Mirror.Username, Password: password}}
}

// mirrorRegistryHost is the mirror's host[:port] (kubelet matches pull secrets
// by host, not by the repo prefix path).
func mirrorRegistryHost(c *config.Config) string {
	return strings.SplitN(c.Registry.Mirror.Host, "/", 2)[0]
}

// ArgoCD returns the Argo CD API client.
func (s *session) ArgoCD() (*argocd.Client, error) {
	tok, err := config.Env(s.cfg.ArgoCD.TokenEnv, "Argo CD API token")
	if err != nil {
		return nil, err
	}
	var ca []byte
	if s.cfg.ArgoCD.CAFile != "" {
		if ca, err = os.ReadFile(s.cfg.ArgoCD.CAFile); err != nil {
			return nil, fmt.Errorf("argocd.ca_file: %w", err)
		}
	}
	c := argocd.New(s.cfg.ArgoCD.Server, tok, s.cfg.ArgoCD.Insecure, ca)
	c.Project = s.cfg.ArgoCD.Project
	return c, nil
}
