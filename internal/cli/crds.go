package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Why uninstall removes CRDs, and which.
//
// They used to stay, by design: cluster-scoped, shared by any BNK on the
// cluster, and deleting a CRD deletes every object of its kind. The cost showed
// on bnkargo (#44): the experimental Gateway API CRDs BNK installs (OpenShift's
// admission policy against them is removed by check-gateway-api-sweep) left the
// ingress operator Degraded — "Unmanaged Gateway API CRDs found" — and the
// cluster's OpenShift update stuck at Partial, Upgradeable=False, for weeks after
// BNK was gone. And 101 F5 and cert-manager CRDs had piled up from installs long
// uninstalled.
//
// So uninstall removes them, with the hazards as guards: nothing at all while a
// FLO Deployment or a CNEInstance exists anywhere on the cluster (another BNK,
// or this one not fully gone); and a CRD only when no object of its kind is left
// anywhere, so deleting it destroys nothing and cannot hang on a finalizer whose
// operator is gone. `--keep-crds` keeps the old behaviour.

var (
	gvrCRD         = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	gvrDeployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrCNEInstance = schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
)

// gatewayAPIGroup is upstream Gateway API. OpenShift installs and owns its
// standard channel; only experimental-channel CRDs are BNK's.
const gatewayAPIGroup = "gateway.networking.k8s.io"

// openshiftGatewayAPIPolicy is the ValidatingAdmissionPolicy (and binding) with
// which OpenShift's ingress operator refuses any change to Gateway API CRDs,
// deletion included: "Gateway API Custom Resource Definitions are managed by the
// Ingress Operator and may not be modified". check-gateway-api-sweep removes it
// so BNK can install its CRDs; the operator restores it within seconds. Live
// (run 95) every experimental CRD delete was refused by it, so the ingress
// operator stayed Degraded although uninstall had removed 102 other CRDs.
const openshiftGatewayAPIPolicy = "openshift-ingress-operator-gatewayapi-crd-admission"

var (
	gvrVAP        = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	gvrVAPBinding = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
)

// crdKind is why a CRD is uninstall's to remove ("" when it is not).
func crdKind(group, channel string, certManagerOurs bool) string {
	switch {
	case group == "f5.com" || group == "f5net.com" || strings.HasSuffix(group, ".f5.com") || strings.HasSuffix(group, ".f5net.com"):
		return "F5"
	case group == gatewayAPIGroup && channel == "experimental":
		return "Gateway API (experimental)"
	case certManagerOurs && (group == "cert-manager.io" || group == "acme.cert-manager.io"):
		return "cert-manager"
	}
	return ""
}

// crdResult is what removeCRDs did.
type crdResult struct {
	Removed []string
	Kept    []string // "name: why"
	Skipped string   // non-empty: nothing was removed, and why
}

// removeCRDs deletes the CRDs uninstall caused to exist, where nothing of their
// kind is left; see the comment at the top of this file.
func removeCRDs(ctx context.Context, dyn dynamic.Interface, certManagerOurs bool, wait time.Duration) (crdResult, error) {
	var res crdResult
	flo, err := dyn.Resource(gvrDeployments).Namespace("").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=f5-lifecycle-operator", Limit: 1})
	if err != nil {
		return res, fmt.Errorf("looking for an F5 Lifecycle Operator: %w", err)
	}
	if len(flo.Items) > 0 {
		res.Skipped = fmt.Sprintf("an F5 Lifecycle Operator still runs (%s/%s)", flo.Items[0].GetNamespace(), flo.Items[0].GetName())
		return res, nil
	}
	cne, err := dyn.Resource(gvrCNEInstance).Namespace("").List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case err == nil && len(cne.Items) > 0:
		res.Skipped = fmt.Sprintf("a CNEInstance still exists (%s/%s)", cne.Items[0].GetNamespace(), cne.Items[0].GetName())
		return res, nil
	case err != nil && !apierrors.IsNotFound(err):
		return res, fmt.Errorf("looking for a CNEInstance: %w", err)
	}

	crds, err := dyn.Resource(gvrCRD).List(ctx, metav1.ListOptions{})
	if err != nil {
		return res, fmt.Errorf("listing CRDs: %w", err)
	}
	var deleted []string
	for _, c := range crds.Items {
		group, _, _ := unstructured.NestedString(c.Object, "spec", "group")
		kind := crdKind(group, c.GetAnnotations()["gateway.networking.k8s.io/channel"], certManagerOurs)
		if kind == "" {
			continue
		}
		gvr, ok := servedGVR(c)
		if !ok {
			res.Kept = append(res.Kept, c.GetName()+": no served version to check for objects")
			continue
		}
		objs, err := dyn.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil && !apierrors.IsNotFound(err) {
			res.Kept = append(res.Kept, c.GetName()+": cannot list its objects: "+err.Error())
			continue
		}
		if err == nil && len(objs.Items) > 0 {
			o := objs.Items[0]
			where := o.GetName()
			if o.GetNamespace() != "" {
				where = o.GetNamespace() + "/" + where
			}
			res.Kept = append(res.Kept, fmt.Sprintf("%s: %s objects remain (%s)", c.GetName(), kind, where))
			continue
		}
		del := deleteCRD
		if group == gatewayAPIGroup {
			del = deleteGatewayAPICRD
		}
		if err := del(ctx, dyn, c.GetName()); err != nil && !apierrors.IsNotFound(err) {
			res.Kept = append(res.Kept, c.GetName()+": delete failed: "+err.Error())
			continue
		}
		deleted = append(deleted, c.GetName())
	}
	// A CRD with no objects goes at once; one that does not has gained an
	// object since the check, and says so by staying.
	deadline := time.Now().Add(wait)
	for {
		var left []string
		for _, n := range deleted {
			if _, err := dyn.Resource(gvrCRD).Get(ctx, n, metav1.GetOptions{}); err == nil {
				left = append(left, n)
			}
		}
		if len(left) == 0 || !time.Now().Before(deadline) {
			for _, n := range deleted {
				if containsString(left, n) {
					res.Kept = append(res.Kept, n+": deleted but still present after "+wait.String())
				} else {
					res.Removed = append(res.Removed, n)
				}
			}
			break
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	sort.Strings(res.Removed)
	sort.Strings(res.Kept)
	return res, nil
}

func deleteCRD(ctx context.Context, dyn dynamic.Interface, name string) error {
	return dyn.Resource(gvrCRD).Delete(ctx, name, metav1.DeleteOptions{})
}

// deleteGatewayAPICRD lifts OpenShift's Gateway API admission policy and deletes
// the CRD, again while the ingress operator keeps restoring the policy in
// between (it does within seconds), for up to gatewayAPIDeleteAttempts tries.
func deleteGatewayAPICRD(ctx context.Context, dyn dynamic.Interface, name string) error {
	var err error
	for i := 0; i < gatewayAPIDeleteAttempts; i++ {
		for _, gvr := range []schema.GroupVersionResource{gvrVAPBinding, gvrVAP} {
			if derr := dyn.Resource(gvr).Delete(ctx, openshiftGatewayAPIPolicy, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("lifting OpenShift's Gateway API admission policy: %w", derr)
			}
		}
		err = deleteCRD(ctx, dyn, name)
		if err == nil || apierrors.IsNotFound(err) || !apierrors.IsForbidden(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gatewayAPIRetryWait):
		}
	}
	return err
}

// Tests shorten the wait.
var (
	gatewayAPIDeleteAttempts = 30
	gatewayAPIRetryWait      = 2 * time.Second
)

// servedGVR is the CRD's storage version (else its first served one) as a
// resource to list.
func servedGVR(c unstructured.Unstructured) (schema.GroupVersionResource, bool) {
	group, _, _ := unstructured.NestedString(c.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(c.Object, "spec", "names", "plural")
	versions, _, _ := unstructured.NestedSlice(c.Object, "spec", "versions")
	pick := ""
	for _, v := range versions {
		m, _ := v.(map[string]any)
		name, _ := m["name"].(string)
		served, _ := m["served"].(bool)
		if !served {
			continue
		}
		if storage, _ := m["storage"].(bool); storage {
			pick = name
			break
		}
		if pick == "" {
			pick = name
		}
	}
	if pick == "" || plural == "" {
		return schema.GroupVersionResource{}, false
	}
	return schema.GroupVersionResource{Group: group, Version: pick, Resource: plural}, true
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
