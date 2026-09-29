package ibm

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGetClusterParse(t *testing.T) {
	cases := []struct {
		name        string
		body        map[string]any
		wantPrivate string
		wantPublic  string
	}{
		{
			name: "v2 serviceEndpoints",
			body: map[string]any{
				"serviceEndpoints": map[string]any{
					"privateServiceEndpointEnabled": true,
					"privateServiceEndpointURL":     "https://c100.private.us-south.containers.cloud.ibm.com:31234",
					"publicServiceEndpointEnabled":  true,
					"publicServiceEndpointURL":      "https://c100.us-south.containers.cloud.ibm.com:31234",
				},
			},
			wantPrivate: "https://c100.private.us-south.containers.cloud.ibm.com:31234",
			wantPublic:  "https://c100.us-south.containers.cloud.ibm.com:31234",
		},
		{
			name: "v1-compatible top level, private only",
			body: map[string]any{
				"privateServiceEndpointURL": "https://c100.private.us-south.containers.cloud.ibm.com:31234",
			},
			wantPrivate: "https://c100.private.us-south.containers.cloud.ibm.com:31234",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			f.mux.HandleFunc("GET /containers/v2/getCluster", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("cluster") != "my-roks" {
					http.NotFound(w, r)
					return
				}
				b := map[string]any{
					"id": "cl123", "name": "my-roks", "region": "us-south",
					"resourceGroup": "rg1", "resourceGroupName": "default",
					"state": "normal", "type": "openshift",
					"crn":               "crn:v1:bluemix:public:containers-kubernetes:us-south:a/acct-1:cl123::",
					"masterURL":         "https://c100-e.us-south.containers.cloud.ibm.com:31234",
					"masterKubeVersion": "4.16.20_openshift",
					"vpcs":              []string{"r006-vpc"},
					"workerZones":       []string{"us-south-1", "us-south-2", "us-south-3"},
				}
				for k, v := range tc.body {
					b[k] = v
				}
				writeJSON(w, b)
			})
			cl, err := c.GetCluster(context.Background(), "my-roks")
			if err != nil {
				t.Fatal(err)
			}
			if cl.ID != "cl123" || cl.ResourceGroupID != "rg1" || cl.VPCID() != "r006-vpc" || len(cl.WorkerZones) != 3 {
				t.Errorf("parsed cluster wrong: %+v", cl)
			}
			if cl.OpenShiftVersion != "4.16.20" || !cl.IsOpenShift() {
				t.Errorf("OpenShiftVersion = %q", cl.OpenShiftVersion)
			}
			if cl.PrivateServiceEndpointURL != tc.wantPrivate || cl.PublicServiceEndpointURL != tc.wantPublic {
				t.Errorf("endpoints = %q / %q", cl.PrivateServiceEndpointURL, cl.PublicServiceEndpointURL)
			}
			if !strings.HasPrefix(cl.CRN, "crn:v1:") || cl.MasterURL == "" {
				t.Errorf("CRN/MasterURL missing: %+v", cl)
			}
			if got := f.requests("GET", "/containers/v2/getCluster")[0].Header.Get("X-Region"); got != "us-south" {
				t.Errorf("X-Region = %q", got)
			}
		})
	}
}

func TestGetClusterNotFound(t *testing.T) {
	f, c := newFake(t)
	f.mux.HandleFunc("GET /containers/v2/getCluster", http.NotFound)
	_, err := c.GetCluster(context.Background(), "nope")
	if !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("err = %v, want ErrClusterNotFound", err)
	}
}

func adminZip(t *testing.T, kubeconfig string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	add("kubeconfig/", "")
	add("kubeconfig/kube-config.yaml", kubeconfig)
	for n, b := range files {
		add("kubeconfig/"+n, b)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const zipKubeconfig = `apiVersion: v1
kind: Config
current-context: my-roks/admin
clusters:
- name: my-roks
  cluster:
    server: https://c100.private.us-south.containers.cloud.ibm.com:31234
    certificate-authority: ca.pem
users:
- name: admin
  user:
    client-certificate: admin.pem
    client-key: ./admin-key.pem
contexts:
- name: my-roks/admin
  context: {cluster: my-roks, user: admin}
`

func TestFetchAdminKubeconfigSelfContained(t *testing.T) {
	for _, private := range []bool{true, false} {
		t.Run(map[bool]string{true: "private", false: "default"}[private], func(t *testing.T) {
			f, c := newFake(t)
			var calls atomic.Int32
			zipBytes := adminZip(t, zipKubeconfig, map[string]string{
				"admin.pem": "CERT", "admin-key.pem": "KEY", "ca.pem": "CA",
			})
			f.mux.HandleFunc("POST /containers/v2/applyRBACAndGetKubeconfig", func(w http.ResponseWriter, r *http.Request) {
				// The first call hits the propagation window: 404, retried.
				if calls.Add(1) == 1 {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write(zipBytes)
			})
			out, err := c.FetchAdminKubeconfig(context.Background(), "my-roks", private)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Errorf("calls = %d, want 2 (one 404 retry)", calls.Load())
			}
			reqs := f.requests("POST", "/containers/v2/applyRBACAndGetKubeconfig")
			body := decodeBody(t, reqs[len(reqs)-1].Body)
			wantEP := map[bool]string{true: "private", false: ""}[private]
			if body["endpointType"] != wantEP || body["admin"] != true || body["cluster"] != "my-roks" || body["format"] != "zip" {
				t.Errorf("request body = %v", body)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			user := dig(t, doc, "users", 0, "user").(map[string]any)
			cluster := dig(t, doc, "clusters", 0, "cluster").(map[string]any)
			for k, want := range map[string]string{"client-certificate-data": "CERT", "client-key-data": "KEY"} {
				if user[k] != base64.StdEncoding.EncodeToString([]byte(want)) {
					t.Errorf("%s = %v", k, user[k])
				}
			}
			if cluster["certificate-authority-data"] != base64.StdEncoding.EncodeToString([]byte("CA")) {
				t.Errorf("certificate-authority-data = %v", cluster["certificate-authority-data"])
			}
			for _, k := range []string{"client-certificate", "client-key"} {
				if _, ok := user[k]; ok {
					t.Errorf("file reference %s left in kubeconfig", k)
				}
			}
			if _, ok := cluster["certificate-authority"]; ok {
				t.Error("certificate-authority file reference left in kubeconfig")
			}
		})
	}
}

func TestFetchAdminKubeconfigMissingFileIsError(t *testing.T) {
	f, c := newFake(t)
	zipBytes := adminZip(t, zipKubeconfig, map[string]string{"admin.pem": "CERT", "ca.pem": "CA"})
	f.mux.HandleFunc("POST /containers/v2/applyRBACAndGetKubeconfig", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	})
	_, err := c.FetchAdminKubeconfig(context.Background(), "my-roks", false)
	if err == nil || !strings.Contains(err.Error(), "admin-key.pem") {
		t.Fatalf("err = %v, want a missing admin-key.pem error", err)
	}
}

func TestFetchAdminKubeconfigNoRetryOn403(t *testing.T) {
	f, c := newFake(t)
	var calls atomic.Int32
	f.mux.HandleFunc("POST /containers/v2/applyRBACAndGetKubeconfig", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	if _, err := c.FetchAdminKubeconfig(context.Background(), "my-roks", false); err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 1 {
		t.Errorf("403 retried: %d calls", calls.Load())
	}
}
