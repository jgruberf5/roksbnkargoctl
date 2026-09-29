// Package checks implements every mode of the check binary. Each mode takes a
// *kube.Client and a config struct, writes human logs to Env.Log, and fills a
// Result that main prints as one JSON line on stdout.
package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
)

// Env is what every mode needs.
type Env struct {
	Kube *kube.Client
	Log  io.Writer
}

func (e *Env) logf(format string, args ...any) {
	if e.Log != nil {
		fmt.Fprintf(e.Log, format+"\n", args...)
	}
}

// Severity of a finding.
const (
	SevPass = "pass"
	SevFail = "fail"
	SevWarn = "warn"
	SevInfo = "info"
)

// Finding is one line of a result.
type Finding struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Detail   string `json:"detail,omitempty"`
}

// Result is the one-line JSON a mode prints on stdout.
type Result struct {
	Mode            string         `json:"mode"`
	Version         string         `json:"version,omitempty"`
	OK              bool           `json:"ok"`
	Started         time.Time      `json:"started"`
	DurationSeconds float64        `json:"durationSeconds"`
	Findings        []Finding      `json:"findings"`
	Data            map[string]any `json:"data,omitempty"`
	Error           string         `json:"error,omitempty"`

	mu  sync.Mutex
	log io.Writer
}

// NewResult starts a result for mode, logging findings to log as they happen.
func NewResult(mode string, log io.Writer) *Result {
	return &Result{Mode: mode, Started: time.Now().UTC(), Findings: []Finding{}, log: log}
}

func (r *Result) add(sev, check, format string, args ...any) {
	f := Finding{Check: check, Severity: sev, Detail: fmt.Sprintf(format, args...)}
	r.mu.Lock()
	r.Findings = append(r.Findings, f)
	r.mu.Unlock()
	if r.log != nil {
		mark := map[string]string{SevPass: "PASS", SevFail: "FAIL", SevWarn: "WARN", SevInfo: "INFO"}[sev]
		fmt.Fprintf(r.log, "[%s] %s: %s\n", mark, check, f.Detail)
	}
}

// Pass records a passed check.
func (r *Result) Pass(check, format string, args ...any) { r.add(SevPass, check, format, args...) }

// Fail records a failed check; the result will not be OK.
func (r *Result) Fail(check, format string, args ...any) { r.add(SevFail, check, format, args...) }

// Warn records something worth reading that does not fail the run.
func (r *Result) Warn(check, format string, args ...any) { r.add(SevWarn, check, format, args...) }

// Info records context.
func (r *Result) Info(check, format string, args ...any) { r.add(SevInfo, check, format, args...) }

// Set stores structured data in the result.
func (r *Result) Set(k string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Data == nil {
		r.Data = map[string]any{}
	}
	r.Data[k] = v
}

// Failed reports whether any finding failed or an error was recorded.
func (r *Result) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Error != "" {
		return true
	}
	for _, f := range r.Findings {
		if f.Severity == SevFail {
			return true
		}
	}
	return false
}

// Failures returns the failing findings' checks and details.
func (r *Result) Failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, f := range r.Findings {
		if f.Severity == SevFail {
			out = append(out, f.Check+": "+f.Detail)
		}
	}
	return out
}

// Finish computes OK and the duration, and returns the JSON line.
func (r *Result) Finish(err error) []byte {
	if err != nil {
		r.Error = err.Error()
	}
	r.OK = !r.Failed()
	r.DurationSeconds = time.Since(r.Started).Round(time.Millisecond).Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(r)
	return b
}

// sleep waits d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// joinMax joins at most n items, noting how many were left out.
func joinMax(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" (+%d more)", len(items)-n)
}

// managedBy reports whether Argo CD Application app tracks o, by either tracking
// method: the annotation (default since Argo CD 3.0) or the instance label.
func managedBy(o kube.Object, app string) bool {
	if app == "" {
		return false
	}
	if strings.HasPrefix(o.Annotations()["argocd.argoproj.io/tracking-id"], app+":") {
		return true
	}
	return o.Labels()["app.kubernetes.io/instance"] == app
}
