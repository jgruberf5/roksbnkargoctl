package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The live gap polling left: a PreDelete check that finishes and is deleted
// between two snapshots. The watch must catch it with no Snapshot at all.
func TestCollectorWatchCatchesAPodBetweenSnapshots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	c := newCheckLogCollector(cs, filepath.Join(t.TempDir(), "u"))
	started := make(chan struct{})
	go func() { close(started); c.Watch(ctx) }()
	<-started
	time.Sleep(100 * time.Millisecond) // let the watch register
	pods := cs.CoreV1().Pods(render.CheckNamespace)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-pre-uninstall-xyz", Namespace: render.CheckNamespace}}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "check", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	if _, err := pods.UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		_, got := c.logs["check-pre-uninstall-xyz"]
		c.mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch did not capture the terminated pod's log")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = pods.Delete(ctx, "check-pre-uninstall-xyz", metav1.DeleteOptions{})
	if n := c.Finish("uninstall"); n != 1 {
		t.Fatalf("kept %d logs, want 1", n)
	}
}
