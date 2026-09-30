package argocd

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// repoCert mirrors v1alpha1.RepositoryCertificate. CertData is the known_hosts
// entry's key field as raw bytes (JSON-encodes as base64), exactly what
// `argocd cert add-ssh` sends (util/cert.TokenizeSSHKnownHostsEntry, v3.5.1).
type repoCert struct {
	ServerName  string `json:"serverName"`
	CertType    string `json:"certType"`
	CertSubType string `json:"certSubType"`
	CertData    []byte `json:"certData"`
}

// UpsertSSHKnownHosts adds known_hosts entries to Argo CD
// (POST /api/v1/certificates?upsert=true), one certificate per hostname, so
// Argo CD's own clone of an SSH repo on a custom Git host verifies its key.
// Hashed hostnames (|1|…) cannot be expanded and are returned as skipped.
func (c *Client) UpsertSSHKnownHosts(ctx context.Context, knownHosts string) (added int, skipped []string, err error) {
	var items []repoCert
	for _, line := range strings.Split(knownHosts, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			skipped = append(skipped, line)
			continue
		}
		if strings.HasPrefix(f[0], "@") { // @cert-authority / @revoked markers
			skipped = append(skipped, line)
			continue
		}
		_, hosts, key, _, _, perr := ssh.ParseKnownHosts([]byte(line))
		if perr != nil {
			return 0, nil, fmt.Errorf("argocd: known_hosts line %q: %w", line, perr)
		}
		for _, h := range hosts {
			if strings.HasPrefix(h, "|1|") {
				skipped = append(skipped, h+" (hashed)")
				continue
			}
			// Argo CD accepts only literal host names (util/db IsValidHostname);
			// one pattern would make it reject the whole request.
			if strings.ContainsAny(h, "*?!") {
				skipped = append(skipped, h+" (pattern)")
				continue
			}
			items = append(items, repoCert{ServerName: h, CertType: "ssh", CertSubType: key.Type(), CertData: []byte(f[2])})
		}
	}
	if len(items) == 0 {
		return 0, skipped, nil
	}
	body := map[string]any{"items": items}
	if err := c.do(ctx, "add SSH known hosts", "POST", "/api/v1/certificates?upsert=true", body, nil); err != nil {
		return 0, skipped, err
	}
	return len(items), skipped, nil
}

// UpsertTLSCert adds a CA certificate Argo CD trusts for HTTPS repositories on
// serverName (POST /api/v1/certificates?upsert=true, certType https), as
// `argocd cert add-tls` does: a private-CA registry's charts then pull without
// skipping verification. serverName is a host name without a port: Argo CD
// matches TLS certificates by host.
func (c *Client) UpsertTLSCert(ctx context.Context, serverName, pemData string) error {
	if strings.Contains(serverName, ":") || serverName == "" {
		return fmt.Errorf("argocd: TLS certificate server name %q must be a host name without a port", serverName)
	}
	body := map[string]any{"items": []repoCert{{ServerName: serverName, CertType: "https", CertData: []byte(pemData)}}}
	return c.do(ctx, "add TLS certificate for "+serverName, "POST", "/api/v1/certificates?upsert=true", body, nil)
}
