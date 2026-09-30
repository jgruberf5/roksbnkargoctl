package cli

import (
	"bytes"
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

// noWorkspaceName stands in for the workspace name in the defaults that are
// derived from it (argocd.application, git.path, test_hub.name) and in resource
// names when a command runs without a workspace.
const noWorkspaceName = "roksbnkargoctl"

// errNoWorkspaceToSave is what save returns when the session has no workspace.
var errNoWorkspaceToSave = errors.New("no workspace: there is no config.yaml to save to (pass -w <name>)")

// session is one command's view of the configuration and the services it talks
// to. Clients are built lazily, so a command only needs the credentials it uses.
//
// There are two configs. cfg is the EFFECTIVE one every command reads: flags
// over ROKSBNKARGOCTL_* variables over config.yaml over defaults. file is what
// config.yaml holds (defaulted, as it always was saved). save writes file, with
// cfg's resolved state, so an override is never persisted; a command that
// means to change a saved setting says so with update, which changes both.
type session struct {
	ws   *config.Workspace // nil when running without a workspace
	name string            // the workspace name, or noWorkspaceName
	cfg  *config.Config    // effective
	p    printer

	file      *config.Config // config.yaml's settings + intended changes; nil without a workspace
	pinned    *config.Config // cfg's settings as built + intended changes, for save's guard
	overrides []config.Override

	ibmc *ibm.Client
	kc   *kube.Client
	farS string

	// lookup answers the on-the-fly IBM Cloud lookups; nil means s.IBM(). Tests
	// substitute a fake.
	lookup ibmLookup
	cosCRN string
	// ignoreBucketCRN stops a bucket CRN in cos.bucket from naming the
	// instance (for commands about the instance, not the bucket).
	ignoreBucketCRN bool
	rgID            string
	tgwID           string
}

// ibmLookup is the part of the IBM Cloud client the session resolves names with.
type ibmLookup interface {
	ResolveResourceGroup(ctx context.Context, nameOrID string) (id, name string, err error)
	ResolveTransitGateway(ctx context.Context, nameOrID string) (*ibm.TransitGateway, error)
	ListServiceInstances(ctx context.Context, serviceName string) ([]ibm.ServiceInstance, error)
	ListResourceGroups(ctx context.Context) ([]ibm.ResourceGroup, error)
}

// cosService is the CRN service segment of a COS instance.
const cosService = "cloud-object-storage"

// newCOSClient builds COS clients; tests point it at a fake.
var newCOSClient = cos.New

// newIBMLookup is the lookup a session uses when none is set: the IBM Cloud
// client. Tests that drive a command through the root substitute a fake.
var newIBMLookup = defaultIBMLookup

func defaultIBMLookup(s *session) (ibmLookup, error) { return s.IBM() }

// newSession opens the selected workspace, as every workspace command always
// has: no workspace is an error.
func newSession(cmd *cobra.Command) (*session, error) { return openSession(cmd, true) }

// newSessionOptional opens the selected workspace if there is one (-w,
// ROKSBNKARGOCTL_WORKSPACE, or the current workspace, unless --no-workspace),
// and otherwise starts from the defaults with no config.yaml: s.ws is nil,
// save returns an error, and settings come only from flags, ROKSBNKARGOCTL_*
// variables and defaults.
func newSessionOptional(cmd *cobra.Command) (*session, error) { return openSession(cmd, false) }

func openSession(cmd *cobra.Command, requireWorkspace bool) (*session, error) {
	ovs, err := commandOverrides(cmd)
	if err != nil {
		return nil, err
	}
	ws, err := selectWorkspace(requireWorkspace)
	if err != nil {
		return nil, err
	}
	raw, name := &config.Config{}, noWorkspaceName
	if ws != nil {
		if raw, err = ws.LoadFile(); err != nil {
			return nil, err
		}
		name = ws.Name
	}
	return buildSession(out(cmd), ws, name, raw, ovs)
}

// buildSession derives the file and effective configs from raw (config.yaml as
// written, or an empty config without a workspace).
func buildSession(p printer, ws *config.Workspace, name string, raw *config.Config, ovs []config.Override) (*session, error) {
	eff := raw.Clone()
	if err := config.Apply(eff, ovs); err != nil {
		return nil, err
	}
	eff.Defaults(name)
	s := &session{ws: ws, name: name, cfg: eff, p: p, pinned: eff.Clone(), overrides: ovs}
	if ws != nil {
		s.file = raw.Clone()
		s.file.Defaults(name)
	}
	return s, nil
}

// commandOverrides are the ROKSBNKARGOCTL_* variables, then the config flags
// the command line set (later wins, so a flag beats the environment).
func commandOverrides(cmd *cobra.Command) ([]config.Override, error) {
	env, err := config.FromEnv(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	fl, err := flagOverrides(cmd)
	if err != nil {
		return nil, err
	}
	return append(env, fl...), nil
}

// hasWorkspace reports whether the session has a workspace to read and save.
func (s *session) hasWorkspace() bool { return s.ws != nil }

// workspace returns the workspace, or an error naming what needs it.
func (s *session) workspace(what string) (*config.Workspace, error) {
	if s.ws == nil {
		return nil, fmt.Errorf("%s needs a workspace: pass -w <name> (or drop --no-workspace)", what)
	}
	return s.ws, nil
}

// update makes an intended change to saved settings: it is applied to the
// effective config (so the rest of the command sees it) and to config.yaml's
// settings (so save writes it). A setting an override still covers is saved,
// but the override keeps winning while it is set, and the operator is told.
func (s *session) update(fn func(*config.Config)) {
	fn(s.cfg)
	fn(s.pinned)
	if s.file == nil {
		return
	}
	before := s.file.Clone()
	fn(s.file)
	for _, path := range config.DiffKeys(before, s.file) {
		if src := s.overrideFor(path); src != "" {
			s.p.warn("%s is saved to config.yaml, but %s overrides it while it is set", path, src)
		}
	}
}

// save writes config.yaml: its own settings, the changes made through update,
// and the resolved state (machine state, not a setting). It refuses to write
// when a setting was changed on the effective config without update, because
// it cannot tell whether that change was meant to be saved.
func (s *session) save() error {
	if s.ws == nil {
		return errNoWorkspaceToSave
	}
	if !sameSettings(s.cfg, s.pinned) {
		changed := config.DiffKeys(s.cfg, s.pinned)
		return fmt.Errorf("not saving %s: setting(s) %s changed without session.update (a bug: say whether the change is meant to be saved)", s.ws.ConfigPath(), strings.Join(changed, ", "))
	}
	out := s.file.Clone()
	out.Resolved = s.cfg.Resolved
	return s.ws.Save(out)
}

// sameSettings compares two configs' settings, ignoring resolved state. It
// compares the YAML rendering, which covers keys that have no override too.
func sameSettings(a, b *config.Config) bool {
	aa, bb := *a, *b
	aa.Resolved, bb.Resolved = nil, nil
	ya, erra := config.Marshal(&aa)
	yb, errb := config.Marshal(&bb)
	return erra == nil && errb == nil && bytes.Equal(ya, yb)
}

// overrideSource names the override that sets path to a value config.yaml does
// not have ("" when the setting is the file's own, or there is no file).
func (s *session) overrideSource(path string) string {
	if s.file != nil {
		fv, _ := s.file.Get(path)
		ev, _ := s.cfg.Get(path)
		if config.SameValue(fv, ev) {
			return ""
		}
	}
	return s.overrideFor(path)
}

// overrideFor names the last (winning) override given for path, whatever its value.
func (s *session) overrideFor(path string) string {
	src := ""
	for _, o := range s.overrides {
		if o.Key.Path == path {
			src = o.Source
		}
	}
	return src
}

// overridden reports whether an override changed path from config.yaml's
// value. Without a workspace every overridden key counts.
func (s *session) overridden(path string) bool { return s.overrideSource(path) != "" }

// resolved is what `init` recorded. It needs a workspace, and refuses when an
// override points the command at a cluster, gateway or endpoint other than
// the one recorded: acting on the recorded one would be acting on the wrong
// thing.
func (s *session) resolved() (*config.Resolved, error) {
	if s.ws == nil {
		return nil, errors.New("this command needs a resolved workspace: pass -w <name> (created with `roksbnkargoctl init`)")
	}
	r := s.cfg.Resolved
	if r == nil || r.ClusterID == "" {
		return nil, fmt.Errorf("workspace %q is not resolved; run `roksbnkargoctl init -w %s --refresh`", s.ws.Name, s.ws.Name)
	}
	stale := func(path, want, recorded string) error {
		return fmt.Errorf("%s overrides %s to %q, but workspace %q was resolved for %s; unset it, or run `roksbnkargoctl init -w %s --refresh` with it set",
			s.overrideSource(path), path, want, s.ws.Name, recorded, s.ws.Name)
	}
	if c := s.cfg.Cluster; s.overridden("cluster") && c != r.ClusterID && c != r.ClusterName {
		return nil, stale("cluster", c, r.ClusterName)
	}
	if g := s.cfg.TransitGateway; s.overridden("transit_gateway") && g != r.TransitGatewayID && g != r.TransitGatewayName {
		return nil, stale("transit_gateway", g, r.TransitGatewayName)
	}
	if e := s.cfg.ArgoCD.ClusterEndpoint; s.overridden("argocd.cluster_endpoint") {
		want := r.PrivateEndpoint
		if e == "public" {
			want = r.PublicEndpoint
		}
		if want != r.ArgoCDClusterServer {
			return nil, stale("argocd.cluster_endpoint", e, r.ArgoCDClusterServer)
		}
	}
	return r, nil
}

// IBM returns the IBM Cloud client for the configured region. It needs only the
// API key, so it works without a workspace.
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

func (s *session) ibmLookup() (ibmLookup, error) {
	if s.lookup != nil {
		return s.lookup, nil
	}
	return newIBMLookup(s)
}

// COSInstanceCRN is the supply-chain COS instance's CRN: the one `init`
// recorded, unless an override names another instance (or cos.bucket is a
// bucket CRN, which names its own) or there is no record, in which case it is
// looked up (and cached for the session).
func (s *session) COSInstanceCRN(ctx context.Context) (string, error) {
	if s.cosCRN != "" {
		return s.cosCRN, nil
	}
	_, _, bucketCRN := cos.ParseBucketCRN(s.cfg.COS.Bucket)
	bucketCRN = bucketCRN && !s.ignoreBucketCRN
	if r := s.cfg.Resolved; r != nil && r.COSInstanceCRN != "" && !s.overridden("cos.instance") && !bucketCRN {
		s.cosCRN = r.COSInstanceCRN
		return s.cosCRN, nil
	}
	si, err := s.resolveCOSInstance(ctx)
	if err != nil {
		return "", err
	}
	s.cosCRN = si.CRN
	return s.cosCRN, nil
}

// resolveCOSInstance looks the COS instance up, never from the record:
// cos.instance is its name, GUID or CRN. When cos.bucket is a bucket CRN, the
// instance is the one that CRN names, and a cos.instance override naming a
// different one is an error rather than a silent pick.
func (s *session) resolveCOSInstance(ctx context.Context) (*ibm.ServiceInstance, error) {
	ref := s.cfg.COS.Instance
	guid, bucket, bucketCRN := cos.ParseBucketCRN(s.cfg.COS.Bucket)
	bucketCRN = bucketCRN && !s.ignoreBucketCRN
	if bucketCRN {
		ref = guid
	}
	if ref == "" {
		return nil, fmt.Errorf("cos.instance is not set (config.yaml, %s, or --instance)", config.EnvName("cos.instance"))
	}
	l, err := s.ibmLookup()
	if err != nil {
		return nil, err
	}
	all, err := l.ListServiceInstances(ctx, cosService)
	if err != nil {
		return nil, err
	}
	si, err := ibm.MatchServiceInstance(all, ref)
	if err != nil {
		if bucketCRN {
			return nil, fmt.Errorf("COS instance %s (from the CRN of bucket %s): %w", guid, bucket, err)
		}
		return nil, fmt.Errorf("COS instance %q: %w", ref, err)
	}
	if bucketCRN && s.overridden("cos.instance") && s.cfg.COS.Instance != "" {
		named, err := ibm.MatchServiceInstance(all, s.cfg.COS.Instance)
		if err != nil {
			return nil, fmt.Errorf("COS instance %q (%s): %w", s.cfg.COS.Instance, s.overrideFor("cos.instance"), err)
		}
		if named.GUID != si.GUID {
			return nil, fmt.Errorf("%s names COS instance %s (%s), but bucket %s belongs to %s (%s); drop one",
				s.overrideFor("cos.instance"), named.Name, named.GUID, bucket, si.Name, si.GUID)
		}
	}
	return si, nil
}

// cosBucketName is cos.bucket, or the name inside it when it is a bucket CRN.
func (s *session) cosBucketName() string {
	if _, name, ok := cos.ParseBucketCRN(s.cfg.COS.Bucket); ok {
		return name
	}
	return strings.TrimSpace(s.cfg.COS.Bucket)
}

// cosBucket returns cos.bucket's name and a client in the bucket's region,
// which is found from the bucket's location: a request to another region's
// endpoint fails with an error that does not say the region is the problem.
//
// strict makes a bucket that cannot be located an error. Otherwise the client
// falls back to cos.region: reading the FAR key and JWT worked before
// discovery existed, with a key that may be allowed to read those objects but
// not to list the instance's buckets, and must keep working.
func (s *session) cosBucket(ctx context.Context, strict bool) (*cos.Client, string, error) {
	name := s.cosBucketName()
	if name == "" {
		return nil, "", fmt.Errorf("cos.bucket is not set (config.yaml, %s, or --bucket)", config.EnvName("cos.bucket"))
	}
	cc, err := s.COS(ctx)
	if err != nil {
		return nil, "", err
	}
	b, err := cc.FindBucket(ctx, name)
	if err != nil {
		if strict {
			return nil, "", err
		}
		if flagVerbose {
			s.p.info("locating bucket %s: %v; using cos.region %s", name, err, cc.Region())
		}
		return cc, name, nil
	}
	rc, err := cc.InRegion(b.Region)
	if err != nil {
		return nil, "", err
	}
	return rc, name, nil
}

// ResourceGroupID is the resource group new resources go into: the cluster's
// group `init` recorded, unless ibmcloud.resource_group is overridden or there
// is no record, in which case ibmcloud.resource_group is looked up.
func (s *session) ResourceGroupID(ctx context.Context) (string, error) {
	if s.rgID != "" {
		return s.rgID, nil
	}
	if r := s.cfg.Resolved; r != nil && r.ResourceGroupID != "" && !s.overridden("ibmcloud.resource_group") {
		s.rgID = r.ResourceGroupID
		return s.rgID, nil
	}
	l, err := s.ibmLookup()
	if err != nil {
		return "", err
	}
	id, _, err := l.ResolveResourceGroup(ctx, s.cfg.IBMCloud.ResourceGroup)
	if err != nil {
		return "", fmt.Errorf("resource group %q: %w", s.cfg.IBMCloud.ResourceGroup, err)
	}
	s.rgID = id
	return id, nil
}

// TransitGatewayID is the transit gateway's id: the recorded one unless
// transit_gateway is overridden or there is no record, else looked up by name
// or id.
func (s *session) TransitGatewayID(ctx context.Context) (string, error) {
	if s.tgwID != "" {
		return s.tgwID, nil
	}
	if r := s.cfg.Resolved; r != nil && r.TransitGatewayID != "" && !s.overridden("transit_gateway") {
		s.tgwID = r.TransitGatewayID
		return s.tgwID, nil
	}
	if s.cfg.TransitGateway == "" {
		return "", fmt.Errorf("transit_gateway is not set (config.yaml, %s, or a flag)", config.EnvName("transit_gateway"))
	}
	l, err := s.ibmLookup()
	if err != nil {
		return "", err
	}
	gw, err := l.ResolveTransitGateway(ctx, s.cfg.TransitGateway)
	if err != nil {
		return "", fmt.Errorf("transit gateway %q: %w", s.cfg.TransitGateway, err)
	}
	s.tgwID = gw.ID
	return gw.ID, nil
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

// COS returns the supply-chain COS client in cos.region. It works without a
// workspace: the instance CRN is looked up from cos.instance when none is
// recorded. For a bucket's objects use cosBucket, which finds its region.
func (s *session) COS(ctx context.Context) (*cos.Client, error) {
	key, err := s.cfg.APIKey()
	if err != nil {
		return nil, err
	}
	crn, err := s.COSInstanceCRN(ctx)
	if err != nil {
		return nil, err
	}
	return newCOSClient(ctx, key, crn, s.cfg.COS.Region)
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
		var bucket string
		if cc, bucket, err = s.cosBucket(ctx, false); err == nil {
			tgz, err = cc.GetObject(ctx, bucket, s.cfg.COS.FARAuthObject)
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
		var bucket string
		if cc, bucket, err = s.cosBucket(ctx, false); err == nil {
			b, err = cc.GetObject(ctx, bucket, s.cfg.COS.JWTObject)
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
