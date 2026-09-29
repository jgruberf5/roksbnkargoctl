package checks

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

func putVAP(s *kubefake.Server) {
	s.Put("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies", obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicy", "", GatewayAPIVAPName))
	s.Put("admissionregistration.k8s.io", "v1", "validatingadmissionpolicybindings", obj("admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding", "", GatewayAPIVAPName))
}

// The ingress operator re-creates the policy; the sweep must keep deleting it
// (policy AND binding) and stop once both CRDs exist.
func TestGatewaySweepDeletesVAPAndStopsWhenCRDsExist(t *testing.T) {
	s, env, res := newFake(t)
	putVAP(s)
	vapDeletes := 0
	s.AfterDelete = func(s *kubefake.Server, r kubefake.Request) {
		if strings.Contains(r.Path, "validatingadmissionpolicies/") {
			vapDeletes++
			if vapDeletes < 3 {
				putVAP(s) // operator restores it
				return
			}
			// Third removal: FLO's crd-installer gets through.
			s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "gateways.gateway.networking.k8s.io"))
			s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "gatewaysettings.gateway.k8s.f5.com"))
			putVAP(s) // restored again, but must not be deleted any more
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- GatewayAPISweep(context.Background(), env, GatewaySweepConfig{Interval: 10 * time.Millisecond, Timeout: 5 * time.Second}, res)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not stop after the CRDs appeared")
	}
	if !hasSev(res, "gateway-api-crds", SevPass) {
		t.Fatalf("not passed: %+v", res.Findings)
	}
	reqs := s.Requests()
	if n := len(deletes(reqs, "validatingadmissionpolicies/"+GatewayAPIVAPName)); n != 3 {
		t.Errorf("VAP deleted %d times, want exactly 3 (no deletes after the CRDs exist)", n)
	}
	if n := len(deletes(reqs, "validatingadmissionpolicybindings/"+GatewayAPIVAPName)); n != 3 {
		t.Errorf("binding deleted %d times, want 3", n)
	}
	if s.Get("admissionregistration.k8s.io", "v1", "validatingadmissionpolicies", "", GatewayAPIVAPName) == nil {
		t.Error("the policy restored after success was deleted anyway")
	}
}

func TestGatewaySweepOnlyOneCRDKeepsSweepingThenTimesOut(t *testing.T) {
	s, env, res := newFake(t)
	putVAP(s)
	s.Put("apiextensions.k8s.io", "v1", "customresourcedefinitions", obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "gateways.gateway.networking.k8s.io"))
	_ = GatewayAPISweep(context.Background(), env, GatewaySweepConfig{Interval: 10 * time.Millisecond, Timeout: 100 * time.Millisecond}, res)
	if !hasSev(res, "gateway-api-crds", SevFail) || !strings.Contains(detail(res, "gateway-api-crds"), "gatewaysettings.gateway.k8s.f5.com") {
		t.Fatalf("want a failure naming the missing CRD: %+v", res.Findings)
	}
	if len(deletes(s.Requests(), "validatingadmissionpolicies/")) == 0 {
		t.Fatal("policy never deleted")
	}
}
