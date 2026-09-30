package render

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Object is one Kubernetes manifest as a generic map, the shape both Helm output
// and our own objects share.
type Object map[string]any

// Kind, Name, Namespace, APIVersion read the standard fields.
func (o Object) Kind() string       { return str(o["kind"]) }
func (o Object) APIVersion() string { return str(o["apiVersion"]) }
func (o Object) Name() string       { return str(o.meta()["name"]) }
func (o Object) Namespace() string  { return str(o.meta()["namespace"]) }

func (o Object) meta() map[string]any {
	m, _ := o["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		o["metadata"] = m
	}
	return m
}

// SetNamespace sets metadata.namespace.
func (o Object) SetNamespace(ns string) { o.meta()["namespace"] = ns }

// Annotations returns (creating) metadata.annotations.
func (o Object) Annotations() map[string]any {
	m := o.meta()
	a, _ := m["annotations"].(map[string]any)
	if a == nil {
		a = map[string]any{}
		m["annotations"] = a
	}
	return a
}

// Labels returns (creating) metadata.labels.
func (o Object) Labels() map[string]any {
	m := o.meta()
	l, _ := m["labels"].(map[string]any)
	if l == nil {
		l = map[string]any{}
		m["labels"] = l
	}
	return l
}

// Annotation reads one annotation.
func (o Object) Annotation(k string) string {
	a, _ := o.meta()["annotations"].(map[string]any)
	return str(a[k])
}

// IsHook reports whether Argo CD will treat the object as a hook (its own
// annotation or a Helm hook annotation Argo CD maps).
func (o Object) IsHook() bool {
	return o.Annotation(AnnoHook) != "" || o.Annotation("helm.sh/hook") != ""
}

// Wave returns the object's sync wave (0 when unset).
func (o Object) Wave() int {
	var w int
	fmt.Sscanf(o.Annotation(AnnoWave), "%d", &w)
	return w
}

// SetWave sets the sync wave.
func (o Object) SetWave(w int) { o.Annotations()[AnnoWave] = fmt.Sprint(w) }

// PruneEmptyMeta removes empty metadata.annotations / metadata.labels maps.
//
// Found live on OpenShift 4.21: a Namespace server-side-applied with
// `annotations: {}` is never given its openshift.io/sa.scc.* annotations by the
// cluster-policy-controller, so every pod in it is rejected ("unable to find
// annotation openshift.io/sa.scc.uid-range"). Without the empty map the
// controller annotates it within seconds, and re-applying without it repairs a
// namespace already affected.
func (o Object) PruneEmptyMeta() {
	m, ok := o["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, k := range []string{"annotations", "labels"} {
		if v, ok := m[k].(map[string]any); ok && len(v) == 0 {
			delete(m, k)
		}
	}
}

// YAML marshals the object.
func (o Object) YAML() ([]byte, error) { return yaml.Marshal(map[string]any(o)) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Argo CD annotations.
const (
	AnnoWave         = "argocd.argoproj.io/sync-wave"
	AnnoHook         = "argocd.argoproj.io/hook"
	AnnoHookDelete   = "argocd.argoproj.io/hook-delete-policy"
	AnnoSyncOptions  = "argocd.argoproj.io/sync-options"
	AnnoCompareOpts  = "argocd.argoproj.io/compare-options"
	LabelManagedBy   = "app.kubernetes.io/managed-by"
	ManagedByValue   = "roksbnkargoctl"
	LabelComponent   = "roksbnkargoctl.io/component"
	RedactedValue    = "UkVEQUNURUQ=" // base64("REDACTED")
	redactedNotice   = "roksbnkargoctl: values redacted; the real Secret is written to the cluster by `roksbnkargoctl install`, never to Git"
	annoRedactedNote = "roksbnkargoctl.io/redacted"
)

// clusterScoped kinds never carry a namespace. Anything else rendered without
// one gets the chart's release namespace, because Argo CD would otherwise put it
// in the Application's destination namespace.
var clusterScoped = map[string]bool{
	"Namespace": true, "CustomResourceDefinition": true, "ClusterRole": true,
	"ClusterRoleBinding": true, "ClusterIssuer": true, "MutatingWebhookConfiguration": true,
	"ValidatingWebhookConfiguration": true, "APIService": true, "PriorityClass": true,
	"StorageClass": true, "CNEManifest": true, "SecurityContextConstraints": true,
	"PersistentVolume": true, "ValidatingAdmissionPolicy": true,
	"ValidatingAdmissionPolicyBinding": true, "CSIDriver": true, "RuntimeClass": true,
}

// IsClusterScoped reports whether kind is cluster-scoped.
func IsClusterScoped(kind string) bool { return clusterScoped[kind] }

// SplitYAML parses a multi-document YAML stream into objects, skipping empty docs.
func SplitYAML(stream string) ([]Object, error) {
	var out []Object
	for i, doc := range splitDocs(stream) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var o Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if len(o) == 0 || o.Kind() == "" {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

func splitDocs(s string) []string {
	var docs []string
	var cur bytes.Buffer
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimRight(line, " \t\r") == "---" {
			docs = append(docs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	docs = append(docs, cur.String())
	return docs
}

// Sort orders objects by wave, then a kind order that puts prerequisites first
// (the same order Argo CD applies within a wave), then namespace and name. It
// makes rendering deterministic, so re-rendering an unchanged config commits
// nothing.
func Sort(objs []Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		a, b := objs[i], objs[j]
		if a.Wave() != b.Wave() {
			return a.Wave() < b.Wave()
		}
		if ka, kb := kindRank(a.Kind()), kindRank(b.Kind()); ka != kb {
			return ka < kb
		}
		if a.Kind() != b.Kind() {
			return a.Kind() < b.Kind()
		}
		if a.Namespace() != b.Namespace() {
			return a.Namespace() < b.Namespace()
		}
		return a.Name() < b.Name()
	})
}

var kindOrder = []string{
	"Namespace", "CustomResourceDefinition", "ServiceAccount", "Secret", "ConfigMap",
	"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Service",
	"DaemonSet", "Deployment", "StatefulSet", "Job",
}

func kindRank(k string) int {
	for i, x := range kindOrder {
		if x == k {
			return i
		}
	}
	return len(kindOrder)
}

// Redact returns a copy of a Secret with every value replaced, for the
// workspace's reference copy.
func Redact(o Object) Object {
	c := deepCopy(o).(map[string]any)
	out := Object(c)
	for _, k := range []string{"data", "stringData"} {
		if m, ok := out[k].(map[string]any); ok {
			for key := range m {
				if k == "data" {
					m[key] = RedactedValue
				} else {
					m[key] = "REDACTED"
				}
			}
		}
	}
	out.Annotations()[annoRedactedNote] = redactedNotice
	return out
}

// MaskLikeArgoCD returns a copy of a Secret with every value replaced as Argo
// CD's manifests API returns it ("++++++++", verified on 3.5.1): the charts'
// reference copy, which install --no-publish compares with that API.
func MaskLikeArgoCD(o Object) Object {
	c := deepCopy(o).(map[string]any)
	out := Object(c)
	for _, k := range []string{"data", "stringData"} {
		if m, ok := out[k].(map[string]any); ok {
			for key := range m {
				m[key] = ArgoCDMask
			}
		}
	}
	return out
}

// ArgoCDMask is the value Argo CD shows for every Secret value.
const ArgoCDMask = "++++++++"

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case Object:
		return deepCopy(map[string]any(t))
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = deepCopy(x)
		}
		return s
	default:
		return v
	}
}
