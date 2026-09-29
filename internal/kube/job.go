package kube

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// JobPoll is how often RunJob reads the Job's status.
var JobPoll = 3 * time.Second

// RunJob replaces any Job of the same name with job, then waits until it
// succeeds (nil), fails (an error quoting its condition) or timeout passes.
// A leftover Job from an earlier run is deleted first: a Job's pod template is
// immutable, and its old status would otherwise read as this run's result.
func RunJob(ctx context.Context, k kubernetes.Interface, job *batchv1.Job, timeout time.Duration) error {
	jobs := k.BatchV1().Jobs(job.Namespace)
	deadline := time.Now().Add(timeout)
	bg := metav1.DeletePropagationBackground
	if err := jobs.Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing the previous Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	for {
		if _, err := jobs.Get(ctx, job.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			return err
		}
		if err := sleepUntil(ctx, deadline); err != nil {
			return fmt.Errorf("the previous Job %s/%s is still being deleted: %w", job.Namespace, job.Name, err)
		}
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
		return fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	for {
		j, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		if j.Status.Succeeded > 0 {
			return nil
		}
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == "True" {
				return fmt.Errorf("Job %s/%s failed: %s %s", job.Namespace, job.Name, c.Reason, c.Message)
			}
		}
		if j.Status.Failed > 0 && (j.Spec.BackoffLimit == nil || j.Status.Failed > *j.Spec.BackoffLimit) {
			return fmt.Errorf("Job %s/%s failed", job.Namespace, job.Name)
		}
		if err := sleepUntil(ctx, deadline); err != nil {
			return fmt.Errorf("Job %s/%s did not finish: %w", job.Namespace, job.Name, err)
		}
	}
}

// sleepUntil waits one JobPoll, or fails once deadline has passed.
func sleepUntil(ctx context.Context, deadline time.Time) error {
	if time.Now().After(deadline) {
		return context.DeadlineExceeded
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(JobPoll):
		return nil
	}
}
