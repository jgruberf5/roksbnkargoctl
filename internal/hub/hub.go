// Package hub builds a TEST Argo CD hub: single-node k3s with upstream Argo CD
// on a VSI with a floating IP, attached to the transit gateway — the shape of
// roksbnkctl's bnk-hub-argocd demo hub, done through the IBM APIs.
//
// It exists so the install can be exercised end to end without a customer's
// Argo CD. It is not a production Argo CD.
package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Account is the Argo CD local account the tool mints an API token for.
const Account = "roksbnkargoctl"

// Params fill the cloud-init.
type Params struct {
	ArgoCDVersion string // e.g. 3.5.1
	K3sChannel    string
	NodePort      int
	AdminPassword string // plaintext; only its bcrypt hash goes into user data
}

// NewPassword returns a random admin password.
func NewPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const cloudInit = `#cloud-config
# TEST Argo CD hub (k3s + upstream Argo CD {{.Version}}), rendered by roksbnkargoctl
# ` + "`argocd up`" + `. Not a production Argo CD.
package_update: true
packages: [curl, jq]
write_files:
  - path: /usr/local/bin/wait-argocd
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
      for i in $(seq 1 90); do
        kubectl -n argocd rollout status deploy/argocd-server --timeout=10s >/dev/null 2>&1 &&
        kubectl -n argocd rollout status deploy/argocd-repo-server --timeout=10s >/dev/null 2>&1 &&
        kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=10s >/dev/null 2>&1 && exit 0
        sleep 10
      done
      exit 1
  - path: /opt/argocd/admin-patch.json
    permissions: '0600'
    content: |
      {"stringData":{"admin.password":"{{.AdminHash}}","admin.passwordMtime":"{{.Mtime}}"}}
  - path: /opt/argocd/cm-patch.json
    permissions: '0644'
    content: |
      {"data":{"accounts.{{.Account}}":"apiKey","accounts.{{.Account}}.enabled":"true"}}
  - path: /opt/argocd/rbac-patch.json
    permissions: '0644'
    content: |
      {"data":{"policy.csv":"g, {{.Account}}, role:admin\n"}}
runcmd:
  - [ bash, -c, "curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL={{.Channel}} INSTALL_K3S_EXEC='--write-kubeconfig-mode 600' sh -" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; until kubectl get nodes >/dev/null 2>&1; do sleep 3; done; kubectl create namespace argocd" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; kubectl apply -n argocd --server-side --force-conflicts -f https://raw.githubusercontent.com/argoproj/argo-cd/v{{.Version}}/manifests/install.yaml" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; kubectl -n argocd patch svc argocd-server --type merge -p '{\"spec\":{\"type\":\"NodePort\",\"ports\":[{\"name\":\"http\",\"port\":80,\"targetPort\":8080,\"nodePort\":30080},{\"name\":\"https\",\"port\":443,\"targetPort\":8080,\"nodePort\":{{.NodePort}}}]}}'" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; /usr/local/bin/wait-argocd" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; kubectl -n argocd patch secret argocd-secret --type merge --patch-file /opt/argocd/admin-patch.json && kubectl -n argocd delete secret argocd-initial-admin-secret --ignore-not-found && rm -f /opt/argocd/admin-patch.json" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; kubectl -n argocd patch configmap argocd-cm --type merge --patch-file /opt/argocd/cm-patch.json && kubectl -n argocd patch configmap argocd-rbac-cm --type merge --patch-file /opt/argocd/rbac-patch.json" ]
  - [ bash, -c, "export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; kubectl -n argocd rollout restart deploy/argocd-server && /usr/local/bin/wait-argocd && touch /var/lib/cloud/argocd-hub-ready" ]
final_message: "Argo CD test hub ready on NodePort {{.NodePort}}."
`

// CloudInit renders the hub's user data. Only the bcrypt hash of the admin
// password is included.
func CloudInit(p Params) (string, error) {
	if p.ArgoCDVersion == "" || p.AdminPassword == "" || p.NodePort == 0 {
		return "", errors.New("hub cloud-init: version, admin password and node port are required")
	}
	if p.K3sChannel == "" {
		p.K3sChannel = "stable"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	t, err := template.New("hub").Option("missingkey=error").Parse(cloudInit)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, map[string]any{
		"Version": strings.TrimPrefix(p.ArgoCDVersion, "v"), "Channel": p.K3sChannel, "NodePort": p.NodePort,
		"AdminHash": string(hash), "Mtime": time.Now().UTC().Format(time.RFC3339), "Account": Account,
	})
	return buf.String(), err
}

// URL is the hub's Argo CD API/UI address.
func URL(floatingIP string, nodePort int) string {
	return fmt.Sprintf("https://%s:%d", floatingIP, nodePort)
}

// insecureClient talks to the hub's self-signed Argo CD.
func insecureClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test hub, self-signed by design
	}}
}

// WaitReady polls /api/version until Argo CD answers (cloud-init takes ~5–8 min).
func WaitReady(ctx context.Context, url string, timeout time.Duration, log func(string, ...any)) error {
	deadline := time.Now().Add(timeout)
	c := insecureClient()
	last := ""
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/version", nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if msg := err.Error(); msg != last && log != nil {
			log("waiting for Argo CD at %s (%v)", url, shortErr(err))
			last = msg
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Argo CD at %s not ready after %s: %w", url, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 {
		return s[i+2:]
	}
	return s
}

// MintToken logs in as admin and generates an API token for the
// roksbnkargoctl account (the account cloud-init enabled with apiKey).
// Waits for the account to exist, since the server restarts after the patch.
func MintToken(ctx context.Context, url, adminPassword string, timeout time.Duration) (string, error) {
	c := insecureClient()
	deadline := time.Now().Add(timeout)
	for {
		tok, err := mint(ctx, c, url, adminPassword)
		if err == nil {
			return tok, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func mint(ctx context.Context, c *http.Client, url, pw string) (string, error) {
	var sess struct {
		Token string `json:"token"`
	}
	if err := post(ctx, c, url+"/api/v1/session", "", map[string]string{"username": "admin", "password": pw}, &sess); err != nil {
		return "", fmt.Errorf("admin login: %w", err)
	}
	var tok struct {
		Token string `json:"token"`
	}
	body := map[string]any{"name": Account, "id": "roksbnkargoctl-" + time.Now().UTC().Format("20060102T150405")}
	if err := post(ctx, c, url+"/api/v1/account/"+Account+"/token", sess.Token, body, &tok); err != nil {
		return "", fmt.Errorf("generating the %s token: %w", Account, err)
	}
	if tok.Token == "" {
		return "", errors.New("Argo CD returned an empty token")
	}
	return tok.Token, nil
}

func post(ctx context.Context, c *http.Client, url, bearer string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	return json.Unmarshal(rb, out)
}
