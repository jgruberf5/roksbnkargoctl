package checks

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// ProbeAnnotation is where a node-probe pod publishes its verdict.
const ProbeAnnotation = "roksbnkargoctl.io/probe-result"

// Probe target and results.
//
// WHY FROM A NODE (roksbnkctl internal/k8s/node_reachability.go). The operator
// host sits on a VPC with Internet egress; the workers are behind a transit
// gateway. A probe run where the CLI runs returns a confident green for a mirror
// the cluster cannot route to. A DaemonSet is one pod per node, which on ROKS
// means every zone — and a TGW attachment or security group that is right in two
// zones and wrong in the third is exactly the failure this catches.

// ProbeTarget is one endpoint.
type ProbeTarget struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	TLS      bool   `json:"tls"`
	Insecure bool   `json:"insecure,omitempty"` // skip chain verification (self-signed FLP)
}

func (t ProbeTarget) String() string { return net.JoinHostPort(t.Host, t.Port) }

// ParseTarget reads `host:port[,tls][,insecure]`. A bare host defaults to 443.
// `https://host:port` is accepted and implies tls.
func ParseTarget(s string) (ProbeTarget, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	ep := strings.TrimSpace(parts[0])
	var t ProbeTarget
	if strings.HasPrefix(ep, "https://") {
		t.TLS = true
	}
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	if i := strings.Index(ep, "/"); i >= 0 {
		ep = ep[:i]
	}
	if ep == "" {
		return t, fmt.Errorf("empty probe target %q", s)
	}
	if h, p, err := net.SplitHostPort(ep); err == nil {
		t.Host, t.Port = h, p
	} else {
		t.Host, t.Port = strings.Trim(ep, "[]"), "443"
	}
	if n, err := strconv.Atoi(t.Port); err != nil || n < 1 || n > 65535 {
		return t, fmt.Errorf("probe target %q: bad port %q", s, t.Port)
	}
	for _, o := range parts[1:] {
		switch strings.ToLower(strings.TrimSpace(o)) {
		case "tls":
			t.TLS = true
		case "insecure":
			t.TLS, t.Insecure = true, true
		case "":
		default:
			return t, fmt.Errorf("probe target %q: unknown option %q (want tls or insecure)", s, o)
		}
	}
	return t, nil
}

// TargetResult is one node's verdict for one target.
type TargetResult struct {
	Target      string   `json:"target"`
	TLS         bool     `json:"tls"`
	OK          bool     `json:"ok"`
	DNS         string   `json:"dns"` // ok | FAILED | skipped-ip
	Addrs       []string `json:"addrs,omitempty"`
	TCP         string   `json:"tcp"`                   // ok | FAILED | skipped
	Handshake   string   `json:"handshake,omitempty"`   // ok | FAILED (tls only)
	Verified    string   `json:"verified,omitempty"`    // yes | no: <why> | skipped (insecure)
	CertSubject string   `json:"certSubject,omitempty"` // leaf subject, reported even when unverified
	CertIssuer  string   `json:"certIssuer,omitempty"`
	Attempts    int      `json:"attempts"`
	Elapsed     float64  `json:"elapsedSeconds"`
	Error       string   `json:"error,omitempty"`
}

// ProbeReport is what one node-probe pod writes to its annotation.
type ProbeReport struct {
	RunID   string         `json:"runId,omitempty"`
	Node    string         `json:"node"`
	Pod     string         `json:"pod,omitempty"`
	Time    time.Time      `json:"time"`
	OK      bool           `json:"ok"`
	Targets []TargetResult `json:"targets"`
}

// Prober does the network work; fields are injectable for tests.
type Prober struct {
	Resolver     *net.Resolver
	DialTimeout  time.Duration
	Window       time.Duration // keep retrying a failing target this long
	Interval     time.Duration // between attempts
	ExtraRoots   *x509.CertPool
	ForceNoCheck bool // --insecure-skip-verify for every TLS target
	// Dial replaces the TCP dialer (tests); nil uses net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// ProbeOnce tries a target exactly once.
func (p *Prober) ProbeOnce(ctx context.Context, t ProbeTarget) TargetResult {
	r := TargetResult{Target: t.String(), TLS: t.TLS, DNS: "skipped-ip", TCP: "FAILED"}
	res := p.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	dialTO := p.DialTimeout
	if dialTO <= 0 {
		dialTO = 6 * time.Second
	}
	addr := t.Host
	// DNS is reported separately from TCP: they fail for different reasons and
	// want different fixes (resolver vs routing/security groups/TGW).
	if net.ParseIP(t.Host) == nil {
		lctx, cancel := context.WithTimeout(ctx, dialTO)
		addrs, err := res.LookupHost(lctx, t.Host)
		cancel()
		if err != nil || len(addrs) == 0 {
			r.DNS = "FAILED"
			r.TCP = "skipped"
			r.Error = fmt.Sprintf("name does not resolve from this node: %v", err)
			return r
		}
		r.DNS = "ok"
		r.Addrs = addrs
		addr = addrs[0]
	}
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTO}).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(addr, t.Port))
	if err != nil {
		r.Error = "tcp connect failed (refused, filtered, or no route): " + err.Error()
		return r
	}
	defer conn.Close()
	r.TCP = "ok"
	if !t.TLS {
		r.OK = true
		return r
	}
	// The handshake always completes WITHOUT verification first, so "TLS works
	// but the chain is not trusted" is reported as that, with the subject, rather
	// than collapsed into a handshake failure that hides who answered.
	_ = conn.SetDeadline(time.Now().Add(dialTO + 4*time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: sniName(t.Host), InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // verified by hand below
	if err := tc.HandshakeContext(ctx); err != nil {
		r.Handshake = "FAILED"
		r.Error = "tls handshake failed (the port answers TCP but does not speak TLS, or rejected us): " + err.Error()
		return r
	}
	r.Handshake = "ok"
	st := tc.ConnectionState()
	if len(st.PeerCertificates) > 0 {
		r.CertSubject = st.PeerCertificates[0].Subject.String()
		r.CertIssuer = st.PeerCertificates[0].Issuer.String()
	}
	if t.Insecure || p.ForceNoCheck {
		r.Verified = "skipped (insecure)"
		r.OK = true
		return r
	}
	if err := p.verify(st.PeerCertificates, t.Host); err != nil {
		r.Verified = "no"
		r.Error = "certificate not trusted: " + err.Error() + " (mark the target ,insecure for a self-signed FLP, or supply --ca-file)"
		return r
	}
	r.Verified = "yes"
	r.OK = true
	return r
}

func sniName(h string) string {
	if net.ParseIP(h) != nil {
		return "" // SNI must not carry an IP literal
	}
	return h
}

func (p *Prober) verify(certs []*x509.Certificate, host string) error {
	if len(certs) == 0 {
		return errors.New("no certificate presented")
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if p.ExtraRoots != nil {
		// Both pools must count; x509 cannot union pools, so verify twice.
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		if _, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Roots: p.ExtraRoots, Intermediates: inter}); err == nil {
			return nil
		}
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inter})
	return err
}

// Probe retries a target until it succeeds or the window is spent.
//
// WHY IT RETRIES (roksbnkctl #57). A transit gateway attachment is asynchronous:
// routes are programmed some time after it reports attached. One probe ~73s
// after attach saw both targets unreachable while the path was healthy minutes
// later. "Not reachable" and "not reachable YET" are different claims, and a
// single failure cannot tell them apart. Success ends the loop at once, so a
// healthy path costs one attempt.
func (p *Prober) Probe(ctx context.Context, t ProbeTarget) TargetResult {
	start := time.Now()
	deadline := start.Add(p.Window)
	interval := p.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	var r TargetResult
	attempts := 0
	for {
		attempts++
		r = p.ProbeOnce(ctx, t)
		if r.OK || !time.Now().Before(deadline) || ctx.Err() != nil {
			break
		}
		if sleep(ctx, interval) != nil {
			break
		}
	}
	r.Attempts = attempts
	r.Elapsed = time.Since(start).Round(time.Millisecond).Seconds()
	if !r.OK && attempts > 1 {
		r.Error += fmt.Sprintf(" (still failing after %d attempts over %s)", attempts, time.Since(start).Round(time.Second))
	}
	return r
}

// ProbeAll probes every target concurrently.
func (p *Prober) ProbeAll(ctx context.Context, targets []ProbeTarget) []TargetResult {
	out := make([]TargetResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = p.Probe(ctx, t)
		}()
	}
	wg.Wait()
	return out
}

// NodeProbeConfig configures node-probe.
type NodeProbeConfig struct {
	Targets      []ProbeTarget
	Prober       Prober
	RunID        string
	NodeName     string
	PodName      string
	PodNamespace string
	// Idle keeps the process alive after reporting (the DaemonSet case); false
	// returns, which is what tests and a one-shot debug run want.
	Idle bool
}

// NodeProbe probes every target, publishes the report on its own pod, and then
// idles. The probe never fails the pod: a non-Ready DaemonSet would collapse
// "this node cannot reach the mirror" into "the DaemonSet is unhealthy",
// losing the detail that makes the failure actionable. The verdict is data.
func NodeProbe(ctx context.Context, env *Env, cfg NodeProbeConfig, res *Result) error {
	if len(cfg.Targets) == 0 {
		return errors.New("no --target given")
	}
	env.logf("probing %d target(s) from node %s (retry window %s)", len(cfg.Targets), cfg.NodeName, cfg.Prober.Window)
	rep := ProbeReport{RunID: cfg.RunID, Node: cfg.NodeName, Pod: cfg.PodName, OK: true}
	rep.Targets = cfg.Prober.ProbeAll(ctx, cfg.Targets)
	rep.Time = time.Now().UTC()
	for _, t := range rep.Targets {
		if t.OK {
			detail := fmt.Sprintf("dns=%s tcp=%s", t.DNS, t.TCP)
			if t.TLS {
				detail += fmt.Sprintf(" tls=%s verified=%s subject=%q", t.Handshake, t.Verified, t.CertSubject)
			}
			res.Pass(t.Target, "%s", detail)
		} else {
			rep.OK = false
			// A WARN, not a FAIL: this pod must stay healthy. pre-install is the
			// gate that turns this into a failure, naming the node.
			res.Warn(t.Target, "UNREACHABLE: %s", t.Error)
		}
	}
	res.Set("report", rep)
	if cfg.PodName == "" || cfg.PodNamespace == "" {
		return errors.New("POD_NAME/POD_NAMESPACE unset: cannot publish the result (set them from the downward API)")
	}
	body, _ := json.Marshal(rep)
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{ProbeAnnotation: string(body)}}}
	// Retried: the API server can be briefly unreachable from a node whose
	// network is exactly what is under test, and an unpublished verdict reads as
	// "never reported" at the gate.
	var err error
	for i := 0; i < 10; i++ {
		if _, err = env.Kube.Patch(ctx, GVRPod.Path(cfg.PodNamespace, cfg.PodName), kube.MergePatch, patch); err == nil {
			break
		}
		env.logf("publishing the probe result failed (attempt %d/10): %v", i+1, err)
		if sleep(ctx, 3*time.Second) != nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("publishing the probe result on pod %s/%s: %w", cfg.PodNamespace, cfg.PodName, err)
	}
	env.logf("published %s on pod %s/%s", ProbeAnnotation, cfg.PodNamespace, cfg.PodName)
	return nil
}

// Probe aggregation, used by pre-install.

// probeView is one read of the probe DaemonSet and the verdicts that count.
type probeView struct {
	desired int
	reports map[string]ProbeReport // by node
	pending []string               // pods (on node) with no current report yet
	badJSON []string
	stale   int
}

// collectProbeReports reads the DaemonSet and its pods once.
//
// COVERAGE IS REQUIRED, NOT BEST-EFFORT (roksbnkctl CollectNodeProbeResults).
// Two nodes pass, the third (which would have failed) has not reported, and a
// summary of what exists reads "2/2 reachable" — passing blind on exactly the
// per-zone break this exists to catch. So a result is required from every node
// the DaemonSet intends to run on.
//
// STALE VERDICTS ARE IGNORED (roksbnkctl #57). A report whose runId differs from
// this run's, or from a pod of an older DaemonSet template generation, answers a
// question someone asked before the network was fixed (or broken).
//
// A RE-SYNC WITH AN UNCHANGED CONFIG renders the same run id, so the DaemonSet is
// not rolled and its pods still hold the previous verdict. pre-install therefore
// deletes the probe pods first, and only pods it did not see at start (by UID)
// and created no earlier than its start count here.
func collectProbeReports(ctx context.Context, env *Env, ns, dsName, runID string, old map[string]bool, notBefore time.Time) (probeView, error) {
	v := probeView{reports: map[string]ProbeReport{}}
	ds, err := env.Kube.Get(ctx, GVRDaemonSet.Path(ns, dsName))
	if err != nil {
		return v, fmt.Errorf("reading DaemonSet %s/%s: %w", ns, dsName, err)
	}
	v.desired = int(ds.Int("status", "desiredNumberScheduled"))
	var sel []string
	for k, val := range ds.StringMap("spec", "selector", "matchLabels") {
		sel = append(sel, k+"="+val)
	}
	sort.Strings(sel)
	pods, err := env.Kube.List(ctx, GVRPod.Path(ns, ""), kube.ListOptions{LabelSelector: strings.Join(sel, ",")})
	if err != nil {
		return v, fmt.Errorf("listing probe pods: %w", err)
	}
	gen := strconv.FormatInt(ds.Int("metadata", "generation"), 10)
	for _, p := range pods {
		if p.Deleting() || old[p.UID()] {
			continue
		}
		if !notBefore.IsZero() {
			if ct, err := time.Parse(time.RFC3339, p.String("metadata", "creationTimestamp")); err != nil || ct.Before(notBefore) {
				v.stale++
				continue
			}
		}
		if g, ok := p.Annotations()["deprecated.daemonset.template.generation"]; ok && gen != "0" && g != gen {
			v.stale++
			continue
		}
		node := p.String("spec", "nodeName")
		raw, ok := p.Annotations()[ProbeAnnotation]
		if !ok {
			v.pending = append(v.pending, p.Name()+" on "+nodeOr(node))
			continue
		}
		var rep ProbeReport
		if err := json.Unmarshal([]byte(raw), &rep); err != nil {
			v.badJSON = append(v.badJSON, p.Name()+" on "+nodeOr(node)+": "+err.Error())
			continue
		}
		if runID != "" && rep.RunID != runID {
			v.stale++
			v.pending = append(v.pending, p.Name()+" on "+nodeOr(node)+" (result is from run "+orNone(rep.RunID)+")")
			continue
		}
		if rep.Node == "" {
			rep.Node = node
		}
		if rep.Node == "" {
			rep.Node = p.Name()
		}
		rep.Pod = p.Name()
		v.reports[rep.Node] = rep
	}
	return v, nil
}

func nodeOr(n string) string {
	if n == "" {
		return "(unscheduled)"
	}
	return n
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// complete reports whether every intended node has a current verdict.
func (v probeView) complete() bool {
	return v.desired > 0 && len(v.reports) >= v.desired && len(v.badJSON) == 0
}

// ProbeFailures lists every unreachable target on every node as
// "node -> target: error", sorted.
func ProbeFailures(reports map[string]ProbeReport) []string {
	var out []string
	for node, rep := range reports {
		for _, t := range rep.Targets {
			if !t.OK {
				out = append(out, fmt.Sprintf("%s -> %s: %s", node, t.Target, t.Error))
			}
		}
	}
	sort.Strings(out)
	return out
}

// ProbeSummary renders "target: n/m nodes reachable" lines. Every node is
// listed so a per-zone split is visible, not just the failures.
func ProbeSummary(reports map[string]ProbeReport) []string {
	type agg struct{ ok, total int }
	by := map[string]*agg{}
	for _, rep := range reports {
		for _, t := range rep.Targets {
			a := by[t.Target]
			if a == nil {
				a = &agg{}
				by[t.Target] = a
			}
			a.total++
			if t.OK {
				a.ok++
			}
		}
	}
	var out []string
	for t, a := range by {
		out = append(out, fmt.Sprintf("%s: %d/%d nodes reachable", t, a.ok, a.total))
	}
	sort.Strings(out)
	return out
}

// restartProbePods deletes every current pod of the probe DaemonSet so the
// DaemonSet controller re-creates them and they probe afresh. It returns the
// UIDs it saw, which are never counted afterwards.
func restartProbePods(ctx context.Context, env *Env, ns, dsName string) (map[string]bool, error) {
	ds, err := env.Kube.Get(ctx, GVRDaemonSet.Path(ns, dsName))
	if err != nil {
		return nil, fmt.Errorf("reading DaemonSet %s/%s: %w", ns, dsName, err)
	}
	var sel []string
	for k, val := range ds.StringMap("spec", "selector", "matchLabels") {
		sel = append(sel, k+"="+val)
	}
	sort.Strings(sel)
	if len(sel) == 0 {
		return nil, fmt.Errorf("DaemonSet %s/%s has no matchLabels selector", ns, dsName)
	}
	pods, err := env.Kube.List(ctx, GVRPod.Path(ns, ""), kube.ListOptions{LabelSelector: strings.Join(sel, ",")})
	if err != nil {
		return nil, fmt.Errorf("listing probe pods: %w", err)
	}
	old := map[string]bool{}
	for _, p := range pods {
		old[p.UID()] = true
		if _, err := env.Kube.DeleteIfExists(ctx, GVRPod.Path(ns, p.Name()), ""); err != nil {
			return old, fmt.Errorf("deleting probe pod %s: %w", p.Name(), err)
		}
	}
	return old, nil
}
