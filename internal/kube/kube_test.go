package kube

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Apply is the last stop before the API server; a Namespace that reaches it with
// `annotations: {}` never gets OpenShift's SCC annotations (found live).
func TestPruneEmptyMeta(t *testing.T) {
	obj := map[string]any{"metadata": map[string]any{
		"name": "ns", "annotations": map[string]any{}, "labels": map[string]any{"a": "b"},
	}}
	pruneEmptyMeta(obj)
	m := obj["metadata"].(map[string]any)
	if _, ok := m["annotations"]; ok {
		t.Fatal("empty annotations map survived")
	}
	if l, ok := m["labels"].(map[string]any); !ok || l["a"] != "b" {
		t.Fatal("a non-empty labels map must be kept")
	}
}

// Asserts on the patch Apply actually sends, so the test fails if Apply stops
// calling the prune (a test bound only to the helper would not).
func TestApplySendsNoEmptyAnnotations(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var sent []byte
	dyn.PrependReactor("patch", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sent = a.(k8stesting.PatchAction).GetPatch()
		return true, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "x"}}}, nil
	})
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	c := &Client{Dynamic: dyn, mapper: mapper}
	err := c.Apply(context.Background(), map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "x", "annotations": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if sent == nil {
		t.Fatal("no patch sent")
	}
	if strings.Contains(string(sent), `"annotations"`) {
		t.Fatalf("Apply sent an annotations map: %s", sent)
	}
}
