package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// Why uninstall runs the uninstall checks itself.
//
// Argo CD 3.5.1 (the issue is open upstream) can declare a
// PreDelete or PostDelete hook finished the moment it creates it: a second
// finalization pass gets AlreadyExists from the create, finds the Job missing
// from its not-yet-updated cache, counts zero running hooks and removes the
// finalizer (argoproj/argo-cd#29100). Live on bnkargo the pre-uninstall pod got
// SIGTERM 0.4s after it started, the cascade pruned FLO underneath it, and the
// post-uninstall hook never ran — leaving f5-bnk, f5-utils and cert-manager
// behind while uninstall printed "uninstalled".
//
// So `uninstall` runs pre-uninstall before it deletes the Application and
// post-uninstall after, as plain Jobs it waits on, then checks the namespaces
// are gone. The hooks stay in Git for a delete started from the Argo CD UI,
// where they are best effort; both checks are idempotent, so a hook that does
// run after the CLI's copy finds nothing left to do.

// argoHookFinalizers are the finalizers Argo CD's controller adds and removes
// itself (with an optional "/cleanup" stage suffix).
var argoHookFinalizers = []string{"pre-delete-finalizer.argocd.argoproj.io", "post-delete-finalizer.argocd.argoproj.io"}

func isArgoHookFinalizer(f string) bool {
	for _, n := range argoHookFinalizers {
		if f == n || strings.HasPrefix(f, n+"/") {
			return true
		}
	}
	return false
}

// mergeFinalizers is ours plus the Argo-CD-managed hook finalizers the live
// Application already carries. Argo CD's upsert replaces the list, so sending
// ours alone would strip them.
func mergeFinalizers(current, ours []string) []string {
	out := append([]string(nil), ours...)
	for _, f := range current {
		if isArgoHookFinalizer(f) && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// cliJobSuffix names the CLI's copy of a hook Job, so it never collides with
// the hook Argo CD may create under the hook's own name.
const cliJobSuffix = "-cli"

// checkJobFromManifests returns the rendered check Job for mode from the
// workspace's manifests/git, as a plain Job: Argo CD hook annotations dropped
// and the name suffixed.
func checkJobFromManifests(gitDir, mode string) (*batchv1.Job, error) {
	objs, err := readYAMLDir(gitDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s (run `roksbnkargoctl render`): %w", gitDir, err)
	}
	for _, o := range objs {
		if o.Kind() != "Job" {
			continue
		}
		md, _ := o["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if labels["roksbnkargoctl.io/check-mode"] != mode {
			continue
		}
		b, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		var j batchv1.Job
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, fmt.Errorf("check Job %s: %w", mode, err)
		}
		for k := range j.Annotations {
			if strings.HasPrefix(k, "argocd.argoproj.io/") {
				delete(j.Annotations, k)
			}
		}
		j.Name += cliJobSuffix
		j.Labels["roksbnkargoctl.io/run-by"] = "cli"
		return &j, nil
	}
	return nil, fmt.Errorf("no %s check Job in %s (run `roksbnkargoctl render`)", mode, gitDir)
}

// jobTimeout is the Job's own activeDeadlineSeconds plus a minute, so the Job,
// not the CLI, is what gives up first.
func jobTimeout(j *batchv1.Job) time.Duration {
	if j.Spec.ActiveDeadlineSeconds != nil {
		return time.Duration(*j.Spec.ActiveDeadlineSeconds)*time.Second + time.Minute
	}
	return 25 * time.Minute
}

// runCheckJob runs one uninstall check in ROKS and waits for it.
func runCheckJob(ctx context.Context, s *session, k kubernetes.Interface, mode string) error {
	if _, err := k.CoreV1().Namespaces().Get(ctx, render.CheckNamespace, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("the %s namespace is not there to run the %s check in (%w); `roksbnkargoctl install --no-sync` restores it", render.CheckNamespace, mode, err)
	}
	j, err := checkJobFromManifests(filepath.Join(s.ws.ManifestsDir(), "git"), mode)
	if err != nil {
		return err
	}
	return kube.RunJob(ctx, k, j, jobTimeout(j))
}

// bnkNamespaces are the namespaces the post-uninstall check deletes.
func bnkNamespaces(c *config.Config) []string {
	out := []string{c.BNK.Namespace}
	if c.BNK.UtilsNamespace != c.BNK.Namespace {
		out = append(out, c.BNK.UtilsNamespace)
	}
	if c.CertManagerInstall() {
		out = append(out, "cert-manager")
	}
	return out
}

// namespacesRemaining lists which of names still exist, with their phase.
func namespacesRemaining(ctx context.Context, k kubernetes.Interface, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		ns, err := k.CoreV1().Namespaces().Get(ctx, n, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s (%s)", n, ns.Status.Phase))
	}
	return out, nil
}

// uninstallSteps is the order uninstall runs in, apart from the API calls, so
// the order and the refusal to report success early are testable.
type uninstallSteps struct {
	p         printer
	force     bool
	appExists func() (bool, error)
	deleteApp func(tick func()) error
	// runCheck and remaining are nil when the cluster cannot be reached.
	runCheck  func(mode string) error
	remaining func() ([]string, error)
}

func (u uninstallSteps) run(tick func()) error {
	exists, err := u.appExists()
	if err != nil {
		return err
	}
	if exists {
		if u.runCheck != nil {
			u.p.step("pre-uninstall check: drain F5 resources while FLO still runs, CNEInstance last")
			if err := u.runCheck("pre-uninstall"); err != nil {
				if !u.force {
					return fmt.Errorf("pre-uninstall check: %w\nthe Application was NOT deleted; the saved check log says what is left (--force deletes it anyway)", err)
				}
				u.p.warn("pre-uninstall check: %v; deleting the Application anyway (--force)", err)
			} else {
				u.p.ok("pre-uninstall check passed")
			}
		}
		if err := u.deleteApp(tick); err != nil {
			return err
		}
	} else {
		u.p.info("the Application does not exist")
	}
	if u.runCheck == nil {
		return nil
	}
	// Re-running uninstall after an interrupted one lands here with the
	// Application gone: the post-uninstall check is what finishes the job.
	left, err := u.remaining()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		u.p.step("post-uninstall check: license secrets, stuck F5 finalizers, namespaces %s", strings.Join(left, ", "))
		if err := u.runCheck("post-uninstall"); err != nil {
			return fmt.Errorf("post-uninstall check: %w", err)
		}
		u.p.ok("post-uninstall check passed")
	}
	if left, err = u.remaining(); err != nil {
		return err
	} else if len(left) > 0 {
		return fmt.Errorf("BNK is not fully removed: namespaces %s remain; re-run `roksbnkargoctl uninstall` (the check namespace is kept so it can)", strings.Join(left, ", "))
	}
	return nil
}
