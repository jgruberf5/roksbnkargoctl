// Package render turns a workspace config into every Kubernetes object the BNK
// install consists of, split three ways:
//
//   - Git: what Argo CD syncs. No secret values, ever.
//   - Direct: what `install` writes straight into ROKS — Secrets (including any
//     a chart renders), and the check namespace, ServiceAccount and RBAC, which
//     must outlive the Application for the PostDelete check to run.
//   - Application: the Argo CD Application itself.
//
// Rendering is deterministic for a given config and run id, so re-publishing an
// unchanged install commits nothing.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// Waves (DESIGN.md "Sync order").
const (
	WaveNamespaces   = -20
	WaveCATrust      = -19
	WavePreInstall   = -18
	WaveCertManager  = -12
	WaveIssuers      = -10 // -10 self-signed, -9 ext-ca, -8 sample-issuer
	WaveNetwork      = -6
	WaveSweep        = -6
	WaveFLO          = -5
	WaveCNEManifest  = -4
	WaveCNEInstance  = -2
	WaveLicenseCheck = 0
)

// Secrets carries the values that go into Direct Secrets. Empty fields produce
// no Secret.
type Secrets struct {
	PullHost     string // registry host kubelet matches (no path)
	PullUsername string
	PullPassword string
	JWT          string
	FLPCAPEM     string
}

// Inputs is everything a render needs. The caller does the network I/O.
type Inputs struct {
	Config            *config.Config
	Workspace         string
	Manifest          *far.Manifest
	CertManagerChart  []byte // nil when cert-manager is not installed by us
	FLOChart          []byte
	KubeVersion       string
	CheckImage        string
	NodeResolverImage string // required when a mirror CA is set
	MirrorCAPEM       string
	FLPURL            string // https://<ip>:8443 in disconnected mode
	RunID             string
	Secrets           Secrets
}

// Output is the three-way split.
type Output struct {
	Git         []Object
	Direct      []Object
	Application Object
}

// Render builds every object.
func Render(in Inputs) (*Output, error) {
	c := in.Config
	if c == nil || in.Manifest == nil || len(in.FLOChart) == 0 {
		return nil, errors.New("render: config, BNK manifest and FLO chart are required")
	}
	if c.Resolved == nil {
		return nil, errors.New("render: workspace is not resolved; run `roksbnkargoctl init` first")
	}
	if in.Manifest.Version != c.BNK.Version {
		return nil, fmt.Errorf("render: manifest is %s but the workspace installs %s", in.Manifest.Version, c.BNK.Version)
	}
	if c.BNK.Mode == config.ModeDisconnected && in.FLPURL == "" {
		return nil, errors.New("render: disconnected mode needs the FLP URL (run `roksbnkargoctl flp up` or set flp.external.url)")
	}
	if in.MirrorCAPEM != "" && in.NodeResolverImage == "" {
		return nil, errors.New("render: a private mirror CA needs the node-cached image (openshift-dns/node-resolver)")
	}
	if in.RunID == "" {
		in.RunID = "0"
	}
	pullSecret := ""
	if in.Secrets.PullPassword != "" {
		pullSecret = c.PullSecretName()
	}
	bnkNS, utilsNS := c.BNK.Namespace, c.BNK.UtilsNamespace

	out := &Output{}
	add := func(objs ...Object) { out.Git = append(out.Git, objs...) }

	// Namespaces.
	nsNames := []string{bnkNS}
	if utilsNS != bnkNS {
		nsNames = append(nsNames, utilsNS)
	}
	if c.CertManagerInstall() {
		nsNames = append(nsNames, "cert-manager")
	}
	for _, n := range nsNames {
		add(ns(n, WaveNamespaces, map[string]string{LabelManagedBy: ManagedByValue}))
	}

	// Check machinery.
	cp := checkParams{
		Image: in.CheckImage, RunID: in.RunID,
		BNKNamespace: bnkNS, UtilsNamespace: utilsNS,
		TMMReplicas: c.BNK.TMMReplicas, StorageClass: c.BNK.StorageClass,
		Targets: probeTargets(c, in.FLPURL),
		AppName: c.ArgoCD.Application,
	}
	if pullSecret != "" && c.Registry.Source == config.SourceMirror {
		cp.PullSecret = pullSecret // the check image comes from the mirror too
	}
	if pullSecret != "" {
		for _, n := range pullSecretNamespaces(c) {
			cp.RequireSecrets = append(cp.RequireSecrets, n+"/"+pullSecret)
		}
	}
	cp.RequireSecrets = append(cp.RequireSecrets, CheckNamespace+"/"+LicenseJWTSecret)
	if c.BNK.Mode == config.ModeDisconnected {
		cp.RequireSecrets = append(cp.RequireSecrets, utilsNS+"/"+FLPRootCASecret)
	}
	if in.MirrorCAPEM != "" {
		add(caTrust(c.Registry.Mirror.Host, in.MirrorCAPEM, in.NodeResolverImage, WaveCATrust)...)
	}
	add(cp.nodeProbe(WavePreInstall))
	add(cp.hookJob("pre-install", "Sync", WavePreInstall, cp.preInstallArgs(), 20*60))

	// cert-manager.
	if c.CertManagerInstall() {
		if len(in.CertManagerChart) == 0 {
			return nil, errors.New("render: cert-manager chart missing")
		}
		objs, err := RenderChart(ChartRender{Chart: in.CertManagerChart, Release: "cert-manager", Namespace: "cert-manager",
			Values: certManagerValues(c, pullSecret), KubeVersion: in.KubeVersion, APIVersions: OpenShiftAPIVersions})
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			o.SetWave(WaveCertManager)
		}
		add(objs...)
	}
	add(certChain(WaveIssuers)...)

	// Networking + FLO.
	add(nad(bnkNS, c.BNK.NADAddress, WaveNetwork))
	add(sccBinding(bnkNS, "flo-f5-lifecycle-operator", WaveNetwork))
	add(cp.sweep(WaveSweep))
	floObjs, err := RenderChart(ChartRender{Chart: in.FLOChart, Release: "flo", Namespace: bnkNS,
		Values: floValues(c, pullSecret), KubeVersion: in.KubeVersion, APIVersions: OpenShiftAPIVersions})
	if err != nil {
		return nil, err
	}
	for _, o := range floObjs {
		o.SetWave(WaveFLO)
	}
	add(floObjs...)
	add(cneManifest(in.Manifest, WaveCNEManifest))
	add(cneInstance(CNEInstanceParams{
		Namespace: bnkNS, ImageHost: c.ImageHost(), PullSecret: pullSecret,
		Region: c.IBMCloud.Region, VPCName: c.Resolved.VPCName, TrustedProfileID: c.Resolved.TrustedProfileID,
		TMMReplicas: c.BNK.TMMReplicas, StorageClass: c.BNK.StorageClass, Version: c.BNK.Version,
	}, WaveCNEInstance))

	// License + lifecycle checks.
	cp.LicenseArgs = licenseArgs(c, in.FLPURL)
	add(cp.hookJob("license", "Sync", WaveLicenseCheck, append([]string{"--timeout=35m"}, cp.LicenseArgs[1:]...), 40*60))
	add(cp.hookJob("post-install", "PostSync", 0, []string{"--timeout=10m", "--bnk-namespace=" + bnkNS,
		"--utils-namespace=" + utilsNS, fmt.Sprintf("--tmm-replicas=%d", c.BNK.TMMReplicas)}, 15*60))
	add(cp.hookJob("pre-uninstall", "PreDelete", 0, []string{"--timeout=15m", "--bnk-namespace=" + bnkNS,
		"--utils-namespace=" + utilsNS}, 20*60))
	postArgs := []string{"--timeout=15m", "--bnk-namespace=" + bnkNS, "--utils-namespace=" + utilsNS}
	if c.CertManagerInstall() {
		postArgs = append(postArgs, "--delete-namespace=cert-manager")
	}
	add(cp.hookJob("post-uninstall", "PostDelete", 0, postArgs, 20*60))

	// Split: every Secret goes Direct, whatever rendered it.
	var git []Object
	for _, o := range out.Git {
		if o.Kind() == "Secret" {
			out.Direct = append(out.Direct, o)
			continue
		}
		stamp(o)
		git = append(git, o)
	}
	out.Git = git

	// Direct objects.
	out.Direct = append(checkRBAC(), out.Direct...)
	out.Direct = append(out.Direct, directNamespaces(nsNames)...)
	out.Direct = append(out.Direct, secrets(c, in.Secrets, pullSecret)...)
	for _, o := range out.Direct {
		// Direct objects carry no Argo CD wave: nothing syncs them.
		delete(o.Annotations(), AnnoWave)
		o.Labels()[LabelManagedBy] = ManagedByValue
		o.PruneEmptyMeta()
	}
	for _, o := range out.Git {
		o.PruneEmptyMeta()
	}
	if err := checkNoSecretsInGit(out.Git, in.Secrets); err != nil {
		return nil, err
	}
	Sort(out.Git)
	out.Application = application(c, in.Workspace)
	return out, nil
}

// stamp marks every Git object as ours and keeps CRDs across uninstall: deleting
// a CRD deletes every CR with it, and F5 CRs whose finalizer's controller is gone
// hang their namespace (roksbnkctl keeps F5 CRDs for the same reason).
func stamp(o Object) {
	if o.Kind() == "CustomResourceDefinition" {
		addSyncOption(o, "Delete=false")
	}
}

func addSyncOption(o Object, opt string) {
	cur := o.Annotation(AnnoSyncOptions)
	for _, x := range strings.Split(cur, ",") {
		if strings.TrimSpace(x) == opt {
			return
		}
	}
	if cur == "" {
		o.Annotations()[AnnoSyncOptions] = opt
	} else {
		o.Annotations()[AnnoSyncOptions] = cur + "," + opt
	}
}

// directNamespaces are the BNK namespaces again, created out of band before the
// Direct Secrets that live in them; Argo CD then adopts them.
func directNamespaces(names []string) []Object {
	var out []Object
	for _, n := range names {
		out = append(out, Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": n}})
	}
	return out
}

func pullSecretNamespaces(c *config.Config) []string {
	out := []string{c.BNK.Namespace}
	if c.BNK.UtilsNamespace != c.BNK.Namespace {
		out = append(out, c.BNK.UtilsNamespace)
	}
	if c.CertManagerInstall() && c.Registry.Source == config.SourceMirror {
		out = append(out, "cert-manager")
	}
	if c.Registry.Source == config.SourceMirror {
		out = append(out, CheckNamespace)
	}
	return out
}

func secrets(c *config.Config, s Secrets, pullSecret string) []Object {
	var out []Object
	if pullSecret != "" {
		dcj, _ := far.DockerConfigJSON(s.PullHost, s.PullUsername, s.PullPassword)
		for _, n := range pullSecretNamespaces(c) {
			out = append(out, Object{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
				"metadata":   map[string]any{"name": pullSecret, "namespace": n},
				"stringData": map[string]any{".dockerconfigjson": string(dcj)}})
		}
	}
	if s.JWT != "" {
		out = append(out, Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata":   map[string]any{"name": LicenseJWTSecret, "namespace": CheckNamespace},
			"stringData": map[string]any{"jwt": s.JWT}})
	}
	if s.FLPCAPEM != "" {
		out = append(out, Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata":   map[string]any{"name": FLPRootCASecret, "namespace": c.BNK.UtilsNamespace},
			"stringData": map[string]any{FLPRootCAKey: s.FLPCAPEM}})
	}
	return out
}

// checkNoSecretsInGit is the last line of defence for DESIGN.md's "no secret
// values in Git": it fails the render if any secret value appears anywhere in a
// Git object.
func checkNoSecretsInGit(git []Object, s Secrets) error {
	vals := map[string]string{"registry password": s.PullPassword, "subscription JWT": s.JWT}
	for _, o := range git {
		b, err := o.YAML()
		if err != nil {
			return err
		}
		for what, v := range vals {
			if len(v) >= 8 && strings.Contains(string(b), v) {
				return fmt.Errorf("render: refusing to publish: the %s appears in %s %s/%s", what, o.Kind(), o.Namespace(), o.Name())
			}
		}
	}
	return nil
}

// probeTargets are the endpoints each node must reach.
func probeTargets(c *config.Config, flpURL string) []ProbeTarget {
	var t []ProbeTarget
	if c.Registry.Source == config.SourceMirror {
		h, p := splitHostPort(strings.SplitN(c.Registry.Mirror.Host, "/", 2)[0], 443)
		t = append(t, ProbeTarget{Host: h, Port: p, TLS: true, Insecure: c.Registry.Mirror.CAFile != "", Why: "private registry"})
	} else {
		t = append(t,
			ProbeTarget{Host: c.Registry.FARHost, Port: 443, TLS: true, Why: "F5 Artifact Registry"},
			ProbeTarget{Host: "ghcr.io", Port: 443, TLS: true, Why: "check image"},
		)
		if c.CertManagerInstall() {
			t = append(t, ProbeTarget{Host: "quay.io", Port: 443, TLS: true, Why: "cert-manager images"})
		}
	}
	if c.BNK.Mode == config.ModeDisconnected {
		h, p := splitHostPort(strings.TrimPrefix(strings.TrimPrefix(flpURL, "https://"), "http://"), 8443)
		t = append(t, ProbeTarget{Host: h, Port: p, TLS: true, Insecure: true, Why: "F5 License Proxy"})
	} else {
		t = append(t,
			ProbeTarget{Host: "product.apis.f5.com", Port: 443, TLS: true, Why: "F5 licensing"},
			ProbeTarget{Host: "product-s.apis.f5.com", Port: 443, TLS: true, Why: "F5 licensing"},
		)
	}
	return t
}

func splitHostPort(s string, def int) (string, int) {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "]") {
		var p int
		if _, err := fmt.Sscanf(s[i+1:], "%d", &p); err == nil && p > 0 {
			return s[:i], p
		}
	}
	return s, def
}

// application is the Argo CD Application, created through the API by `install`.
func application(c *config.Config, workspace string) Object {
	app := Object{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{
			"name": c.ArgoCD.Application, "namespace": "argocd",
			// Foreground-cascading delete: without the finalizer Argo CD deletes
			// the Application and orphans BNK, skipping the uninstall checks.
			"finalizers": []any{"resources-finalizer.argocd.argoproj.io"},
			"labels":     map[string]any{LabelManagedBy: ManagedByValue, "roksbnkargoctl.io/workspace": workspace},
		},
		"spec": map[string]any{
			"project": c.ArgoCD.Project,
			"source": map[string]any{
				"repoURL": c.Git.URL, "targetRevision": c.Git.Branch, "path": c.Git.Path,
				"directory": map[string]any{"recurse": true},
			},
			"destination": map[string]any{"server": c.Resolved.ArgoCDClusterServer, "namespace": c.BNK.Namespace},
			"syncPolicy": map[string]any{
				// Manual sync: the sync is the operator's approval. Server-side
				// apply because FLO's CRDs exceed the client-side annotation limit.
				"syncOptions": []any{"ServerSideApply=true", "RespectIgnoreDifferences=true", "PruneLast=true"},
				"retry":       map[string]any{"limit": 0},
			},
		}}
	return app
}

// RunID derives a stable id from the render inputs, so an unchanged config
// re-renders byte-identically (and commits nothing), while any change re-rolls
// the node probes.
func RunID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Write lays the output out in the workspace:
//
//	<dir>/git/NNN-<kind>-<ns>-<name>.yaml   exactly what is published to Git
//	<dir>/direct/NNN-<kind>-<ns>-<name>.yaml  applied by install; Secrets redacted
//	<dir>/application.yaml
//
// The directory is replaced wholesale so a removed object does not linger.
func Write(out *Output, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	write := func(sub string, objs []Object, redact bool) error {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		for i, o := range objs {
			if redact && o.Kind() == "Secret" {
				o = Redact(o)
			}
			b, err := o.YAML()
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(d, FileName(i, o)), b, 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	if err := write("git", out.Git, false); err != nil {
		return err
	}
	if err := write("direct", out.Direct, true); err != nil {
		return err
	}
	b, err := out.Application.YAML()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "application.yaml"), b, 0o600)
}

// FileName is a sortable, readable file name for an object.
func FileName(i int, o Object) string {
	parts := []string{fmt.Sprintf("%03d", i), strings.ToLower(o.Kind())}
	if o.Namespace() != "" {
		parts = append(parts, o.Namespace())
	}
	parts = append(parts, o.Name())
	n := strings.Join(parts, "-")
	n = strings.NewReplacer(":", "_", "/", "_", " ", "_").Replace(n)
	return n + ".yaml"
}

// Summary counts objects by wave for display.
func Summary(objs []Object) []string {
	byWave := map[int][]string{}
	var waves []int
	for _, o := range objs {
		w := o.Wave()
		label := o.Kind()
		if o.IsHook() {
			label = "hook:" + o.Name()
			if h := o.Annotation(AnnoHook); h != "" && h != "Sync" {
				w = 1000 // post-sync / delete hooks sort last
				label = h + ":" + o.Name()
			}
		}
		if _, ok := byWave[w]; !ok {
			waves = append(waves, w)
		}
		byWave[w] = append(byWave[w], label)
	}
	sort.Ints(waves)
	var out []string
	for _, w := range waves {
		counts := map[string]int{}
		var order []string
		for _, l := range byWave[w] {
			if counts[l] == 0 {
				order = append(order, l)
			}
			counts[l]++
		}
		var parts []string
		for _, l := range order {
			if counts[l] > 1 {
				parts = append(parts, fmt.Sprintf("%s×%d", l, counts[l]))
			} else {
				parts = append(parts, l)
			}
		}
		ws := fmt.Sprint(w)
		if w == 1000 {
			ws = "hooks"
		}
		out = append(out, fmt.Sprintf("%6s  %s", ws, strings.Join(parts, ", ")))
	}
	return out
}
