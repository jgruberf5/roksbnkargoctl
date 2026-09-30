package cli

import (
	"context"
	"errors"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	c := newCheckLogCollector(cs, cs, dir)
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
	c := newCheckLogCollector(cs, cs, t.TempDir())
	c.Snapshot(context.Background())
	first := c.last
	c.MaybeSnapshot(context.Background())
	if c.last != first {
		t.Fatal("MaybeSnapshot within the interval must not snapshot again")
	}
}

// The live gap polling left: a PreDelete check that finishes and is deleted
// between two snapshots. The watch must catch it with no Snapshot at all.
// startWatch runs c.Watch until the test ends, then cancels it and waits for it
// to return, so the watch never outlives the test and its temporary directory
// (#20: a watch goroutine left running into later tests). It also fails the
// test if Watch does not stop when its context is cancelled.
func startWatch(t *testing.T, c *checkLogCollector) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Watch(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the collector's watch did not stop when its context was cancelled")
		}
	})
	return ctx
}

func TestCollectorWatchCatchesAPodBetweenSnapshots(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := newCheckLogCollector(cs, cs, filepath.Join(t.TempDir(), "u"))
	ctx := startWatch(t, c)
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

// Review finding W1–W3: the collector's wiring was untested — removing the watch,
// the per-poll tick, or the save on the failure path left the suite green.
// collectCheckLogsDuring is what uninstall runs; each case below fails if one
// of those three is removed.

// W1: a pod that terminates and is deleted with no tick at all is caught only
// by the watch collectCheckLogsDuring starts.
func TestCollectDuringStartsTheWatch(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	c := newCheckLogCollector(cs, cs, filepath.Join(t.TempDir(), "u"))
	pods := cs.CoreV1().Pods(render.CheckNamespace)
	n, err := collectCheckLogsDuring(ctx, c, func(func()) error {
		time.Sleep(100 * time.Millisecond)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-pre-uninstall-w1", Namespace: render.CheckNamespace}}
		_, _ = pods.Create(ctx, p, metav1.CreateOptions{})
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		_, _ = pods.UpdateStatus(ctx, p, metav1.UpdateOptions{})
		time.Sleep(300 * time.Millisecond)
		return pods.Delete(ctx, "check-pre-uninstall-w1", metav1.DeleteOptions{})
	})
	if err != nil || n != 1 {
		t.Fatalf("kept %d logs (err %v); the watch must catch the deleted PreDelete pod", n, err)
	}
}

// W2: a running pod (no terminated container, so the watch ignores it) that is
// deleted before the end is caught only by fn's tick.
func TestCollectDuringTickSnapshots(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	c := newCheckLogCollector(cs, cs, filepath.Join(t.TempDir(), "u"))
	c.every = 0
	pods := cs.CoreV1().Pods(render.CheckNamespace)
	n, _ := collectCheckLogsDuring(ctx, c, func(tick func()) error {
		_, _ = pods.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-pre-uninstall-w2", Namespace: render.CheckNamespace}}, metav1.CreateOptions{})
		tick()
		return pods.Delete(ctx, "check-pre-uninstall-w2", metav1.DeleteOptions{})
	})
	if n != 1 {
		t.Fatalf("kept %d logs; the tick must snapshot", n)
	}
}

// W3: the deletion fails; the logs are still saved and the error returned.
func TestCollectDuringSavesOnFailure(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-pre-uninstall-w3", Namespace: render.CheckNamespace}})
	dir := filepath.Join(t.TempDir(), "u")
	c := newCheckLogCollector(cs, cs, dir)
	n, err := collectCheckLogsDuring(ctx, c, func(func()) error { return errors.New("application deletion timed out") })
	if err == nil || n != 1 {
		t.Fatalf("n=%d err=%v; a failed deletion must still save logs and return its error", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.md")); err != nil {
		t.Fatalf("summary.md not written on the failure path: %v", err)
	}
}

// Review finding: the watch stream ends (client timeout, server expiry, error
// event) and the old Watch returned for good. It must re-establish itself.
func TestCollectorRewatchesWhenTheStreamEnds(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var watches atomic.Int32
	cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		if watches.Add(1) == 1 {
			w := watch.NewFake()
			w.Stop() // the first stream ends at once, as a timed-out one does
			return true, w, nil
		}
		return false, nil, nil // later watches: the fake clientset's real tracker
	})
	c := newCheckLogCollector(cs, cs, filepath.Join(t.TempDir(), "u"))
	ctx := startWatch(t, c)
	deadline := time.Now().Add(5 * time.Second)
	for watches.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the watch was not re-established after its stream ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	pods := cs.CoreV1().Pods(render.CheckNamespace)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "check-pre-uninstall-late", Namespace: render.CheckNamespace}}
	_, _ = pods.Create(ctx, p, metav1.CreateOptions{})
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	_, _ = pods.UpdateStatus(ctx, p, metav1.UpdateOptions{})
	for {
		c.mu.Lock()
		_, got := c.logs["check-pre-uninstall-late"]
		c.mu.Unlock()
		if got {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a pod that terminated after the first stream ended was missed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
