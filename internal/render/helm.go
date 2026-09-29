package render

import (
	"bytes"
	"fmt"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
)

// ChartRender says how to template one chart.
type ChartRender struct {
	Chart       []byte // the .tgz
	Release     string
	Namespace   string
	Values      map[string]any
	KubeVersion string   // e.g. "v1.34.9", the cluster's
	APIVersions []string // extra group/versions the chart tests with .Capabilities
}

// RenderChart templates a chart in-process with the Helm SDK: client-only, no
// cluster, no helm binary. `lookup` returns nothing, as it would under
// `helm template` or Argo CD's own Helm rendering; callers set values to steer
// charts that fail on an empty lookup (FLO's skipCertMgr).
//
// Hooks are returned with their helm.sh/hook annotations intact: Argo CD maps
// them to its own phases.
func RenderChart(r ChartRender) ([]Object, error) {
	ch, err := loader.LoadArchive(bytes.NewReader(r.Chart))
	if err != nil {
		return nil, fmt.Errorf("loading chart: %w", err)
	}
	inst := action.NewInstall(&action.Configuration{})
	inst.DryRun = true
	inst.DryRunOption = "client"
	inst.ClientOnly = true
	inst.Replace = true
	inst.IncludeCRDs = true
	inst.ReleaseName = r.Release
	inst.Namespace = r.Namespace
	inst.DisableOpenAPIValidation = true
	if r.KubeVersion != "" {
		kv, err := chartutil.ParseKubeVersion(r.KubeVersion)
		if err != nil {
			return nil, fmt.Errorf("kube version %q: %w", r.KubeVersion, err)
		}
		inst.KubeVersion = kv
	}
	inst.APIVersions = chartutil.VersionSet(r.APIVersions)
	rel, err := inst.Run(ch, r.Values)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", ch.Name(), err)
	}
	var stream strings.Builder
	stream.WriteString(rel.Manifest)
	for _, h := range rel.Hooks {
		stream.WriteString("\n---\n")
		stream.WriteString(h.Manifest)
	}
	objs, err := SplitYAML(stream.String())
	if err != nil {
		return nil, fmt.Errorf("parsing %s output: %w", ch.Name(), err)
	}
	for _, o := range objs {
		if !IsClusterScoped(o.Kind()) && o.Namespace() == "" {
			o.SetNamespace(r.Namespace)
		}
	}
	return objs, nil
}

// OpenShiftAPIVersions are the group/versions charts probe to detect OpenShift
// (FLO's rbac.yaml grants SCC access only when config.openshift.io/v1 exists).
var OpenShiftAPIVersions = []string{
	"config.openshift.io/v1", "security.openshift.io/v1", "route.openshift.io/v1",
	"k8s.cni.cncf.io/v1",
}
