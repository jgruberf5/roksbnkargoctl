package checks

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube"
	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/kube/kubefake"
)

type tlog struct {
	t  *testing.T
	mu sync.Mutex
}

func (w *tlog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newFake(t *testing.T) (*kubefake.Server, *Env, *Result) {
	t.Helper()
	s := kubefake.New()
	t.Cleanup(s.Close)
	lw := &tlog{t: t}
	return s, &Env{Kube: s.Client(), Log: lw}, NewResult(t.Name(), lw)
}

func obj(apiVersion, kind, ns, name string) kube.Object {
	return kubefake.Obj(apiVersion, kind, ns, name)
}

func with(o kube.Object, path []string, v any) kube.Object {
	cur := map[string]any(o)
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
	return o
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func findingSev(r *Result, check string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f.Severity)
		}
	}
	return out
}

func hasSev(r *Result, check, sev string) bool {
	for _, s := range findingSev(r, check) {
		if s == sev {
			return true
		}
	}
	return false
}

func detail(r *Result, check string) string {
	var out []string
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f.Detail)
		}
	}
	return strings.Join(out, " | ")
}

func deletes(reqs []kubefake.Request, contains string) []string {
	var out []string
	for _, r := range reqs {
		if r.Method == "DELETE" && strings.Contains(r.Path, contains) {
			out = append(out, r.Path)
		}
	}
	return out
}

func conditions(kv ...string) []any {
	var out []any
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, map[string]any{"type": kv[i], "status": kv[i+1]})
	}
	return out
}
