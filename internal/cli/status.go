package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/jgruberf5/roksbnkargoctl/internal/argocd"
	"github.com/jgruberf5/roksbnkargoctl/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Application sync/health, check results and BNK state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			return runStatus(cmd.Context(), s, cmd.OutOrStdout())
		},
	}
}

func runStatus(ctx context.Context, s *session, w io.Writer) error {
	c := s.cfg
	fmt.Fprintf(w, "Workspace:       %s\n", s.ws.Name)
	fmt.Fprintf(w, "Cluster:         %s\n", c.Cluster)
	fmt.Fprintf(w, "Transit gateway: %s\n", c.TransitGateway)
	fmt.Fprintf(w, "Mode / registry: %s / %s (%s)\n", c.BNK.Mode, c.Registry.Source, c.ImageHost())
	fmt.Fprintf(w, "Git:             %s %s:%s\n", c.Git.URL, c.Git.Branch, c.Git.Path)
	if c.Resolved != nil && c.Resolved.LastPublishedCommitSHA != "" {
		fmt.Fprintf(w, "Last published:  %s\n", short(c.Resolved.LastPublishedCommitSHA))
	}
	if ac, err := s.ArgoCD(); err != nil {
		fmt.Fprintf(w, "Argo CD:         %v\n", err)
	} else if app, err := ac.GetApplication(ctx, c.ArgoCD.Application, ""); err != nil {
		if argocd.IsNotFound(err) {
			fmt.Fprintf(w, "Application:     %s — not created (run install)\n", c.ArgoCD.Application)
		} else {
			fmt.Fprintf(w, "Application:     %v\n", err)
		}
	} else {
		st := app.Status
		fmt.Fprintf(w, "Application:     %s  sync=%s  health=%s\n", c.ArgoCD.Application, st.Sync.Status, st.Health.Status)
		if op := st.OperationState; op != nil {
			fmt.Fprintf(w, "Last operation:  %s — %s\n", op.Phase, strings.TrimSpace(op.Message))
		}
	}
	k, err := s.Kube(ctx)
	if err != nil {
		fmt.Fprintf(w, "Cluster access:  %v\n", err)
		return nil
	}
	if jobs, err := k.CheckJobs(ctx, render.CheckNamespace); err == nil && len(jobs) > 0 {
		fmt.Fprintln(w, "Checks:")
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
		for _, j := range jobs {
			line := fmt.Sprintf("  %-28s %s", j.Name, j.Phase)
			if j.Message != "" {
				line += "  " + j.Message
			}
			fmt.Fprintln(w, line)
		}
	}
	for _, x := range []struct{ api, kind, ns, name, what string }{
		{"k8s.f5.com/v1", "CNEInstance", c.BNK.Namespace, render.CNEInstanceName, "conditions"},
		{"k8s.f5net.com/v1", "License", c.BNK.UtilsNamespace, render.LicenseName, "state"},
	} {
		u, err := k.Get(ctx, x.api, x.kind, x.ns, x.name)
		if err != nil {
			if kube.IsNotFound(err) {
				fmt.Fprintf(w, "%-16s absent\n", x.kind+":")
				continue
			}
			fmt.Fprintf(w, "%-16s %v\n", x.kind+":", err)
			continue
		}
		fmt.Fprintf(w, "%-16s %s\n", x.kind+":", crSummary(u.Object))
	}
	return nil
}

func crSummary(obj map[string]any) string {
	st, _ := obj["status"].(map[string]any)
	if st == nil {
		return "no status yet"
	}
	var parts []string
	if s, ok := st["state"].(string); ok {
		parts = append(parts, "state="+s)
	}
	if conds, ok := st["conditions"].([]any); ok {
		for _, c := range conds {
			m, _ := c.(map[string]any)
			parts = append(parts, fmt.Sprintf("%v=%v", m["type"], m["status"]))
		}
	}
	if len(parts) == 0 {
		return "status present, no state/conditions"
	}
	return strings.Join(parts, " ")
}

// ---- diagnose ---------------------------------------------------------------------

func newDiagnoseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose",
		Short: "Collect Application, check-log, pod, event and BNK CR state into the workspace",
		Long: `diagnose writes diagnostics/<time>/ in the workspace: the Application and its
resource tree, every check Job's and check pod's log, pods and events in the BNK,
cert-manager and check namespaces, and the CNEInstance and License status. Read
summary.md first. Secret values are never collected. This is what the
troubleshooter persona works from; no kubectl or oc is needed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			dir, err := runDiagnose(cmd.Context(), s)
			if err != nil {
				return err
			}
			s.p.ok("diagnostics written to %s (start with summary.md)", dir)
			return nil
		},
	}
}

func runDiagnose(ctx context.Context, s *session) (string, error) {
	c := s.cfg
	dir := filepath.Join(s.ws.Dir, "diagnostics", time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var sum bytes.Buffer
	fmt.Fprintf(&sum, "# roksbnkargoctl diagnostics — %s\n\n", time.Now().UTC().Format(time.RFC3339))
	write := func(name string, v any) {
		var b []byte
		switch t := v.(type) {
		case []byte:
			b = t
		case string:
			b = []byte(t)
		default:
			b, _ = yaml.Marshal(t)
		}
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o600)
	}

	fmt.Fprint(&sum, "## Status\n\n```\n")
	_ = runStatus(ctx, s, &sum)
	fmt.Fprintln(&sum, "```")

	if ac, err := s.ArgoCD(); err == nil {
		if app, err := ac.GetApplication(ctx, c.ArgoCD.Application, ""); err == nil {
			write("application.yaml", app)
			if op := app.Status.OperationState; op != nil {
				fmt.Fprint(&sum, "\n## Last Argo CD operation\n\n")
				fmt.Fprintf(&sum, "- phase: %s\n- message: %s\n", op.Phase, op.Message)
			}
			if tree, err := ac.ResourceTree(ctx, c.ArgoCD.Application, ""); err == nil {
				write("resource-tree.yaml", tree)
				if jobs := argocd.Jobs(tree, app.Status.OperationState); len(jobs) > 0 {
					fmt.Fprint(&sum, "\n## Hook Jobs (Argo CD's view)\n\n")
					for _, j := range jobs {
						fmt.Fprintf(&sum, "- %+v\n", j)
					}
				}
			}
		}
	}

	k, err := s.Kube(ctx)
	if err != nil {
		fmt.Fprintf(&sum, "\ncluster access failed: %v\n", err)
		write("summary.md", sum.String())
		return dir, nil
	}
	nss := []string{render.CheckNamespace, c.BNK.Namespace, c.BNK.UtilsNamespace, "cert-manager"}
	fmt.Fprint(&sum, "\n## Pods not Running/Succeeded\n\n")
	bad := 0
	for _, ns := range nss {
		pods, err := k.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		write("pods-"+ns+".yaml", podTable(pods.Items))
		for _, p := range pods.Items {
			if reason := podProblem(p); reason != "" {
				fmt.Fprintf(&sum, "- %s/%s: %s\n", ns, p.Name, reason)
				bad++
			}
		}
		if evs, err := k.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{}); err == nil {
			var lines []string
			for _, e := range evs.Items {
				if e.Type == corev1.EventTypeWarning {
					lines = append(lines, fmt.Sprintf("%s %s/%s %s: %s", e.LastTimestamp.Format(time.RFC3339), e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message))
				}
			}
			sort.Strings(lines)
			write("warnings-"+ns+".txt", strings.Join(lines, "\n")+"\n")
		}
	}
	if bad == 0 {
		fmt.Fprintln(&sum, "none")
	}

	// Check logs: every pod the check image runs, hooks and long-running alike.
	fmt.Fprint(&sum, "\n## Check verdicts (last JSON line of each check pod)\n\n")
	pods, _ := k.Typed.CoreV1().Pods(render.CheckNamespace).List(ctx, metav1.ListOptions{})
	for _, p := range pods.Items {
		for _, ct := range p.Spec.Containers {
			raw, err := k.Typed.CoreV1().Pods(render.CheckNamespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: ct.Name, TailLines: ptr(int64(2000))}).DoRaw(ctx)
			if err != nil {
				continue
			}
			write("log-"+p.Name+".txt", raw)
			if v := lastJSONLine(raw); v != "" {
				fmt.Fprintf(&sum, "- %s: `%s`\n", p.Name, v)
			}
		}
		if res := p.Annotations["roksbnkargoctl.io/probe-result"]; res != "" {
			fmt.Fprintf(&sum, "- %s probe: `%s`\n", p.Name, res)
		}
	}

	for _, x := range []struct{ api, kind, ns, name string }{
		{"k8s.f5.com/v1", "CNEInstance", c.BNK.Namespace, render.CNEInstanceName},
		{"k8s.f5net.com/v1", "License", c.BNK.UtilsNamespace, render.LicenseName},
	} {
		if u, err := k.Get(ctx, x.api, x.kind, x.ns, x.name); err == nil {
			obj := u.Object
			if spec, ok := obj["spec"].(map[string]any); ok {
				if _, has := spec["jwt"]; has {
					spec["jwt"] = "REDACTED"
				}
			}
			write(strings.ToLower(x.kind)+".yaml", obj)
		}
	}
	write("summary.md", sum.String())
	return dir, nil
}

func ptr[T any](v T) *T { return &v }

func lastJSONLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && json.Valid([]byte(l)) {
			if len(l) > 600 {
				l = l[:600] + "…"
			}
			return l
		}
	}
	return ""
}

func podProblem(p corev1.Pod) string {
	for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" && w.Reason != "ContainerCreating" {
			return w.Reason + ": " + w.Message
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Sprintf("%s exited %d: %s", cs.Name, t.ExitCode, t.Reason)
		}
	}
	switch p.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded:
		return ""
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			return "unschedulable: " + c.Message
		}
	}
	return string(p.Status.Phase)
}

func podTable(pods []corev1.Pod) []map[string]any {
	var out []map[string]any
	for _, p := range pods {
		out = append(out, map[string]any{"name": p.Name, "phase": p.Status.Phase, "node": p.Spec.NodeName, "problem": podProblem(p)})
	}
	return out
}

// readYAMLDir reads every *.yaml in dir as generic objects.
func readYAMLDir(dir string) ([]render.Object, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []render.Object
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		objs, err := render.SplitYAML(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, objs...)
	}
	return out, nil
}
