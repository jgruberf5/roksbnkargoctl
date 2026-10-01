// Package render turns a workspace config into every Kubernetes object the BNK
// install consists of, split three ways:
//
//   - Git: what Argo CD syncs from Git: our own objects, and the values file of
//     each Helm chart. No secret values, ever. The charts themselves
//     (cert-manager, FLO) are not expanded into Git: the Application installs
//     them as Helm sources, as `helm install` would in F5's manual procedure.
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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"sigs.k8s.io/yaml"
)

// Waves (DESIGN.md "Sync order"). The cert-manager and FLO charts are Helm
// sources of the Application, so their objects carry no wave and sync in
// wave 0: what must come before them is negative, what needs them positive.
const (
	WaveNamespaces   = -20
	WaveCATrust      = -19
	WavePreInstall   = -18
	WaveCert         = -10 // single-certificate mode: the Secret FLO mounts, before FLO
	WaveNetwork      = -6
	WaveSweep        = -6
	WaveCharts       = 0 // cert-manager and FLO, from their Helm sources
	WaveCMReady      = 1
	WaveIssuers      = 2 // 2 self-signed, 3 ext-ca, 4 sample-issuer
	WaveCNEManifest  = 6
	WaveCNEInstance  = 8
	WaveLicenseCheck = 10
)

// Secrets carries the values that go into Direct Secrets. Empty fields produce
// no Secret.
type Secrets struct {
	PullHost     string // registry host kubelet matches (no path)
	PullUsername string
	PullPassword string
	JWT          string
	FLPCAPEM     string
	// Single-certificate mode, issuer ca (CertPEM, KeyPEM: your CA) or
	// provided (CertPEM, KeyPEM, CAPEM: your certificate). Written into the
	// check namespace as SingleCertSourceSecret; never to Git.
	SingleCertPEM   string
	SingleCertKey   string
	SingleCertCAPEM string
}

// Inputs is everything a render needs. The caller does the network I/O.
type Inputs struct {
	Config           *config.Config
	Workspace        string
	Manifest         *far.Manifest
	CertManagerChart []byte // nil when cert-manager is not installed by us
	FLOChart         []byte
	// The charts' OCI references, <registry>/<path>/<chart>:<version>: the
	// Application pulls them from there.
	CertManagerChartRef string
	FLOChartRef         string
	KubeVersion         string
	CheckImage          string
	NodeResolverImage   string // required when a mirror CA is set
	MirrorCAPEM         string
	FLPURL              string // https://<ip>:8443 in disconnected mode
	RunID               string
	Secrets             Secrets
}

// Output is the three-way split.
type Output struct {
	Git         []Object
	Direct      []Object
	Application Object
	// Charts are the Application's Helm sources. Git holds each one's values
	// file (ValuesFiles); the chart itself comes from the registry.
	Charts []Chart
}

// Chart is a Helm chart the Application installs as a source of its own.
type Chart struct {
	Name      string // values/<Name>.yaml under git.path
	RepoURL   string // the OCI registry path, no scheme: Argo CD's Helm OCI form
	Chart     string
	Version   string
	Release   string
	Namespace string
	Values    map[string]any
	// Objects is the chart rendered here the way Argo CD renders it (same
	// release, namespace, values and API versions). It is never published:
	// install --no-publish compares it with what Argo CD reads back.
	Objects []Object
}

// ValuesPath is the chart's values file, relative to git.path.
func (ch Chart) ValuesPath() string { return "values/" + ch.Name + ".yaml" }

// ValuesFiles are the charts' values files, by path relative to git.path.
func (o *Output) ValuesFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, ch := range o.Charts {
		b, err := yaml.Marshal(ch.Values)
		if err != nil {
			return nil, err
		}
		files[ch.ValuesPath()] = append([]byte("# Helm values for "+ch.Chart+" "+ch.Version+", installed by the Application as release "+ch.Release+" in "+ch.Namespace+".\n"), b...)
	}
	return files, nil
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
	if singleCertSource(c) {
		cp.RequireSecrets = append(cp.RequireSecrets, CheckNamespace+"/"+SingleCertSourceSecret)
	}
	if in.MirrorCAPEM != "" {
		add(caTrust(c.Registry.Mirror.Host, in.MirrorCAPEM, in.NodeResolverImage, WaveCATrust)...)
	}
	add(cp.nodeProbe(WavePreInstall))
	add(cp.hookJob("pre-install", "Sync", WavePreInstall, cp.preInstallArgs(), 20*60))

	// cert-manager, as a Helm source.
	if c.CertManagerInstall() {
		if len(in.CertManagerChart) == 0 {
			return nil, errors.New("render: cert-manager chart missing")
		}
		ch, err := chartSource("cert-manager", in.CertManagerChartRef, in.CertManagerChart, "cert-manager", "cert-manager", bnkNS,
			certManagerValues(c, pullSecret), in.KubeVersion)
		if err != nil {
			return nil, err
		}
		out.Charts = append(out.Charts, ch)
	}
	if c.UsesCertManager() {
		// Deployment health is not webhook readiness: gate the issuers on the
		// webhook actually admitting one (a reinstall raced it live).
		add(cp.hookJob("cert-manager-ready", "Sync", WaveCMReady, []string{"--timeout=10m"}, 12*60))
		add(certChain(WaveIssuers)...)
	} else {
		// Single certificate: the one Secret FLO mounts everywhere, in every
		// BNK namespace before FLO starts.
		add(cp.hookJob("cert", "Sync", WaveCert, singleCertArgs(c), 10*60))
	}

	// Networking + FLO.
	add(nad(bnkNS, c.BNK.NADAddress, WaveNetwork))
	add(sccBinding(bnkNS, "flo-f5-lifecycle-operator", WaveNetwork))
	add(cp.sweep(WaveSweep))
	flo, err := chartSource("flo", in.FLOChartRef, in.FLOChart, "flo", bnkNS, bnkNS, floValues(c, pullSecret), in.KubeVersion)
	if err != nil {
		return nil, err
	}
	out.Charts = append(out.Charts, flo)
	add(cneManifest(in.Manifest, WaveCNEManifest))
	add(cneInstance(CNEInstanceParams{
		Namespace: bnkNS, ImageHost: c.ImageHost(), PullSecret: pullSecret,
		Region: c.IBMCloud.Region, VPCName: c.Resolved.VPCName, TrustedProfileID: c.Resolved.TrustedProfileID,
		TMMReplicas: c.BNK.TMMReplicas, StorageClass: c.BNK.StorageClass, Version: c.BNK.Version,
		CertManager: c.UsesCertManager(),
	}, WaveCNEInstance))

	// License + lifecycle checks.
	cp.LicenseArgs = licenseArgs(c, in.FLPURL)
	add(cp.hookJob("license", "Sync", WaveLicenseCheck, append(licenseWaitArgs(), cp.LicenseArgs[1:]...), licenseDeadlineSeconds()))
	add(cp.hookJob("post-install", "PostSync", 0, []string{"--timeout=10m", "--bnk-namespace=" + bnkNS,
		"--utils-namespace=" + utilsNS, fmt.Sprintf("--tmm-replicas=%d", c.BNK.TMMReplicas)}, 15*60))
	add(cp.hookJob("pre-uninstall", "PreDelete", 0, []string{"--timeout=15m", "--bnk-namespace=" + bnkNS,
		"--utils-namespace=" + utilsNS}, 20*60))
	postArgs := []string{"--timeout=15m", "--bnk-namespace=" + bnkNS, "--utils-namespace=" + utilsNS}
	if c.CertManagerInstall() {
		postArgs = append(postArgs, "--delete-namespace=cert-manager")
	}
	add(cp.hookJob("post-uninstall", "PostDelete", 0, postArgs, 20*60))

	// Split: every Secret of ours goes Direct. (A Secret a chart ships, like
	// FLO's external-otelsvr-secret, is chart content: Argo CD renders it from
	// the chart, and it is never in Git.)
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
	out.Direct = append(checkRBAC(!c.UsesCertManager()), out.Direct...)
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
	files, err := out.ValuesFiles()
	if err != nil {
		return nil, err
	}
	if err := checkNoSecretsInFiles(files, in.Secrets); err != nil {
		return nil, err
	}
	if err := checkNoSecretsInGit(out.Git, in.Secrets); err != nil {
		return nil, err
	}
	Sort(out.Git)
	out.Application = application(c, in.Workspace, out.Charts, in.KubeVersion)
	return out, nil
}

// stamp keeps a CRD in Git across uninstall: deleting a CRD deletes every CR
// with it, and F5 CRs whose finalizer's controller is gone hang their namespace
// (roksbnkctl keeps F5 CRDs for the same reason). The charts' CRDs are not in
// Git: both charts annotate them helm.sh/resource-policy: keep, which Argo CD
// honours on delete (verified on 3.5.1).
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
	if singleCertSource(c) && s.SingleCertPEM != "" {
		data := map[string]any{"tls.crt": s.SingleCertPEM, "tls.key": s.SingleCertKey}
		if s.SingleCertCAPEM != "" {
			data["ca.crt"] = s.SingleCertCAPEM
		}
		out = append(out, Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata":   map[string]any{"name": SingleCertSourceSecret, "namespace": CheckNamespace},
			"stringData": data})
	}
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

// checkNoSecretsInFiles is checkNoSecretsInGit for the charts' values files.
func checkNoSecretsInFiles(files map[string][]byte, s Secrets) error {
	vals := map[string]string{"registry password": s.PullPassword, "subscription JWT": s.JWT, "single-certificate private key": s.SingleCertKey}
	for name, b := range files {
		for what, v := range vals {
			if len(v) >= 8 && strings.Contains(string(b), v) {
				return fmt.Errorf("render: refusing to publish: the %s appears in %s", what, name)
			}
		}
	}
	return nil
}

// checkNoSecretsInGit is the last line of defence for DESIGN.md's "no secret
// values in Git": it fails the render if any secret value appears anywhere in a
// Git object.
func checkNoSecretsInGit(git []Object, s Secrets) error {
	vals := map[string]string{"registry password": s.PullPassword, "subscription JWT": s.JWT, "single-certificate private key": s.SingleCertKey}
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
func application(c *config.Config, workspace string, charts []Chart, kubeVersion string) Object {
	gitPath := strings.Trim(path.Clean("/"+c.Git.Path), "/")
	var sources []any
	for _, ch := range charts {
		helm := map[string]any{
			"releaseName": ch.Release, "namespace": ch.Namespace,
			"valueFiles": []any{"$values/" + path.Join(gitPath, ch.ValuesPath())},
			// Render as here: the same API versions (and kube version), so what
			// Argo CD applies is what install --no-publish compares.
			"apiVersions": toAnySlice(OpenShiftAPIVersions),
		}
		if kubeVersion != "" {
			helm["kubeVersion"] = kubeVersion
		}
		sources = append(sources, map[string]any{"repoURL": ch.RepoURL, "chart": ch.Chart, "targetRevision": ch.Version, "helm": helm})
	}
	// The Git source is both the objects at git.path and, as $values, the
	// charts' values files under git.path/values (not applied: no recursion).
	sources = append(sources, map[string]any{
		"repoURL": c.Git.URL, "targetRevision": c.Git.Branch, "path": c.Git.Path, "ref": "values",
		"directory": map[string]any{"recurse": false},
	})
	app := Object{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{
			"name": c.ArgoCD.Application, "namespace": "argocd",
			// Foreground-cascading delete: without the finalizer Argo CD deletes
			// the Application and orphans BNK, skipping the uninstall checks.
			"finalizers": []any{"resources-finalizer.argocd.argoproj.io"},
			"labels":     map[string]any{LabelManagedBy: ManagedByValue, "roksbnkargoctl.io/workspace": workspace},
		},
		"spec": map[string]any{
			"project":     c.ArgoCD.Project,
			"sources":     sources,
			"destination": map[string]any{"server": c.Resolved.ArgoCDClusterServer, "namespace": c.BNK.Namespace},
			"syncPolicy": map[string]any{
				// Manual sync: the sync is the operator's approval. Server-side
				// apply because FLO's CRDs exceed the client-side annotation limit.
				"syncOptions": []any{"ServerSideApply=true", "RespectIgnoreDifferences=true", "PruneLast=true"},
				"retry":       map[string]any{"limit": 0},
			},
			// OpenShift appends its own <sa>-dockercfg-* pull secret to every
			// ServiceAccount; without this the cert-manager ServiceAccounts
			// (which carry the mirror pull secret) read OutOfSync after every
			// sync. Only OpenShift's entries are ignored, never ours.
			"ignoreDifferences": []any{map[string]any{
				"group": "", "kind": "ServiceAccount",
				"jqPathExpressions": []any{`.imagePullSecrets[]? | select(.name | test("-dockercfg-[a-z0-9]+$"))`},
			}},
		}}
	return app
}

// chartSource renders a chart the way Argo CD will render it as a Helm source
// and describes that source. ref is <registry>/<path>/<chart>:<version>.
// destNS is the Application's destination namespace: Argo CD puts a namespaced
// object that names none there, whatever the release namespace, so a chart
// released elsewhere must name its namespace on every object.
func chartSource(name, ref string, chart []byte, release, namespace, destNS string, values map[string]any, kubeVersion string) (Chart, error) {
	repo, chartName, version, err := splitChartRef(ref)
	if err != nil {
		return Chart{}, fmt.Errorf("render: %s chart: %w", name, err)
	}
	objs, err := RenderChart(ChartRender{Chart: chart, Release: release, Namespace: destNS,
		Values: values, KubeVersion: kubeVersion, APIVersions: OpenShiftAPIVersions, ReleaseNamespace: namespace})
	if err != nil {
		return Chart{}, err
	}
	if namespace != destNS {
		for _, o := range objs {
			if !IsClusterScoped(o.Kind()) && o.Namespace() == destNS {
				return Chart{}, fmt.Errorf("render: the %s chart renders %s %s without a namespace; Argo CD would put it in %s, not %s", name, o.Kind(), o.Name(), destNS, namespace)
			}
		}
	}
	return Chart{Name: name, RepoURL: repo, Chart: chartName, Version: version, Release: release, Namespace: namespace,
		Values: values, Objects: objs}, nil
}

// splitChartRef splits <registry>/<path>/<chart>:<version>.
func splitChartRef(ref string) (repo, chart, version string, err error) {
	i := strings.LastIndex(ref, "/")
	j := strings.LastIndex(ref, ":")
	if i <= 0 || j <= i+1 || j == len(ref)-1 {
		return "", "", "", fmt.Errorf("%q is not <registry>/<path>/<chart>:<version>", ref)
	}
	return ref[:i], ref[i+1 : j], ref[j+1:], nil
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
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
//	<dir>/git/NNN-<kind>-<ns>-<name>.yaml   exactly what is published to Git,
//	<dir>/git/values/<chart>.yaml           with the charts' values files
//	<dir>/direct/NNN-<kind>-<ns>-<name>.yaml  applied by install; Secrets redacted
//	<dir>/charts/<chart>/NNN-….yaml         each chart as Argo CD renders it (not published)
//	<dir>/application.yaml
//
// The directory is replaced wholesale so a removed object does not linger.
func Write(out *Output, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	write := func(sub string, objs []Object, mask func(Object) Object) error {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		for i, o := range objs {
			if mask != nil && o.Kind() == "Secret" {
				o = mask(o)
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
	if err := write("git", out.Git, nil); err != nil {
		return err
	}
	files, err := out.ValuesFiles()
	if err != nil {
		return err
	}
	for name, b := range files {
		fp := filepath.Join(dir, "git", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(fp, b, 0o600); err != nil {
			return err
		}
	}
	// Each chart as Argo CD will render it, for review and for the
	// --no-publish comparison. Not published.
	for _, ch := range out.Charts {
		if err := write(filepath.Join("charts", ch.Name), ch.Objects, MaskLikeArgoCD); err != nil {
			return err
		}
	}
	if err := write("direct", out.Direct, Redact); err != nil {
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

// licenseWaits are the check-license hook's sequential waits, rendered
// explicitly: the License CRD, retrying a refused apply, License Active, then
// CNEInstance Available.
var licenseWaits = []struct {
	flag string
	d    time.Duration
}{
	{"--crd-timeout", 10 * time.Minute},
	{"--apply-retry", 5 * time.Minute},
	{"--timeout", 35 * time.Minute},
	{"--cne-timeout", 15 * time.Minute},
}

func licenseWaitArgs() []string {
	var out []string
	for _, w := range licenseWaits {
		out = append(out, w.flag+"="+w.d.String())
	}
	return out
}

// licenseDeadlineSeconds covers every wait plus five minutes, so the check
// reports which wait ran out instead of the Job dying of DeadlineExceeded with
// no finding (#12: the deadline was 40m against 65m of waits).
func licenseDeadlineSeconds() int {
	total := 5 * time.Minute
	for _, w := range licenseWaits {
		total += w.d
	}
	return int(total.Seconds())
}

// SingleCertSourceSecret holds, in the check namespace, the CA (issuer ca) or
// the certificate (issuer provided) the single-certificate Job reads.
const SingleCertSourceSecret = "bnk-single-cert-source"

// singleCertSource reports whether single-certificate mode reads operator
// material (issuer ca or provided) rather than generating a CA.
func singleCertSource(c *config.Config) bool {
	i := c.BNK.Certificates.Issuer
	return !c.UsesCertManager() && (i == "ca" || i == "provided")
}

// singleCertArgs are the `check cert` flags for the configured certificate.
func singleCertArgs(c *config.Config) []string {
	ct := c.BNK.Certificates
	nss := c.BNK.Namespace
	if c.BNK.UtilsNamespace != c.BNK.Namespace {
		nss += "," + c.BNK.UtilsNamespace
	}
	args := []string{
		"--issuer=" + ct.Issuer,
		"--secret-name=" + ct.SecretName, "--namespace=" + nss,
		"--common-name=" + ct.CommonName, "--ca-common-name=" + ct.CACommonName,
		"--organization=" + ct.Organization, "--organizational-unit=" + ct.OrganizationalUnit,
		"--country=" + ct.Country, "--state=" + ct.State, "--locality=" + ct.Locality,
		"--key-type=" + ct.KeyType, fmt.Sprintf("--key-bits=%d", ct.KeyBits),
		fmt.Sprintf("--validity=%dh", ct.ValidityDays*24), fmt.Sprintf("--renew-before=%dh", ct.RenewBeforeDays*24),
	}
	if singleCertSource(c) {
		args = append(args, "--source-secret="+CheckNamespace+"/"+SingleCertSourceSecret)
	}
	for _, d := range ct.ExtraDNSNames {
		args = append(args, "--dns="+d)
	}
	for _, ip := range ct.IPAddresses {
		args = append(args, "--ip="+ip)
	}
	return args
}
