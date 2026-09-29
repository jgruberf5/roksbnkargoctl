package ibm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// APIError is a non-2xx response from an IBM Cloud API.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s returned HTTP %d: %s", e.Method, redactQuery(e.URL), e.StatusCode, e.Body)
}

// redactQuery drops the query string from a URL for error messages; queries
// carry nothing secret today, but pagination tokens make them long and noisy.
func redactQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// IsNotFound reports whether err is (or wraps) an HTTP 404 from an IBM API.
func IsNotFound(err error) bool { return statusOf(err) == http.StatusNotFound }

// IsConflict reports whether err is (or wraps) an HTTP 409 from an IBM API.
func IsConflict(err error) bool { return statusOf(err) == http.StatusConflict }

func statusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// token returns an IAM bearer token from the shared authenticator.
func (c *Client) token() (string, error) {
	if c.auth == nil {
		return "", errors.New("IBM client has no authenticator (construct it with ibm.New)")
	}
	t, err := c.auth.GetToken()
	if err != nil {
		return "", fmt.Errorf("getting IAM token: %w", err)
	}
	return t, nil
}

func (c *Client) httpClient() *http.Client {
	if c.http != nil {
		return c.http
	}
	return http.DefaultClient
}

// do sends an authenticated JSON request. in (if non-nil) is marshalled as the
// body; out (if non-nil) receives the decoded 2xx response. Non-2xx returns an
// *APIError. headers are extra request headers.
func (c *Client) do(ctx context.Context, method, url string, headers map[string]string, in, out any) error {
	raw, err := c.doRaw(ctx, method, url, headers, in)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parsing %s %s response: %w", method, redactQuery(url), err)
	}
	return nil
}

// doRaw is do without response decoding.
func (c *Client) doRaw(ctx context.Context, method, url string, headers map[string]string, in any) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encoding %s body: %w", method, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "roksbnkargoctl")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, redactQuery(url), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s %s response: %w", method, redactQuery(url), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 4096 {
			snippet = snippet[:4096]
		}
		return nil, &APIError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: snippet}
	}
	return raw, nil
}

// del issues a DELETE and treats 404 as success, so teardown is idempotent.
func (c *Client) del(ctx context.Context, url string) error {
	err := c.do(ctx, http.MethodDelete, url, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// nextHref is the VPC / Transit Gateway pagination cursor.
type nextHref struct {
	Next *struct {
		Href string `json:"href"`
	} `json:"next"`
}

func (n nextHref) href() string {
	if n.Next == nil {
		return ""
	}
	return n.Next.Href
}

// listPaged walks a VPC-style collection: each page is decoded by decode, which
// returns the next.href cursor ("" on the last page).
//
// IBM's next.href omits the version (and generation) query parameters the VPC
// API requires on every call, and a page-two GET without them is a 400
// "missing_version". So the first URL's are carried onto every next page.
func (c *Client) listPaged(ctx context.Context, url string, decode func([]byte) (string, error)) error {
	carry := carriedParams(url)
	for url != "" {
		raw, err := c.doRaw(ctx, http.MethodGet, url, nil, nil)
		if err != nil {
			return err
		}
		next, err := decode(raw)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", redactQuery(url), err)
		}
		url = withParams(next, carry)
	}
	return nil
}

// carriedParams extracts the query parameters every page must repeat.
func carriedParams(raw string) neturl.Values {
	out := neturl.Values{}
	u, err := neturl.Parse(raw)
	if err != nil {
		return out
	}
	q := u.Query()
	for _, k := range []string{"version", "generation"} {
		if v := q.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out
}

// withParams adds carried parameters the next URL lacks.
func withParams(next string, carry neturl.Values) string {
	if next == "" || len(carry) == 0 {
		return next
	}
	u, err := neturl.Parse(next)
	if err != nil {
		return next
	}
	q := u.Query()
	for k := range carry {
		if q.Get(k) == "" {
			q.Set(k, carry.Get(k))
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// poll calls check every c.pollInterval until it reports done, returns an
// error, the timeout passes, or ctx ends. what names the wait for the timeout
// error.
func (c *Client) poll(ctx context.Context, timeout time.Duration, what string, check func() (bool, error)) error {
	interval := c.pollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ref is the {"id": ...} identity the VPC API uses in request bodies.
type ref struct {
	ID string `json:"id"`
}

// nameRef is the {"name": ...} identity (zones, profiles).
type nameRef struct {
	Name string `json:"name"`
}
