package argocd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flaky answers each poll in turn: a cut connection, a 502, a 429, then ok().
func flaky(t *testing.T, ok func(w http.ResponseWriter)) (*Client, *atomic.Int32) {
	var n atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch n.Add(1) {
		case 1: // no answer at all: what a hub that stops responding looks like
			hj, okj := w.(http.Hijacker)
			if !okj {
				t.Fatal("cannot hijack")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		case 2:
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "upstream"})
		case 3:
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"message": "slow down"})
		default:
			ok(w)
		}
	})
	return c, &n
}

// A wait does not end on a read that got no answer, a 5xx or a 429: the sync
// carries on in Argo CD (run 94: one timed-out read ended an upgrade whose
// sync succeeded).
func TestWaitOperationRidesOutTransientErrors(t *testing.T) {
	c, n := flaky(t, func(w http.ResponseWriter) {
		writeJSON(w, 200, opApp(false, PhaseSucceeded, "successfully synced", nil))
	})
	res, err := c.WaitOperation(context.Background(), "bnk-demo", 10*time.Second, nil)
	if err != nil || !res.Succeeded() {
		t.Fatalf("got %v, %v after %d polls", res, err, n.Load())
	}
	if n.Load() != 4 {
		t.Errorf("polls = %d, want 4 (three transient failures, then the answer)", n.Load())
	}
}

func TestWaitGoneRidesOutTransientErrors(t *testing.T) {
	c, n := flaky(t, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 5, "message": "not found"})
	})
	if err := c.WaitGone(context.Background(), "bnk-demo", 10*time.Second, nil); err != nil {
		t.Fatalf("WaitGone: %v after %d polls", err, n.Load())
	}
}

// An answer that says no is not retried: a revoked token fails the wait at
// once instead of spinning until the timeout.
func TestWaitStopsOnARealRefusal(t *testing.T) {
	for name, wait := range map[string]func(*Client) error{
		"WaitOperation": func(c *Client) error {
			_, err := c.WaitOperation(context.Background(), "bnk-demo", 10*time.Second, nil)
			return err
		},
		"WaitGone": func(c *Client) error { return c.WaitGone(context.Background(), "bnk-demo", 10*time.Second, nil) },
	} {
		var n atomic.Int32
		c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			n.Add(1)
			writeJSON(w, http.StatusForbidden, map[string]any{"code": 7, "message": "permission denied"})
		})
		start := time.Now()
		err := wait(c)
		if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "timed out") {
			t.Errorf("%s: want the 403 at once, got %v", name, err)
		}
		if n.Load() != 1 || time.Since(start) > 2*time.Second {
			t.Errorf("%s: %d polls in %s, want 1 at once", name, n.Load(), time.Since(start))
		}
	}
}

// Running out of time while the hub only fails says so, and names the failure.
func TestWaitTimeoutNamesTheLastFailure(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "hub overloaded"})
	})
	_, err := c.WaitOperation(context.Background(), "bnk-demo", 300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "hub overloaded") {
		t.Errorf("got %v", err)
	}
}

func TestIsTransient(t *testing.T) {
	// Built at run time: a literal malformed URL is what staticcheck (SA1007) flags,
	// and a malformed URL is exactly what this test needs.
	badURL := "https://hub:" + strings.Repeat("x", 3)
	_, parseErr := url.Parse(badURL)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no answer: url.Error", &url.Error{Op: "Get", URL: "https://hub", Err: errors.New("i/o timeout")}, true},
		{"cut-off response", io.ErrUnexpectedEOF, true},
		{"503", &APIError{StatusCode: http.StatusServiceUnavailable}, true},
		{"429", &APIError{StatusCode: http.StatusTooManyRequests}, true},
		{"403", &APIError{StatusCode: http.StatusForbidden}, false},
		{"401", &APIError{StatusCode: http.StatusUnauthorized}, false},
		{"404", &APIError{StatusCode: http.StatusNotFound}, false},
		{"anything else", errors.New("encode request: unsupported type"), false},
	} {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("%s: IsTransient = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A malformed server URL never heals: the client must not report it as a
	// network failure (url.Parse's error is a *url.Error too).
	if parseErr == nil {
		t.Fatal("expected a parse error")
	}
	c := New(badURL, "tok", true, nil)
	_, err := c.GetApplication(context.Background(), "x", "")
	if err == nil || IsTransient(err) {
		t.Errorf("bad server URL: err %v, transient %v; want a permanent error", err, IsTransient(err))
	}
}
