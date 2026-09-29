package ibm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type staticToken string

func (s staticToken) GetToken() (string, error) { return string(s), nil }

// recorded is one request the fake server saw.
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// fakeIBM is an httptest server standing in for every IBM endpoint. All base
// URLs of the client it returns point at it, under distinct prefixes.
type fakeIBM struct {
	*httptest.Server
	mux *http.ServeMux
	mu  sync.Mutex
	req []recorded
}

func newFake(t *testing.T) (*fakeIBM, *Client) {
	t.Helper()
	f := &fakeIBM{mux: http.NewServeMux()}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.req = append(f.req, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad auth "+got, http.StatusUnauthorized)
			return
		}
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	// The account id is served for every test that needs it.
	f.mux.HandleFunc("GET /iam/v1/apikeys/details", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("IAM-ApiKey") != "key" {
			http.Error(w, "no key", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"account_id": "acct-1", "iam_id": "IBMid-x"})
	})
	c := &Client{
		apiKey:              "key",
		region:              "us-south",
		auth:                staticToken("test-token"),
		http:                f.Client(),
		iamURL:              f.URL + "/iam",
		rcURL:               f.URL + "/rc",
		containersURL:       f.URL + "/containers",
		transitURL:          f.URL + "/tgw",
		vpcURL:              f.URL + "/vpc",
		pollInterval:        time.Millisecond,
		kubeconfigAttempts:  3,
		kubeconfigRetryWait: time.Millisecond,
		acct:                &accountCache{},
	}
	return f, c
}

func (f *fakeIBM) requests(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.req {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody unmarshals a recorded JSON body into a generic map.
func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, b)
	}
	return m
}

// dig walks a decoded JSON value by map keys and slice indexes.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	cur := v
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("dig %v: %T is not an object at %q", path, cur, k)
			}
			cur = m[k]
		case int:
			s, ok := cur.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("dig %v: no index %d in %v", path, k, cur)
			}
			cur = s[k]
		}
	}
	return cur
}
