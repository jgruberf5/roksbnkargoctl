// Package argocd is a small REST client for the Argo CD API server (Argo CD 3.3–3.5).
//
// It speaks the grpc-gateway JSON API under /api/v1 directly over net/http, so the tool
// never needs the argocd CLI. Endpoints and body shapes were checked against the upstream
// swagger (argo-cd v3.5.1, assets/swagger.json) and the server sources
// (server/application, server/cluster, server/repository).
package argocd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultProject is the Argo CD project used when Client.Project is empty.
const DefaultProject = "default"

// Client talks to one Argo CD API server with one bearer token.
type Client struct {
	server string
	token  string
	hc     *http.Client

	// Project is sent as the `project` query parameter on per-Application reads and
	// deletes. Argo CD answers a GET for a missing Application with 403 unless the
	// request names a project (server/application getAppEnforceRBAC), so without it a
	// missing app and a forbidden app are indistinguishable. Set it to the project the
	// Application lives in. New sets it to "default".
	Project string

	// PollInterval is how often the Wait* helpers poll. New sets it to 3s.
	PollInterval time.Duration
}

// New returns a client for server (for example https://10.0.0.5:30443) authenticating
// with a bearer token. insecureTLS skips certificate verification; caPEM, when non-empty,
// is trusted in addition to the system roots.
func New(server, token string, insecureTLS bool, caPEM []byte) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecureTLS {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // operator opted in
	} else if len(caPEM) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(caPEM)
		tlsCfg.RootCAs = pool
	}
	tr.TLSClientConfig = tlsCfg
	return &Client{
		server:       strings.TrimRight(server, "/"),
		token:        token,
		hc:           &http.Client{Transport: tr, Timeout: 60 * time.Second},
		Project:      DefaultProject,
		PollInterval: 3 * time.Second,
	}
}

// Server returns the API server base URL.
func (c *Client) Server() string { return c.server }

// APIError is a non-2xx answer from the Argo CD API server.
type APIError struct {
	Op         string // what we were doing, e.g. "create application bnk-x"
	StatusCode int    // HTTP status
	Code       int    // gRPC code from the body (5 = NotFound, 7 = PermissionDenied, 16 = Unauthenticated)
	Message    string // Argo CD's message (or the raw body)
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Sprintf("argocd: %s: token rejected (401): %s — check ARGOCD_AUTH_TOKEN is a valid, unexpired Argo CD token", e.Op, msg)
	case http.StatusForbidden:
		return fmt.Sprintf("argocd: token lacks permission for %s (403): %s — Argo CD also answers 403 when the object does not exist", e.Op, msg)
	}
	return fmt.Sprintf("argocd: %s: HTTP %d: %s", e.Op, e.StatusCode, msg)
}

// IsNotFound reports whether err is an Argo CD "not found" answer (HTTP 404 or gRPC code 5).
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.Code == 5)
}

// IsForbidden reports whether err is an HTTP 403 from Argo CD.
func IsForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusForbidden
}

// IsUnauthorized reports whether err is an HTTP 401 from Argo CD.
func IsUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusUnauthorized
}

func notFound(op, msg string) error {
	return &APIError{Op: op, StatusCode: http.StatusNotFound, Code: 5, Message: msg}
}

// pathEscape escapes a value for use as one path segment, like JavaScript's
// encodeURIComponent (which is what the Argo CD UI uses for cluster and repo URLs).
func pathEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// do sends a request. path may carry a query string; body, when non-nil, is sent as JSON;
// out, when non-nil, receives the decoded 2xx answer.
func (c *Client) do(ctx context.Context, op, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("argocd: %s: encode request: %w", op, err)
		}
		rdr = bytes.NewReader(b)
	}
	u, err := url.Parse(c.server + path)
	if err != nil {
		return fmt.Errorf("argocd: %s: bad URL: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return fmt.Errorf("argocd: %s: %w", op, err)
	}
	req.URL = u // keep RawPath so %2F in a cluster/repo URL segment survives
	// Argo CD's gateway answers a body-less DELETE without a Content-Type with
	// 415 "Invalid content type" (found live on 3.5.1, deleting an Application),
	// so every non-GET request declares JSON whether or not it has a body.
	if body != nil || method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("argocd: %s: %w", op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("argocd: %s: read response: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Op: op, StatusCode: resp.StatusCode}
		var rt struct {
			Error   string `json:"error"`
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &rt) == nil && (rt.Message != "" || rt.Error != "") {
			ae.Code = rt.Code
			ae.Message = rt.Message
			if ae.Message == "" {
				ae.Message = rt.Error
			}
		} else {
			ae.Message = strings.TrimSpace(string(data))
			if len(ae.Message) > 512 {
				ae.Message = ae.Message[:512] + "…"
			}
		}
		return ae
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("argocd: %s: decode response: %w", op, err)
		}
	}
	return nil
}
