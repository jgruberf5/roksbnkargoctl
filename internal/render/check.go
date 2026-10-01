package render

import (
	"fmt"
	"strings"
)

// The check container's home. It is created out of band (not by Argo CD) so it
// outlives the Application: the PostDelete hook runs here after Argo CD has
// deleted everything it owns.
const (
	CheckNamespace   = "roksbnkargoctl-check"
	CheckSA          = "check"
	CheckClusterRole = "roksbnkargoctl-check"
	ProbeDaemonSet   = "check-node-probe"
	SweepDeployment  = "check-gateway-api-sweep"
	CATrustDaemonSet = "registry-ca-trust"
	CATrustConfigMap = "registry-ca"
	AnnoRunID        = "roksbnkargoctl.io/run-id"
)

// ProbeTarget is one endpoint every node must reach before BNK is installed.
type ProbeTarget struct {
	Host     string
	Port     int
	TLS      bool
	Insecure bool // self-signed (FLP): handshake, report the subject, do not verify
	Why      string
}

// Flag renders the node-probe --target value.
func (t ProbeTarget) Flag() string {
	s := fmt.Sprintf("%s:%d", t.Host, t.Port)
	if t.TLS {
		s += ",tls"
	}
	if t.Insecure {
		s += ",insecure"
	}
	return s
}

type checkParams struct {
	Image          string
	PullSecret     string // in CheckNamespace, "" when not needed
	RunID          string
	BNKNamespace   string
	UtilsNamespace string
	TMMReplicas    int
	StorageClass   string
	Targets        []ProbeTarget
	RequireSecrets []string // ns/name
	LicenseArgs    []string
	ExtraDeleteNS  []string
	AppName        string
}

func (p checkParams) podSpec(args []string, extra map[string]any) map[string]any {
	c := map[string]any{
		"name": "check", "image": p.Image, "imagePullPolicy": "IfNotPresent",
		"args": toAny(args),
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []any{"ALL"}},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
			"limits":   map[string]any{"memory": "128Mi"},
		},
	}
	spec := map[string]any{
		"serviceAccountName": CheckSA,
		"containers":         []any{c},
		"securityContext":    map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
	}
	if p.PullSecret != "" {
		spec["imagePullSecrets"] = []any{map[string]any{"name": p.PullSecret}}
	}
	for k, v := range extra {
		if k == "env" {
			c["env"] = v
			continue
		}
		spec[k] = v
	}
	return spec
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// hookJob is one check mode run by Argo CD as a hook Job. BeforeHookCreation
// keeps a failed Job (and its logs) until the next sync, which is when an
// operator needs them.
func (p checkParams) hookJob(mode, hook string, wave int, args []string, deadline int) Object {
	o := Object{"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{
			"name": "check-" + mode, "namespace": CheckNamespace,
			"labels": map[string]any{LabelComponent: "check", "roksbnkargoctl.io/check-mode": mode},
		},
		"spec": map[string]any{
			"backoffLimit":          0,
			"activeDeadlineSeconds": deadline,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{LabelComponent: "check", "roksbnkargoctl.io/check-mode": mode}},
				"spec": func() map[string]any {
					s := p.podSpec(append([]string{mode}, args...), nil)
					s["restartPolicy"] = "Never"
					return s
				}(),
			},
		}}
	a := o.Annotations()
	a[AnnoHook] = hook
	a[AnnoHookDelete] = "BeforeHookCreation"
	if hook == "Sync" {
		o.SetWave(wave)
	}
	return o
}

func (p checkParams) nodeProbe(wave int) Object {
	args := []string{"node-probe", "--window=180s"}
	for _, t := range p.Targets {
		args = append(args, "--target="+t.Flag())
	}
	labels := map[string]any{"app": ProbeDaemonSet, LabelComponent: "check"}
	fieldEnv := func(name, path string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": path}}}
	}
	spec := p.podSpec(args, map[string]any{
		// The node's own network and resolver: kubelet pulls images from here, so
		// this is the vantage point that decides whether BNK can install.
		"hostNetwork": true,
		"dnsPolicy":   "Default",
		"tolerations": []any{map[string]any{"operator": "Exists"}},
		"env": []any{
			fieldEnv("NODE_NAME", "spec.nodeName"),
			fieldEnv("POD_NAME", "metadata.name"),
			fieldEnv("POD_NAMESPACE", "metadata.namespace"),
			fieldEnv("RUN_ID", "metadata.annotations['"+AnnoRunID+"']"),
		},
	})
	o := Object{"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"name": ProbeDaemonSet, "namespace": CheckNamespace, "labels": labels},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": ProbeDaemonSet}},
			"updateStrategy": map[string]any{"type": "RollingUpdate",
				"rollingUpdate": map[string]any{"maxUnavailable": "100%"}},
			"template": map[string]any{
				// A per-render run id rolls every pod, so pre-install never reads a
				// verdict a previous sync wrote (roksbnkctl #57).
				"metadata": map[string]any{"labels": labels, "annotations": map[string]any{AnnoRunID: p.RunID}},
				"spec":     spec,
			},
		}}
	o.SetWave(wave)
	return o
}

func (p checkParams) preInstallArgs() []string {
	args := []string{
		"--timeout=15m",
		"--probe-daemonset=" + ProbeDaemonSet, "--probe-namespace=" + CheckNamespace,
		"--run-id=" + p.RunID,
		"--bnk-namespace=" + p.BNKNamespace, "--utils-namespace=" + p.UtilsNamespace,
		fmt.Sprintf("--tmm-replicas=%d", p.TMMReplicas),
		"--storage-class=" + p.StorageClass,
		"--min-openshift=4.16",
		// Every sync re-runs the hook; FLO created by this same Application is a
		// re-sync, not a foreign install.
		"--argocd-app=" + p.AppName,
	}
	for _, s := range p.RequireSecrets {
		args = append(args, "--require-secret="+s)
	}
	return args
}

func (p checkParams) sweep(wave int) Object {
	labels := map[string]any{"app": SweepDeployment, LabelComponent: "check"}
	spec := p.podSpec([]string{"gateway-api-sweep", "--idle-after"}, nil)
	o := Object{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": SweepDeployment, "namespace": CheckNamespace, "labels": labels},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]any{"app": SweepDeployment}},
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": spec},
		}}
	o.SetWave(wave)
	return o
}

// caTrust installs a private mirror CA on every node before anything pulls from
// the mirror — including the check image — so it runs on an image every ROKS
// node already has cached (openshift-dns/node-resolver), IfNotPresent.
func caTrust(mirrorHost, caPEM, nodeImage string, wave int) []Object {
	host := strings.SplitN(mirrorHost, "/", 2)[0]
	cm := Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": CATrustConfigMap, "namespace": CheckNamespace},
		"data":     map[string]any{"ca.crt": caPEM}}
	cm.SetWave(wave)
	script := fmt.Sprintf(`for d in /host/etc/containers/certs.d /host/etc/docker/certs.d; do `+
		`mkdir -p "$d/%[1]s" && rm -f "$d/%[1]s/ca.crt" && install -m644 /ca/ca.crt "$d/%[1]s/ca.crt"; done; `+
		`echo "registry CA installed for %[1]s on $NODE_NAME"; sleep infinity`, host)
	labels := map[string]any{"app": CATrustDaemonSet, LabelComponent: "check"}
	priv := true
	ds := Object{"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"name": CATrustDaemonSet, "namespace": CheckNamespace, "labels": labels},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": CATrustDaemonSet}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"serviceAccountName": CheckSA,
					"tolerations":        []any{map[string]any{"operator": "Exists"}},
					"containers": []any{map[string]any{
						"name": "installer", "image": nodeImage, "imagePullPolicy": "IfNotPresent",
						"command":         []any{"/bin/sh", "-c"},
						"args":            []any{script},
						"securityContext": map[string]any{"privileged": priv},
						"env": []any{map[string]any{"name": "NODE_NAME",
							"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "spec.nodeName"}}}},
						"volumeMounts": []any{
							map[string]any{"name": "hostetc", "mountPath": "/host/etc"},
							map[string]any{"name": "ca", "mountPath": "/ca"},
						},
						"resources": map[string]any{"requests": map[string]any{"cpu": "5m", "memory": "16Mi"}},
					}},
					"volumes": []any{
						map[string]any{"name": "hostetc", "hostPath": map[string]any{"path": "/etc"}},
						map[string]any{"name": "ca", "configMap": map[string]any{"name": CATrustConfigMap}},
					},
				},
			},
		}}
	ds.SetWave(wave)
	return []Object{cm, ds}
}

// checkRBAC is created out of band with the check namespace. It is broad on the
// F5 API groups (the uninstall checks delete whatever F5 CRs exist) and narrow
// elsewhere.
// checkRBAC is the check's namespace, ServiceAccount and cluster RBAC.
// singleCert adds creating Secrets: the cert mode writes the single
// certificate into the BNK namespaces (patch alone cannot create one).
func checkRBAC(singleCert bool) []Object {
	nsObj := Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": CheckNamespace,
		"labels": map[string]any{LabelManagedBy: ManagedByValue}}}
	sa := Object{"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata": map[string]any{"name": CheckSA, "namespace": CheckNamespace}}
	rule := func(groups, resources, verbs []string) map[string]any {
		return map[string]any{"apiGroups": toAny(groups), "resources": toAny(resources), "verbs": toAny(verbs)}
	}
	read := []string{"get", "list", "watch"}
	all := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	role := Object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": CheckClusterRole},
		"rules": []any{
			rule([]string{""}, []string{"nodes", "namespaces", "pods", "services", "events", "persistentvolumeclaims", "serviceaccounts", "configmaps", "secrets"}, read),
			rule([]string{""}, []string{"pods", "secrets", "configmaps", "services", "serviceaccounts", "persistentvolumeclaims"}, []string{"patch", "delete"}),
			rule([]string{""}, []string{"namespaces"}, []string{"delete", "patch"}),
			rule([]string{""}, []string{"namespaces/finalize"}, []string{"update"}),
			rule([]string{"apps"}, []string{"deployments", "daemonsets", "statefulsets", "replicasets", "controllerrevisions"}, read),
			rule([]string{"apps"}, []string{"deployments", "daemonsets", "statefulsets"}, []string{"patch"}),
			rule([]string{"batch"}, []string{"jobs"}, read),
			rule([]string{"storage.k8s.io"}, []string{"storageclasses"}, read),
			rule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, read),
			rule([]string{"config.openshift.io"}, []string{"clusterversions"}, read),
			rule([]string{"admissionregistration.k8s.io"}, []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings", "validatingwebhookconfigurations"}, []string{"get", "list", "delete"}),
			rule([]string{"cert-manager.io"}, []string{"certificates", "clusterissuers", "issuers"}, read),
			// cert-manager-ready dry-run creates a ClusterIssuer (creates nothing,
			// but the API server authorizes it as a create).
			rule([]string{"cert-manager.io"}, []string{"clusterissuers"}, []string{"create"}),
			rule([]string{"k8s.f5.com", "k8s.f5net.com", "gateway.k8s.f5.com", "fic.f5.com", "metrics.f5.com"}, []string{"*"}, all),
		}}
	if singleCert {
		role["rules"] = append(role["rules"].([]any), rule([]string{""}, []string{"secrets"}, []string{"create"}))
	}
	bind := Object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": CheckClusterRole},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": CheckClusterRole},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": CheckSA, "namespace": CheckNamespace}}}
	// hostNetwork for the node probe and hostPath for CA trust need the privileged
	// SCC on OpenShift.
	scc := sccBinding(CheckNamespace, CheckSA, 0)
	delete(scc.Annotations(), AnnoWave)
	delete(scc.meta(), "annotations")
	return []Object{nsObj, sa, role, bind, scc}
}
