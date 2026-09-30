package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const wsDeleteBase = "cluster: c\ntransit_gateway: t\n"

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// A workspace that records nothing in the cloud is deleted, and the current
// workspace is unset only when it named the deleted one.
func TestWorkspacesDeleteRemovesAWorkspace(t *testing.T) {
	home := isolate(t)
	writeWorkspace(t, home, "gone", wsDeleteBase)
	writeWorkspace(t, home, "kept", wsDeleteBase)
	if err := setCurrent("gone"); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "ws", "delete", "gone", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(filepath.Join(home, "gone")) {
		t.Fatal("the workspace directory is still there")
	}
	if !exists(filepath.Join(home, "kept", "config.yaml")) {
		t.Fatal("another workspace was touched")
	}
	if cur := currentWorkspace(); cur != "" {
		t.Errorf("current workspace %q after deleting it", cur)
	}

	if err := setCurrent("kept"); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, home, "other", wsDeleteBase)
	if out, err := runRoot(t, "workspaces", "rm", "other", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if cur := currentWorkspace(); cur != "kept" {
		t.Errorf("deleting another workspace changed the current one to %q", cur)
	}
}

// While the workspace records an install, an FLP or a test hub, delete refuses,
// names each and the command that removes it, and deletes nothing.
func TestWorkspacesDeleteRefusesWhileItRecordsCloudResources(t *testing.T) {
	home := isolate(t)
	dir := writeWorkspace(t, home, "busy", wsDeleteBase+"resolved: {cluster_id: cid, trusted_profile_id: Profile-1}\n")
	wsDir := filepath.Dir(dir)
	for _, f := range []string{"flp-outputs.json", "argocd-hub.json"} {
		if err := os.WriteFile(filepath.Join(wsDir, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runRoot(t, "ws", "delete", "busy", "--yes")
	if err == nil {
		t.Fatal("deleted a workspace that records cloud resources")
	}
	for _, want := range []string{"trusted profile Profile-1", "roksbnkargoctl uninstall -w busy",
		"F5 License Proxy", "roksbnkargoctl flp down -w busy", "test Argo CD hub", "roksbnkargoctl argocd down -w busy", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
	if !exists(filepath.Join(wsDir, "config.yaml")) || !exists(filepath.Join(wsDir, "flp-outputs.json")) {
		t.Fatal("a refused delete removed files")
	}

	// Each record blocks on its own.
	for _, f := range []string{"flp-outputs.json", "argocd-hub.json"} {
		d := filepath.Dir(writeWorkspace(t, home, "one", wsDeleteBase))
		if err := os.WriteFile(filepath.Join(d, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runRoot(t, "ws", "delete", "one", "--yes"); err == nil || !exists(d) {
			t.Errorf("%s alone did not block the delete (err %v)", f, err)
		}
		_ = os.RemoveAll(d)
	}
}

// --force deletes anyway and says what is left without a record.
func TestWorkspacesDeleteForce(t *testing.T) {
	home := isolate(t)
	d := filepath.Dir(writeWorkspace(t, home, "busy", wsDeleteBase+"resolved: {cluster_id: cid, trusted_profile_id: Profile-1}\n"))
	if err := os.WriteFile(filepath.Join(d, "flp-outputs.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "ws", "delete", "busy", "--force", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(d) {
		t.Fatal("--force did not delete the workspace")
	}
	for _, want := range []string{"left without a local record (--force)", "Profile-1", "F5 License Proxy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A transit gateway connection install created is reported but does not block:
// a normal uninstall leaves it attached.
func TestWorkspacesDeleteWarnsAboutTheTGWConnection(t *testing.T) {
	home := isolate(t)
	d := filepath.Dir(writeWorkspace(t, home, "tgw", wsDeleteBase+"resolved: {cluster_id: cid, tgw_connection_created_id: conn-9}\n"))
	out, err := runRoot(t, "ws", "delete", "tgw", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(d) {
		t.Fatal("not deleted")
	}
	if !strings.Contains(out, "conn-9") || !strings.Contains(out, "--detach-tgw") {
		t.Errorf("no warning about the TGW connection:\n%s", out)
	}
}

// Without --yes and without a terminal to ask on, nothing is deleted; a
// workspace that does not exist, or a name that is not one, is an error.
func TestWorkspacesDeleteNeedsConfirmationAndAWorkspace(t *testing.T) {
	home := isolate(t)
	d := filepath.Dir(writeWorkspace(t, home, "keep", wsDeleteBase))
	if _, err := runRoot(t, "ws", "delete", "keep"); err == nil || !strings.Contains(err.Error(), "not confirmed") || !exists(d) {
		t.Errorf("unconfirmed delete: err %v, exists %v", err, exists(d))
	}
	if _, err := runRoot(t, "ws", "delete", "nosuch", "--yes"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing workspace: %v", err)
	}
	outside := filepath.Join(filepath.Dir(home), "victim")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "ws", "delete", "../victim", "--yes", "--force"); err == nil || !exists(outside) {
		t.Errorf("a path outside the workspaces was accepted: err %v", err)
	}
	// A directory without config.yaml is not a workspace.
	bare := filepath.Join(home, "bare")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "ws", "delete", "bare", "--yes", "--force"); err == nil || !exists(bare) {
		t.Errorf("a directory without config.yaml was deleted: err %v", err)
	}
}

// An unreadable config.yaml blocks unless --force.
func TestWorkspacesDeleteUnreadableConfig(t *testing.T) {
	home := isolate(t)
	d := filepath.Dir(writeWorkspace(t, home, "bad", "cluster: [unclosed\n"))
	if _, err := runRoot(t, "ws", "delete", "bad", "--yes"); err == nil || !strings.Contains(err.Error(), "--force") || !exists(d) {
		t.Errorf("unreadable config deleted without --force: %v", err)
	}
	if out, err := runRoot(t, "ws", "delete", "bad", "--yes", "--force"); err != nil || exists(d) {
		t.Errorf("--force on an unreadable config: %v\n%s", err, out)
	}
}
