// Package kube is the operator-side Kubernetes client for the few things
// `install` and `uninstall` do in ROKS directly: server-side apply of the
// out-of-band objects (Secrets, the check namespace and RBAC), minting the
// ServiceAccount token Argo CD uses, and reading cluster facts the renderer needs.
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager is the server-side-apply owner of everything we write directly.
const FieldManager = "roksbnkargoctl"

// Client wraps the typed, dynamic and discovery clients for one cluster.
type Client struct {
	Config  *rest.Config
	Typed   kubernetes.Interface
	Dynamic dynamic.Interface
	mapper  meta.RESTMapper
}

// FromKubeconfig builds a client from kubeconfig bytes (the self-contained admin
// kubeconfig the IBM containers API returns).
func FromKubeconfig(kubeconfig []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.Timeout = 60 * time.Second
	cfg.QPS, cfg.Burst = 20, 40
	return FromConfig(cfg)
}

// FromConfig builds a client from a rest.Config.
func FromConfig(cfg *rest.Config) (*Client, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	return &Client{Config: cfg, Typed: typed, Dynamic: dyn, mapper: mapper}, nil
}

// Streaming returns a clientset for long-lived watches. The regular client's
// 60s http.Client timeout also covers reading a response body, which cuts a
// watch stream after a minute (found in review: the uninstall log watch died
// 60s in, long before a real drain finishes).
func (c *Client) Streaming() (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(c.Config)
	cfg.Timeout = 0
	return kubernetes.NewForConfig(cfg)
}

// ServerVersion returns the Kubernetes git version (e.g. v1.34.9).
func (c *Client) ServerVersion() (string, error) {
	v, err := c.Typed.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// Apply server-side-applies one object (a generic map) with force, so objects
// we own converge to what we render.
//
// Empty metadata.annotations / metadata.labels maps are dropped first: applying
// `annotations: {}` to a Namespace stops OpenShift's cluster-policy-controller
// from ever adding the SCC annotations pods need (see render.PruneEmptyMeta).
func (c *Client) Apply(ctx context.Context, obj map[string]any) error {
	pruneEmptyMeta(obj)
	u := &unstructured.Unstructured{Object: obj}
	gvk := u.GroupVersionKind()
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("%s: %w", gvk.Kind, err)
	}
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = c.Dynamic.Resource(mapping.Resource).Namespace(u.GetNamespace())
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", gvk.Kind, u.GetNamespace(), u.GetName(), err)
	}
	return nil
}

// pruneEmptyMeta drops empty metadata.annotations / metadata.labels maps.
func pruneEmptyMeta(obj map[string]any) {
	m, ok := obj["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, k := range []string{"annotations", "labels"} {
		if v, ok := m[k].(map[string]any); ok && len(v) == 0 {
			delete(m, k)
		}
	}
}

// Delete removes one object; absent is success.
func (c *Client) Delete(ctx context.Context, obj map[string]any) error {
	u := &unstructured.Unstructured{Object: obj}
	gvk := u.GroupVersionKind()
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = c.Dynamic.Resource(mapping.Resource).Namespace(u.GetNamespace())
	}
	pol := metav1.DeletePropagationBackground
	err = ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{PropagationPolicy: &pol})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// NodeResolverImage is an image every ROKS node has cached: the registry CA
// installer runs on it before anything can pull from a private-CA mirror.
func (c *Client) NodeResolverImage(ctx context.Context) (string, error) {
	ds, err := c.Typed.AppsV1().DaemonSets("openshift-dns").Get(ctx, "node-resolver", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading openshift-dns/node-resolver: %w", err)
	}
	if len(ds.Spec.Template.Spec.Containers) == 0 || ds.Spec.Template.Spec.Containers[0].Image == "" {
		return "", errors.New("openshift-dns/node-resolver has no container image")
	}
	return ds.Spec.Template.Spec.Containers[0].Image, nil
}

// ArgoManager names the ServiceAccount Argo CD uses to manage the cluster.
const (
	ArgoManagerNamespace = "kube-system"
	ArgoManagerName      = "roksbnkargoctl-argocd-manager"
)

// EnsureArgoCDManager creates the ServiceAccount, a cluster-admin binding (Argo
// CD installs CRDs, cluster roles and SCC bindings) and a long-lived token Secret,
// then returns the token and the cluster CA. This is what `argocd cluster add`
// does, done without the argocd binary.
func (c *Client) EnsureArgoCDManager(ctx context.Context) (token string, caData []byte, err error) {
	objs := []map[string]any{
		{"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": ArgoManagerName, "namespace": ArgoManagerNamespace}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
			"metadata": map[string]any{"name": ArgoManagerName},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": ArgoManagerName, "namespace": ArgoManagerNamespace}}},
		{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/service-account-token",
			"metadata": map[string]any{"name": ArgoManagerName + "-token", "namespace": ArgoManagerNamespace,
				"annotations": map[string]any{"kubernetes.io/service-account.name": ArgoManagerName}}},
	}
	for _, o := range objs {
		if err := c.Apply(ctx, o); err != nil {
			return "", nil, err
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s, err := c.Typed.CoreV1().Secrets(ArgoManagerNamespace).Get(ctx, ArgoManagerName+"-token", metav1.GetOptions{})
		if err == nil && len(s.Data["token"]) > 0 {
			return string(s.Data["token"]), s.Data["ca.crt"], nil
		}
		if time.Now().After(deadline) {
			return "", nil, fmt.Errorf("token for %s/%s was not populated in 2m", ArgoManagerNamespace, ArgoManagerName)
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// RemoveArgoCDManager deletes what EnsureArgoCDManager created.
func (c *Client) RemoveArgoCDManager(ctx context.Context) error {
	var errs []error
	for _, o := range []map[string]any{
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": ArgoManagerName + "-token", "namespace": ArgoManagerNamespace}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": map[string]any{"name": ArgoManagerName}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": ArgoManagerName, "namespace": ArgoManagerNamespace}},
	} {
		if err := c.Delete(ctx, o); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Get reads one object by apiVersion/kind/namespace/name.
func (c *Client) Get(ctx context.Context, apiVersion, kind, namespace, name string) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	mapping, err := c.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		return nil, err
	}
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = c.Dynamic.Resource(mapping.Resource).Namespace(namespace)
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

// IsNotFound reports a missing object or an unknown kind.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || (err != nil && strings.Contains(err.Error(), "no matches for kind"))
}

// JobResult is one check Job's outcome, for `status`.
type JobResult struct {
	Name, Phase, Message string
}

// CheckJobs lists the check hook Jobs and their state.
func (c *Client) CheckJobs(ctx context.Context, namespace string) ([]JobResult, error) {
	jobs, err := c.Typed.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: "roksbnkargoctl.io/component=check"})
	if err != nil {
		return nil, err
	}
	var out []JobResult
	for _, j := range jobs.Items {
		r := JobResult{Name: j.Name, Phase: "Running"}
		switch {
		case j.Status.Succeeded > 0:
			r.Phase = "Succeeded"
		case j.Status.Failed > 0:
			r.Phase = "Failed"
		}
		for _, cond := range j.Status.Conditions {
			if cond.Message != "" {
				r.Message = cond.Message
			}
		}
		out = append(out, r)
	}
	return out, nil
}
