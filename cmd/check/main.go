// Command check is the roksbnkargoctl in-cluster check binary. It runs only in
// ROKS, as Argo CD hook Jobs, a DaemonSet (node-probe) and a Deployment
// (gateway-api-sweep), with the pod's ServiceAccount. Standard library only, so
// the image is one static binary on scratch.
//
// Every mode logs for humans on stderr and prints one JSON result line on
// stdout; exit status 0 is pass, 1 is fail, 2 is a usage error.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/checks"
	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: check <mode> [flags]

modes:
  pre-install        cluster prerequisites + node-probe verdicts (Sync hook, wave -18)
  node-probe         DNS/TCP/TLS from this node; publishes a pod annotation (DaemonSet)
  gateway-api-sweep  removes OpenShift's Gateway API CRD admission policy until the CRDs exist (Deployment)
  license            builds License from the JWT Secret; waits Active, then CNEInstance Available (Sync hook, wave 0)
  post-install       verifies FLO, CNEInstance, License, TMM, pod health (PostSync hook)
  pre-uninstall      drains F5 CRs while FLO runs; CNEInstance last (PreDelete hook)
  post-uninstall     CWC secrets, namespaces, stuck F5 finalizers (PostDelete hook)
  version            prints the version

Every flag can also be set from the environment as CHECK_<FLAG> (upper case,
dashes as underscores), e.g. --bnk-namespace = CHECK_BNK_NAMESPACE.
Run "check <mode> -h" for a mode's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	mode, rest := args[0], args[1:]
	switch mode {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	m, ok := modes[mode]
	if !ok {
		fmt.Fprintf(stderr, "unknown mode %q\n\n%s", mode, usage)
		return 2
	}
	exec := m(fs)
	applyEnvDefaults(fs)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	kc, err := kube.InCluster()
	if err != nil {
		res := checks.NewResult(mode, stderr)
		res.Version = version
		fmt.Fprintln(stdout, string(res.Finish(err)))
		return 1
	}
	env := &checks.Env{Kube: kc, Log: stderr}
	res := checks.NewResult(mode, stderr)
	res.Version = version
	idle, err := exec(ctx, env, res)
	fmt.Fprintln(stdout, string(res.Finish(err)))
	if res.Failed() {
		why := res.Failures()
		if res.Error != "" {
			why = append(why, res.Error)
		}
		fmt.Fprintf(stderr, "%s FAILED:\n  %s\n", mode, strings.Join(why, "\n  "))
		// Always exit on failure, idle mode or not: a node-probe that could not
		// publish restarts and tries again rather than idling unseen.
		return 1
	}
	fmt.Fprintf(stderr, "%s passed\n", mode)
	if idle {
		// DaemonSet / Deployment: stay up (an exiting container restarts forever).
		fmt.Fprintf(stderr, "idling until terminated\n")
		<-ctx.Done()
	}
	return 0
}

// modeFunc registers a mode's flags and returns its runner. The runner reports
// whether the process should idle afterwards.
type modeFunc func(fs *flag.FlagSet) func(ctx context.Context, env *checks.Env, res *checks.Result) (idle bool, err error)

var modes = map[string]modeFunc{
	"pre-install":       preInstall,
	"node-probe":        nodeProbe,
	"gateway-api-sweep": gatewaySweep,
	"license":           license,
	"post-install":      postInstall,
	"pre-uninstall":     preUninstall,
	"post-uninstall":    postUninstall,
}

// listFlag is a repeatable flag; each value may also be comma-separated.
type listFlag struct {
	vals  []string
	set   bool
	split func(string) []string
}

func (l *listFlag) String() string { return strings.Join(l.vals, ",") }
func (l *listFlag) Set(s string) error {
	if !l.set { // the first explicit value replaces an env/default list
		l.vals, l.set = nil, true
	}
	sp := l.split
	if sp == nil {
		sp = func(s string) []string { return strings.Split(s, ",") }
	}
	for _, v := range sp(s) {
		if v = strings.TrimSpace(v); v != "" {
			l.vals = append(l.vals, v)
		}
	}
	return nil
}

// applyEnvDefaults sets every flag from CHECK_<FLAG> when present; command-line
// values parsed afterwards override it.
func applyEnvDefaults(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		name := "CHECK_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(name); ok {
			if lf, isList := f.Value.(*listFlag); isList {
				_ = lf.Set(v)
				lf.set = false // command-line values still replace env ones
				return
			}
			_ = f.Value.Set(v)
		}
	})
}

func envOr(keys []string, def string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func namespaces(fs *flag.FlagSet) (bnk, utils *string) {
	bnk = fs.String("bnk-namespace", checks.DefaultFLONamespace, "namespace of FLO and the CNEInstance")
	utils = fs.String("utils-namespace", checks.DefaultUtilsNamespace, "namespace of BNK's shared components (CWC, License)")
	return
}

func preInstall(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	bnk, utils := namespaces(fs)
	minOCP := fs.String("min-openshift", "4.16", "minimum OpenShift version")
	minZones := fs.Int("min-zones", 3, "minimum zones the schedulable workers must span")
	minWorkers := fs.Int("min-workers", 3, "minimum schedulable Ready workers")
	secrets := &listFlag{}
	fs.Var(secrets, "require-secret", "namespace/name of a Secret that must exist (repeatable, or comma-separated)")
	tmm := fs.Int("tmm-replicas", 1, "TMM replicas; >1 requires a ReadWriteMany StorageClass")
	sc := fs.String("storage-class", "", "StorageClass TMM uses (empty = the default class)")
	app := fs.String("argocd-app", "", "Argo CD Application name; its own objects do not count as a pre-existing BNK on a re-sync")
	ds := fs.String("probe-daemonset", "check-node-probe", "node-probe DaemonSet name")
	pns := fs.String("probe-namespace", envOr([]string{"POD_NAMESPACE"}, kube.PodNamespace()), "node-probe DaemonSet namespace (default: this pod's)")
	runID := fs.String("run-id", os.Getenv("RUN_ID"), "only accept probe results carrying this run id (env RUN_ID)")
	skip := fs.Bool("skip-probe", false, "do not wait for node-probe results")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for every node-probe pod to report")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		return false, checks.PreInstall(ctx, env, checks.PreInstallConfig{
			BNKNamespace: *bnk, UtilsNamespace: *utils, MinOpenShift: *minOCP, MinZones: *minZones, MinWorkers: *minWorkers,
			RequiredSecrets: secrets.vals, TMMReplicas: *tmm, StorageClass: *sc, ArgoApp: *app,
			ProbeDaemonSet: *ds, ProbeNamespace: *pns, RunID: *runID, SkipProbe: *skip, Timeout: *timeout,
		}, res)
	}
}

func nodeProbe(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	targets := &listFlag{split: func(s string) []string {
		// One target per flag; an env value holds several separated by spaces or
		// semicolons (a comma is the option separator inside a target).
		return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ' ' || r == '\n' })
	}}
	fs.Var(targets, "target", "host:port[,tls][,insecure] to probe (repeatable; env: space/semicolon separated)")
	window := fs.Duration("window", 180*time.Second, "keep retrying a failing target this long (transit gateway routes program asynchronously)")
	interval := fs.Duration("interval", 10*time.Second, "between attempts")
	dialTO := fs.Duration("dial-timeout", 6*time.Second, "per-attempt DNS/TCP/TLS timeout")
	insecure := fs.Bool("insecure-skip-verify", false, "do not verify any TLS chain (subjects are still reported)")
	caFile := fs.String("ca-file", "", "extra PEM roots to trust (e.g. a private mirror CA)")
	runID := fs.String("run-id", os.Getenv("RUN_ID"), "run id recorded in the result (env RUN_ID)")
	node := fs.String("node-name", os.Getenv("NODE_NAME"), "this node (env NODE_NAME, downward API)")
	pod := fs.String("pod-name", os.Getenv("POD_NAME"), "this pod (env POD_NAME, downward API)")
	podNS := fs.String("pod-namespace", envOr([]string{"POD_NAMESPACE"}, kube.PodNamespace()), "this pod's namespace (env POD_NAMESPACE)")
	idle := fs.Bool("idle", true, "after publishing, sleep until terminated (DaemonSet)")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		var ts []checks.ProbeTarget
		for _, s := range targets.vals {
			t, err := checks.ParseTarget(s)
			if err != nil {
				return false, err
			}
			ts = append(ts, t)
		}
		p := checks.Prober{DialTimeout: *dialTO, Window: *window, Interval: *interval, ForceNoCheck: *insecure}
		if *caFile != "" {
			pem, err := os.ReadFile(*caFile)
			if err != nil {
				return false, err
			}
			p.ExtraRoots = x509.NewCertPool()
			if !p.ExtraRoots.AppendCertsFromPEM(pem) {
				return false, fmt.Errorf("%s holds no PEM certificate", *caFile)
			}
		}
		err := checks.NodeProbe(ctx, env, checks.NodeProbeConfig{Targets: ts, Prober: p, RunID: *runID, NodeName: *node, PodName: *pod, PodNamespace: *podNS}, res)
		return *idle, err
	}
}

func gatewaySweep(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	interval := fs.Duration("interval", 5*time.Second, "between sweeps")
	timeout := fs.Duration("timeout", 20*time.Minute, "give up after this long")
	idle := fs.Bool("idle-after", true, "after the CRDs exist, sleep until terminated instead of exiting (Deployment)")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		err := checks.GatewayAPISweep(ctx, env, checks.GatewaySweepConfig{Interval: *interval, Timeout: *timeout}, res)
		// Idle only on success: a failed sweep exits 1 and the Deployment
		// restarts it, which keeps sweeping — the right thing while FLO is late.
		return *idle && err == nil && !res.Failed(), err
	}
}

func license(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	ns := fs.String("namespace", checks.DefaultUtilsNamespace, "License namespace")
	cneNS := fs.String("cne-namespace", checks.DefaultFLONamespace, "CNEInstance namespace")
	name := fs.String("name", "bnk-license", "License name")
	secret := fs.String("jwt-secret", "bnk-license-jwt", "Secret holding the subscription JWT")
	secretNS := fs.String("jwt-secret-namespace", envOr([]string{"POD_NAMESPACE"}, kube.PodNamespace()), "namespace of --jwt-secret (default: this pod's)")
	key := fs.String("jwt-key", "jwt", "key of the JWT in --jwt-secret")
	mode := fs.String("mode", checks.ModeConnected, "connected | f5licenseproxy")
	flp := fs.String("flp-url", "", "F5 License Proxy base URL, e.g. https://10.0.0.5:8443 (f5licenseproxy mode)")
	caPath := fs.String("flp-ca-path", checks.DefaultFLPCAPath, "licenseProxyServerRootCaPath")
	restart := fs.Bool("restart-cwc", true, "f5licenseproxy: roll CWC (once per CA) so it trusts the FLP root CA")
	crdTO := fs.Duration("crd-timeout", 10*time.Minute, "wait for the License CRD")
	applyTO := fs.Duration("apply-retry", 5*time.Minute, "keep retrying a refused apply (ResourceQuota race)")
	timeout := fs.Duration("timeout", 15*time.Minute, "wait for License Active")
	cneTO := fs.Duration("cne-timeout", 15*time.Minute, "wait for CNEInstance Available")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		return false, checks.License(ctx, env, checks.LicenseConfig{
			Namespace: *ns, CNENamespace: *cneNS, Name: *name, JWTSecret: *secret, JWTSecretNamespace: *secretNS, JWTKey: *key,
			Mode: *mode, FLPURL: *flp, FLPCAPath: *caPath, RestartCWC: *restart,
			CRDTimeout: *crdTO, ApplyRetry: *applyTO, LicenseTimeout: *timeout, CNETimeout: *cneTO,
		}, res)
	}
}

func postInstall(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	bnk, utils := namespaces(fs)
	tmm := fs.Int("tmm-replicas", 0, "expected Ready TMM pods (0 = at least one)")
	sel := fs.String("tmm-selector", "app=f5-tmm", "label selector of TMM pods")
	tmmNS := fs.String("tmm-namespace", "", "namespace of TMM pods (default --bnk-namespace)")
	timeout := fs.Duration("timeout", 10*time.Minute, "keep re-checking until everything passes or this expires")
	interval := fs.Duration("interval", 15*time.Second, "time between attempts")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		cfg := checks.PostInstallConfig{
			BNKNamespace: *bnk, UtilsNamespace: *utils, TMMReplicas: *tmm, TMMSelector: *sel, TMMNamespace: *tmmNS,
		}
		// PostSync runs the moment the license hook passes, while TMM may still
		// be settling: retry quietly, then report the final attempt in full.
		deadline := time.Now().Add(*timeout)
		for attempt := 1; ; attempt++ {
			try := checks.NewResult(res.Mode, io.Discard)
			err := checks.PostInstall(ctx, env, cfg, try)
			if (err == nil && !try.Failed()) || time.Now().Add(*interval).After(deadline) {
				break
			}
			fmt.Fprintf(env.Log, "attempt %d not yet healthy: %s; retrying in %s\n", attempt, strings.Join(try.Failures(), "; "), *interval)
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(*interval):
			}
		}
		return false, checks.PostInstall(ctx, env, cfg, res)
	}
}

func preUninstall(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	bnk, utils := namespaces(fs)
	groups := &listFlag{vals: append([]string(nil), checks.DefaultDrainGroups...)}
	fs.Var(groups, "drain-group", "API group drained before the CNEInstance (repeatable; replaces the default "+strings.Join(checks.DefaultDrainGroups, ",")+")")
	timeout := fs.Duration("timeout", 4*time.Minute, "drain budget per namespace")
	grace := fs.Duration("refusal-grace", 45*time.Second, "how long every-delete-refused must persist before giving up")
	licTO := fs.Duration("license-timeout", 3*time.Minute, "wait for the License to go")
	cneTO := fs.Duration("cne-timeout", 10*time.Minute, "wait for the CNEInstance to go")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		return false, checks.PreUninstall(ctx, env, checks.PreUninstallConfig{
			BNKNamespace: *bnk, UtilsNamespace: *utils, DrainGroups: groups.vals, DrainTimeout: *timeout,
			RefusalGrace: *grace, LicenseTimeout: *licTO, CNETimeout: *cneTO,
		}, res)
	}
}

func postUninstall(fs *flag.FlagSet) func(context.Context, *checks.Env, *checks.Result) (bool, error) {
	bnk, utils := namespaces(fs)
	extra := &listFlag{}
	fs.Var(extra, "delete-namespace", "extra namespace to delete and wait for (repeatable), e.g. cert-manager")
	timeout := fs.Duration("timeout", 5*time.Minute, "wait for the namespaces to go")
	grace := fs.Duration("finalizer-grace", 30*time.Second, "let a namespace terminate on its own this long before stripping F5 finalizers")
	return func(ctx context.Context, env *checks.Env, res *checks.Result) (bool, error) {
		return false, checks.PostUninstall(ctx, env, checks.PostUninstallConfig{
			BNKNamespace: *bnk, UtilsNamespace: *utils, DeleteNamespaces: extra.vals, Timeout: *timeout, FinalizerGrace: *grace,
		}, res)
	}
}
