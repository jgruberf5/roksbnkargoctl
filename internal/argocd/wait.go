package argocd

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// OperationResult is the final state of a sync operation.
type OperationResult struct {
	Phase     string // Succeeded, Failed or Error
	Message   string
	Revision  string
	Resources []ResourceResult
}

// Succeeded reports whether the operation succeeded.
func (r *OperationResult) Succeeded() bool { return r != nil && r.Phase == PhaseSucceeded }

// Failures returns the resources that failed to sync and the hooks that failed.
func (r *OperationResult) Failures() []ResourceResult {
	if r == nil {
		return nil
	}
	var out []ResourceResult
	for _, x := range r.Resources {
		if x.Status == "SyncFailed" || x.HookPhase == PhaseFailed || x.HookPhase == PhaseError {
			out = append(out, x)
		}
	}
	return out
}

// OperationError is returned by WaitOperation when the operation ended Failed or Error.
type OperationError struct {
	App    string
	Result *OperationResult
}

func (e *OperationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "argocd: sync of %s ended %s", e.App, e.Result.Phase)
	if e.Result.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Result.Message)
	}
	for _, f := range e.Result.Failures() {
		what := f.Kind + "/" + f.Name
		if f.Namespace != "" {
			what = f.Namespace + "/" + what
		}
		if f.HookType != "" {
			what += " (" + f.HookType + " hook, " + f.HookPhase + ")"
		}
		fmt.Fprintf(&b, "\n  %s: %s", what, f.Message)
	}
	return b.String()
}

// Progress is called on every poll with the latest Application.
type Progress func(app *Application)

// WaitOperation polls the Application until its current operation completes, and returns
// the result. The operation is complete only when status.operationState.phase is terminal
// AND the top-level .operation field is gone: the controller clears .operation in the same
// patch that records a terminal phase (controller/appcontroller.go setOperationState), so a
// stale Succeeded from a previous sync, or a Failed that is about to be retried, is not
// mistaken for the outcome. A Failed/Error outcome returns the result and an *OperationError.
func (c *Client) WaitOperation(ctx context.Context, name string, timeout time.Duration, progress Progress) (*OperationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		app, err := c.GetApplication(ctx, name, "")
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("argocd: timed out after %s waiting for the sync of %s: %w", timeout, name, err)
			}
			return nil, err
		}
		if progress != nil {
			progress(app)
		}
		if res := completedOperation(app); res != nil {
			if res.Succeeded() {
				return res, nil
			}
			return res, &OperationError{App: name, Result: res}
		}
		if err := c.sleep(ctx); err != nil {
			return nil, fmt.Errorf("argocd: timed out after %s waiting for the sync of %s: %w", timeout, name, err)
		}
	}
}

func completedOperation(app *Application) *OperationResult {
	if len(app.Operation) > 0 && string(app.Operation) != "null" {
		return nil
	}
	if app.Status == nil || app.Status.OperationState == nil {
		return nil
	}
	st := app.Status.OperationState
	if !PhaseCompleted(st.Phase) {
		return nil
	}
	res := &OperationResult{Phase: st.Phase, Message: st.Message}
	if st.SyncResult != nil {
		res.Revision = st.SyncResult.Revision
		res.Resources = st.SyncResult.Resources
	}
	return res
}

// WaitGone polls until the Application no longer exists. It relies on Client.Project being
// the app's project: without a project Argo CD answers 403 for a missing Application, and
// 403 is reported as an error rather than taken as "gone".
func (c *Client) WaitGone(ctx context.Context, name string, timeout time.Duration, progress Progress) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		app, err := c.GetApplication(ctx, name, "")
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("argocd: timed out after %s waiting for %s to be deleted: %w", timeout, name, err)
			}
			return err
		}
		if progress != nil {
			progress(app)
		}
		if err := c.sleep(ctx); err != nil {
			return fmt.Errorf("argocd: timed out after %s waiting for %s to be deleted: %w", timeout, name, err)
		}
	}
}

func (c *Client) sleep(ctx context.Context) error {
	d := c.PollInterval
	if d <= 0 {
		d = 3 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
