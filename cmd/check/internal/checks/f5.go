package checks

import (
	"strings"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// Default namespaces of a BNK 2.4 install.
const (
	DefaultFLONamespace   = "f5-bnk"
	DefaultUtilsNamespace = "f5-utils"
)

// F5Groups are the API groups BNK 2.4 serves CRDs in.
//
// metrics.f5.com, NOT metrics.f5.net — the 2.4 guide's cleanup block greps the
// latter, which matches nothing (roksbnkctl bnk_teardown_cleanup.go).
var F5Groups = []string{
	"k8s.f5.com",         // CNEInstance and the components FLO derives from it
	"k8s.f5net.com",      // License, F5BigTcpSetting, ... (the F5SPK* family)
	"gateway.k8s.f5.com", // Infra, GatewaySettings, EgressGateway, ...
	"fic.f5.com",         // IPAM, IPAMRange — controller-generated
	"metrics.f5.com",     // telemetry
}

// Resource types the checks address by name.
var (
	GVRCNEInstance  = kube.GVR{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances", Namespaced: true, Kind: "CNEInstance"}
	GVRLicense      = kube.GVR{Group: "k8s.f5net.com", Version: "v1", Resource: "licenses", Namespaced: true, Kind: "License"}
	GVRCRD          = kube.GVR{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	GVRNamespace    = kube.GVR{Version: "v1", Resource: "namespaces"}
	GVRNode         = kube.GVR{Version: "v1", Resource: "nodes"}
	GVRPod          = kube.GVR{Version: "v1", Resource: "pods", Namespaced: true}
	GVRSecret       = kube.GVR{Version: "v1", Resource: "secrets", Namespaced: true}
	GVRDeployment   = kube.GVR{Group: "apps", Version: "v1", Resource: "deployments", Namespaced: true}
	GVRDaemonSet    = kube.GVR{Group: "apps", Version: "v1", Resource: "daemonsets", Namespaced: true}
	GVRStorageClass = kube.GVR{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	GVRClusterVer   = kube.GVR{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}
	GVRVWC          = kube.GVR{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}
	GVRVAP          = kube.GVR{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	GVRVAPBinding   = kube.GVR{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
	GVRClusterIss   = kube.GVR{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
)

// CNEInstanceName is the name the FLO chart's CNEInstance takes in namespace ns.
func CNEInstanceName(floNS string) string { return floNS + "-f5-cne-controller" }

// FLOLabelSelector finds the F5 Lifecycle Operator Deployment.
const FLOLabelSelector = "app.kubernetes.io/name=f5-lifecycle-operator"

// LicenseSecretNames are the CWC secrets that survive a teardown and stop the
// next install from licensing: CWC reads them, finds a previous activation, and
// does not re-activate.
//
// Ported verbatim from roksbnkctl internal/orchestration/bnk_teardown_cleanup.go.
// Spelled out rather than matched by prefix because they sit in a namespace with
// unrelated secrets — a prefix sweep would be a wildcard delete against someone
// else's data.
var LicenseSecretNames = []string{
	"activationcontext",
	"activationmessage",
	"activationstatus",
	"certificate",
	"certificatechain",
	"configreport",
	"configreportsignedackresponse",
	"context",
	"cpcl-config-secret",
	"cpcl-key-secret",
	"csr",
	"customerprovidedid",
	"cwcstate",
	"cwcswitchstate",
	"digitalassetid",
	"digitalassetname",
	"digitalassetversion",
	"entitlements",
	"initialregistrationstatus",
	"jwtsecret",
	"licensekey",
	"licensestatus",
	"modeofoperation",
	"previousreportname",
	"previousreportverifieddate",
	"privatekey",
	"productname",
	"publickey",
	"statehistory",
	"switchlicensestatus",
	"telemetryreport",
	"telemetryreports",
	"telemetryreportsignedackresponse",
	"telemetrystatus",
}

// KnownF5Finalizers are F5 finalizers seen stuck on live teardowns. The last has
// no domain at all, so the domain rule alone would miss it.
var KnownF5Finalizers = []string{
	"k8s.f5.com/CNEInstanceFinalizer",
	"k8s.f5net.com/uninstall",
	"handletmmconfig_inconsistency",
}

// IsF5Finalizer reports whether a finalizer belongs to F5: a known one, or one
// whose domain is f5.com / f5net.com or a subdomain of either.
func IsF5Finalizer(f string) bool {
	for _, k := range KnownF5Finalizers {
		if f == k {
			return true
		}
	}
	domain, _, found := strings.Cut(f, "/")
	if !found {
		return false
	}
	return domain == "f5.com" || domain == "f5net.com" ||
		strings.HasSuffix(domain, ".f5.com") || strings.HasSuffix(domain, ".f5net.com")
}

// RetainNonF5Finalizers splits a finalizer list into what stays and how many F5
// entries were dropped. Everything that is not F5's is preserved: a
// kubernetes.io or foregroundDeletion entry dropped here would leak whatever it
// was protecting (roksbnkctl's first version wrote `finalizers: null`).
func RetainNonF5Finalizers(in []string) (keep []string, removed int) {
	keep = []string{}
	for _, f := range in {
		if IsF5Finalizer(f) {
			removed++
			continue
		}
		keep = append(keep, f)
	}
	return keep, removed
}

// WebhookRefusal: an admission webhook could not be CALLED (its backend is gone).
// Transient — remove the webhook and retry.
func WebhookRefusal(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "failed calling webhook") ||
		(strings.Contains(m, "webhook") && strings.Contains(m, "no endpoints available"))
}

// WebhookDenial: an admission webhook deliberately REJECTED the request
// ("Default TCP parameters CR cannot be deleted!"). Permanent — respect it; the
// object goes with the namespace.
func WebhookDenial(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "admission webhook") && strings.Contains(m, "denied the request")
}
