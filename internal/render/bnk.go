package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
)

// Fixed names. They match what roksbnkctl installs, so the operator guides,
// the check container and a roksbnkctl-installed cluster all agree.
const (
	ClusterIssuerSelfSigned = "selfsigned-cluster-issuer"
	ClusterIssuerCA         = "sample-issuer"
	CACertName              = "ext-ca"
	NADName                 = "ens3-ipvlan-l2"
	LicenseName             = "bnk-license"
	LicenseJWTSecret        = "bnk-license-jwt"
	FLPRootCASecret         = "licenseserver-rootca"
	FLPRootCAKey            = "licenseserver-rootca.txt"
	FLPRootCAPath           = "/etc/cm20/licenseserver-rootca/licenseserver-rootca.txt"
	GatewayAPIVersion       = "1.5.0"
	TMMK8sRoutes            = "172.17.0.0/18"
	// CNEControllerSA is the ServiceAccount the IAM trusted profile links to on
	// 2.4. A wrong name here fails silently: status stays green, the data path
	// never comes up (roksbnkctl, verified live).
	CNEControllerSA = "f5-cne-controller"
)

// CNEManifestName is the name FLO itself would mint (lower("BNK-<version>")), so
// a CR left by an earlier install is overwritten rather than duplicated — two CRs
// for one version trip FLO's MultipleMatching guard.
func CNEManifestName(version string) string { return strings.ToLower("BNK-" + version) }

func ns(name string, wave int, extraLabels map[string]string) Object {
	o := Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}
	o.SetWave(wave)
	// Argo CD must not delete the BNK namespaces itself: a namespace stuck on an
	// F5 finalizer would hang the Application's deletion before the PostDelete
	// check ever ran. `check post-uninstall` deletes them, finalizers handled.
	o.Annotations()[AnnoSyncOptions] = "Delete=false"
	for k, v := range extraLabels {
		o.Labels()[k] = v
	}
	return o
}

func certChain(wave int) []Object {
	self := Object{"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer",
		"metadata": map[string]any{"name": ClusterIssuerSelfSigned},
		"spec":     map[string]any{"selfSigned": map[string]any{}}}
	self.SetWave(wave)
	ca := Object{"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": CACertName, "namespace": "cert-manager"},
		"spec": map[string]any{
			"isCA": true, "commonName": CACertName, "secretName": CACertName,
			"privateKey": map[string]any{"algorithm": "ECDSA", "size": 256},
			"issuerRef":  map[string]any{"name": ClusterIssuerSelfSigned, "kind": "ClusterIssuer", "group": "cert-manager.io"},
		}}
	ca.SetWave(wave + 1)
	issuer := Object{"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer",
		"metadata": map[string]any{"name": ClusterIssuerCA},
		"spec":     map[string]any{"ca": map[string]any{"secretName": CACertName}}}
	issuer.SetWave(wave + 2)
	return []Object{self, ca, issuer}
}

func nad(namespace, address string, wave int) Object {
	cfg, _ := json.Marshal(map[string]any{
		"cniVersion": "0.3.1", "type": "ipvlan", "master": "ens3", "mode": "l2",
		"ipam": map[string]any{"type": "static", "addresses": []any{map[string]any{"address": address}}},
	})
	o := Object{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
		"metadata": map[string]any{"name": NADName, "namespace": namespace},
		"spec":     map[string]any{"config": string(cfg)}}
	o.SetWave(wave)
	return o
}

// sccBinding grants an SA OpenShift's privileged SCC, named the way roksbnkctl
// names it so the two tools recognise each other's objects.
func sccBinding(namespace, sa string, wave int) Object {
	o := Object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "system:openshift:scc:privileged:" + namespace + ":" + sa},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "system:openshift:scc:privileged"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": namespace}}}
	o.SetWave(wave)
	return o
}

func cneManifest(m *far.Manifest, wave int) Object {
	var images, charts []any
	for _, i := range m.Images {
		images = append(images, map[string]any{"name": i.Short(), "version": i.Version})
	}
	for _, c := range m.Charts {
		charts = append(charts, map[string]any{"name": c.Short(), "version": c.Version})
	}
	o := Object{"apiVersion": "k8s.f5.com/v1", "kind": "CNEManifest",
		"metadata": map[string]any{"name": CNEManifestName(m.Version)},
		"spec":     map[string]any{"version": m.Version, "images": images, "charts": charts}}
	o.SetWave(wave)
	o.Annotations()[AnnoSyncOptions] = "SkipDryRunOnMissingResource=true"
	return o
}

func env(pairs ...string) []any {
	var out []any
	for i := 0; i+1 < len(pairs); i += 2 {
		e := map[string]any{"name": pairs[i]}
		if pairs[i+1] != "" {
			// An empty value is omitted, as the API server stores it; a
			// rendered "" reads as a permanent diff in Argo CD.
			e["value"] = pairs[i+1]
		}
		out = append(out, e)
	}
	return out
}

// CNEInstanceParams are the cluster facts the CR needs.
type CNEInstanceParams struct {
	Namespace        string
	ImageHost        string
	PullSecret       string // "" when the registry needs none
	Region           string
	VPCName          string
	TrustedProfileID string
	TMMReplicas      int
	StorageClass     string
	Version          string
	CertManager      bool // spec.certificate names the cert-manager issuer; absent in single-certificate mode
}

// CNEInstanceName is the CNEInstance's name in namespace ns. The check binary
// derives the same name from the namespace it is given
// (cmd/check/internal/checks.CNEInstanceName); a constant here once named it
// f5-bnk-f5-cne-controller in every namespace, so any bnk.namespace but
// f5-bnk left the license hook waiting for a CR that did not exist (#11).
func CNEInstanceName(ns string) string { return ns + "-f5-cne-controller" }

// cneInstance builds the 2.4 CNEInstance exactly as roksbnkctl's verified 2.4
// install renders it (terraform/modules/cne_instance, 2.4 branch): Tiny +
// demoMode off (stock ROKS workers have zero hugepages), wholeCluster false with
// watchNamespaces [All], gatewayAPI on with USE_GATEWAY_SETTINGS, and the
// reference TMM placement.
//
// Every "off" is rendered as the field's absence (an empty object where the
// parent is kept): false is each field's default, and FLO rewrites the spec
// without false bools, so a rendered `enabled: false` left the CNEInstance
// OutOfSync in Argo CD straight after a successful sync (seen live on bnkargo).
func cneInstance(p CNEInstanceParams, wave int) Object {
	var pullSecrets []any
	if p.PullSecret != "" {
		pullSecrets = []any{map[string]any{"name": p.PullSecret}}
	}
	tmmSelector := map[string]any{"matchLabels": map[string]any{"app": "f5-tmm"}}
	spec := map[string]any{
		"product":         map[string]any{"gatewayAPI": true, "type": "BNK"},
		"manifestVersion": p.Version,
		"telemetry": map[string]any{
			"loggingSubsystem": map[string]any{"enabled": true},
			"metricSubsystem":  map[string]any{"enabled": true},
		},
		"deploymentSize": "Tiny",
		"registry": map[string]any{
			"uri": p.ImageHost, "imagePullSecrets": pullSecrets, "imagePullPolicy": "Always",
		},
		"networkAttachments": []any{NADName},
		"dynamicRouting":     map[string]any{},
		"firewallACL":        map[string]any{"enabled": true},
		"pseudoCNI":          map[string]any{"enabled": true},
		"coreCollection":     map[string]any{"enabled": true},
		"advanced": map[string]any{
			"coremon":      map[string]any{"hostPath": true, "env": env("COREMOND_OVERRIDE_CORE_PATTERN", "true")},
			"envDiscovery": map[string]any{"stopOnFail": false, "runAfterSuccess": false},
			"cneController": map[string]any{"env": env(
				"TMM_DEFAULT_MTU", "9000",
				"CLOUD_ENV", "true",
				"CLOUD_PROVIDER", "ibm",
				"CLOUD_REGION", p.Region,
				"GSLB_DATACENTER_NAME", "",
				"CLOUD_VPC", p.VPCName,
				"CLOUD_TRUSTED_PROFILE", p.TrustedProfileID,
				"USE_GATEWAY_SETTINGS", "true",
				"GATEWAY_API_VERSION", GatewayAPIVersion,
			)},
			"demoMode":        map[string]any{},
			"maintenanceMode": map[string]any{},
			"tmm": map[string]any{
				"env": env(
					"TMM_CALICO_ROUTER", "default",
					"TMM_DEFAULT_MTU", "9000",
					"PAL_CPU_SET", "0,2",
					"TMM_MAPRES_ADDL_VETHS_ON_DP", "TRUE",
					"TMM_K8S_ROUTES", TMMK8sRoutes,
					"TMM_IGNORE_GATEWAYS", "true",
					"DISABLE_HT", "true",
					"ENABLE_K8S_ROUTES", "true",
				),
				"rollingUpdate": map[string]any{"maxSurge": 0, "maxUnavailable": 1},
			},
			"pseudoCNI": map[string]any{"env": env("DISABLE_CHECKSUM_OFFLOAD", "true")},
		},
		"tmmReplicas":     p.TMMReplicas,
		"watchNamespaces": []any{"All"},
		"externalBigip":   map[string]any{},
		"placement": map[string]any{"dataPlane": map[string]any{
			"affinity": map[string]any{"podAntiAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": []any{
					map[string]any{"labelSelector": tmmSelector, "topologyKey": "kubernetes.io/hostname"},
				},
			}},
			"topologySpreadConstraints": []any{map[string]any{
				"labelSelector": tmmSelector, "maxSkew": 1,
				"topologyKey": "topology.kubernetes.io/zone", "whenUnsatisfiable": "DoNotSchedule",
			}},
		}},
	}
	if p.StorageClass != "" {
		spec["storageClassName"] = p.StorageClass
	}
	if p.CertManager {
		// Single-certificate mode leaves it out: FLO ignores it there (F5).
		spec["certificate"] = map[string]any{"clusterIssuer": ClusterIssuerCA}
	}
	o := Object{"apiVersion": "k8s.f5.com/v1", "kind": "CNEInstance",
		"metadata": map[string]any{
			"name": CNEInstanceName(p.Namespace), "namespace": p.Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/name": "f5-lifecycle-operator", "app.kubernetes.io/managed-by": "kustomize",
			},
		},
		"spec": spec}
	o.SetWave(wave)
	o.Annotations()[AnnoSyncOptions] = "SkipDryRunOnMissingResource=true"
	return o
}

// floValues are the FLO chart values roksbnkctl installs with, plus skipCertMgr
// (the chart `lookup`s cert-manager's CRDs and fails when it cannot see them,
// which is always the case when rendering offline).
func floValues(c *config.Config, pullSecret string) map[string]any {
	var ips []any
	if pullSecret != "" {
		ips = []any{map[string]any{"name": pullSecret}}
	}
	repo := c.ImageHost() + "/images"
	certmgr := map[string]any{"clusterIssuer": ClusterIssuerCA}
	if !c.UsesCertManager() {
		// F5's single certificate: no cert-manager Certificates; FLO mounts
		// this one Secret in their place in every component.
		certmgr = map[string]any{"enabled": false, "secretName": c.BNK.Certificates.SecretName}
	}
	return map[string]any{
		"global": map[string]any{
			"imagePullSecrets": ips,
			"certmgr":          certmgr,
		},
		"skipCertMgr": true,
		// Explicit, though they are the chart's defaults: uninstall relies on
		// helm.sh/resource-policy: keep to leave FLO's CRDs (Argo CD honours it).
		"crds":                     map[string]any{"enabled": true, "keep": true},
		"namespace":                c.BNK.Namespace,
		"containerPlatform":        "IBM",
		"sharedComponentNamespace": c.BNK.UtilsNamespace,
		"image":                    map[string]any{"repository": repo, "pullPolicy": "Always"},
		"f5-ipam-operator": map[string]any{
			"image":            map[string]any{"repository": repo, "pullPolicy": "Always"},
			"namespace":        c.BNK.Namespace,
			"nameOverride":     "f5-ipam-operator",
			"fullnameOverride": "f5-ipam-operator",
		},
	}
}

// certManagerValues redirect every cert-manager component image at the mirror
// (overriding only the controller leaves the others on quay.io — roksbnkctl
// learned that in its air-gap sprint).
func certManagerValues(c *config.Config, pullSecret string) map[string]any {
	// startupapicheck is a post-install Helm hook Job: Argo CD would run it as a
	// PostSync hook, one more image to pull for a check Argo CD's own Deployment
	// health already makes (the issuers wait for the webhook in a later wave).
	v := map[string]any{
		"crds":            map[string]any{"enabled": true, "keep": true},
		"startupapicheck": map[string]any{"enabled": false},
	}
	if c.Registry.Source == config.SourceMirror {
		base := c.ImageHost() + "/jetstack/cert-manager-"
		v["image"] = map[string]any{"repository": base + "controller"}
		v["webhook"] = map[string]any{"image": map[string]any{"repository": base + "webhook"}}
		v["cainjector"] = map[string]any{"image": map[string]any{"repository": base + "cainjector"}}
		v["acmesolver"] = map[string]any{"image": map[string]any{"repository": base + "acmesolver"}}
		if pullSecret != "" {
			v["global"] = map[string]any{"imagePullSecrets": []any{map[string]any{"name": pullSecret}}}
		}
	}
	return v
}

// licenseArgs are the flags the `check license` hook builds License from.
func licenseArgs(c *config.Config, flpURL string) []string {
	args := []string{"license",
		"--namespace=" + c.BNK.UtilsNamespace,
		"--cne-namespace=" + c.BNK.Namespace,
		"--jwt-secret=" + LicenseJWTSecret,
		"--jwt-secret-namespace=" + CheckNamespace,
	}
	if c.BNK.Mode == config.ModeDisconnected {
		args = append(args, "--mode=f5licenseproxy", "--flp-url="+strings.TrimSuffix(flpURL, "/"),
			"--flp-ca-path="+FLPRootCAPath)
	} else {
		args = append(args, "--mode=connected")
	}
	return args
}

// String form for error messages.
func (p CNEInstanceParams) String() string {
	return fmt.Sprintf("CNEInstance(ns=%s image=%s replicas=%d)", p.Namespace, p.ImageHost, p.TMMReplicas)
}
