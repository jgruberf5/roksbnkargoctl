package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// Issue #5: the PreDelete pod is deleted with the Application. A log seen while
// the pod existed must survive the pod — that is the whole point of collecting
// during the deletion rather than after it.
func TestCollectorKeepsLogsOfDeletedPods(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-pre-uninstall-abc", Namespace: render.CheckNamespace}})
	dir := filepath.Join(t.TempDir(), "uninstall")
	c := newCheckLogCollector(cs, dir)
	c.Snapshot(ctx) // the PreDelete pod exists
	if err := cs.CoreV1().Pods(render.CheckNamespace).Delete(ctx, "check-pre-uninstall-abc", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _ = cs.CoreV1().Pods(render.CheckNamespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-post-uninstall-def", Namespace: render.CheckNamespace}}, metav1.CreateOptions{})
	c.Snapshot(ctx) // PreDelete gone, PostDelete present
	if n := c.Finish("uninstall"); n != 2 {
		t.Fatalf("kept %d pods' logs, want 2 (the deleted PreDelete pod must be kept)", n)
	}
	for _, f := range []string{"log-check-pre-uninstall-abc.txt", "log-check-post-uninstall-def.txt", "summary.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	sum, _ := os.ReadFile(filepath.Join(dir, "summary.md"))
	if !strings.Contains(string(sum), "check-pre-uninstall-abc") {
		t.Errorf("summary omits the PreDelete pod:\n%s", sum)
	}
}

func TestCollectorThrottles(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := newCheckLogCollector(cs, t.TempDir())
	c.Snapshot(context.Background())
	first := c.last
	c.MaybeSnapshot(context.Background())
	if c.last != first {
		t.Fatal("MaybeSnapshot within the interval must not snapshot again")
	}
}
