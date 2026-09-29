package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testJob() *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "check-post-uninstall-cli", Namespace: "roksbnkargoctl-check"}}
}

// finishOnGet makes the Job report outcome from the n-th read after it was
// created, so RunJob has to poll rather than read a result it was handed.
func finishOnGet(k *fake.Clientset, n int, outcome func(*batchv1.Job)) {
	reads := 0
	k.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := k.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), a.GetNamespace(), a.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		j := obj.(*batchv1.Job).DeepCopy()
		if reads++; reads >= n {
			outcome(j)
		}
		return true, j, nil
	})
}

func init() { JobPoll = time.Millisecond }

// A Succeeded Job left by the previous uninstall must not read as this run's
// result: RunJob deletes it, and the new Job's failure is what is reported.
func TestRunJobIgnoresAStaleSucceededJob(t *testing.T) {
	stale := testJob()
	stale.Status.Succeeded = 1
	k := fake.NewSimpleClientset(stale)
	finishOnGet(k, 3, func(j *batchv1.Job) {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	})
	err := RunJob(context.Background(), k, testJob(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "BackoffLimitExceeded") {
		t.Fatalf("want the new Job's failure, got %v", err)
	}
}

func TestRunJobWaitsForSuccess(t *testing.T) {
	k := fake.NewSimpleClientset()
	finishOnGet(k, 4, func(j *batchv1.Job) { j.Status.Succeeded = 1 })
	if err := RunJob(context.Background(), k, testJob(), time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestRunJobTimesOut(t *testing.T) {
	k := fake.NewSimpleClientset()
	err := RunJob(context.Background(), k, testJob(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("want a timeout, got %v", err)
	}
}
