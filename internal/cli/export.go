package cli

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// exportReadme is the instructions file at the root of an export.
const exportReadme = "ROKSBNKARGOCTL-EXPORT.md"

// exportApplication is the Argo CD Application install would create, for reference.
const exportApplication = "roksbnkargoctl-application.yaml"

// exportEpoch is every entry's modification time: an unchanged render exports
// byte-identical zips, so a re-export shows no spurious difference.
var exportEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func newExportCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Zip the rendered manifests, laid out for you to commit to Git yourself",
		Long: `export packs the workspace's rendered Git content (manifests/git, written by
` + "`render`" + `) into a zip whose paths start at git.path, so unzipping it at the root of
your repository puts every file where the Argo CD Application reads them.

Use it when roksbnkargoctl should not push to your repository: commit and push
the files yourself, then run ` + "`roksbnkargoctl install --no-publish`" + `.

The zip never contains a Secret: the pull secret, the license JWT and the FLP CA
are written straight into ROKS by install, never to Git. It also holds
` + exportReadme + ` (these instructions) and ` + exportApplication + `
(the Application install creates, for reference).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			gitDir := filepath.Join(s.ws.ManifestsDir(), "git")
			if _, err := os.Stat(gitDir); err != nil {
				return fmt.Errorf("nothing rendered in %s: run `roksbnkargoctl render` first", gitDir)
			}
			if stale(s.ws.ConfigPath(), gitDir) {
				s.p.warn("config.yaml changed after the last render; run `roksbnkargoctl render` to export the current configuration")
			}
			if output == "" {
				output = "roksbnkargoctl-" + s.ws.Name + ".zip"
			}
			app, _ := os.ReadFile(filepath.Join(s.ws.ManifestsDir(), "application.yaml"))
			var buf bytes.Buffer
			n, err := writeExport(&buf, gitDir, s.cfg.Git.Path, exportInstructions(s.ws.Name, s.cfg.Git.URL, s.cfg.Git.Branch, s.cfg.Git.Path), app)
			if err != nil {
				return err
			}
			if err := os.WriteFile(output, buf.Bytes(), 0o600); err != nil {
				return err
			}
			s.p.ok("exported %d manifests under %s/ to %s", n, s.cfg.Git.Path, output)
			s.p.info("unzip it at the root of %s (branch %s), commit and push, then run `roksbnkargoctl install --no-publish`", s.cfg.Git.URL, s.cfg.Git.Branch)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "zip file to write (default roksbnkargoctl-<workspace>.zip)")
	return cmd
}

// writeExport writes the zip: every file of gitDir under gitPath/, the
// instructions and the reference Application. It refuses a Secret: an export
// is meant to be committed, and no secret may reach Git.
func writeExport(w io.Writer, gitDir, gitPath, readme string, application []byte) (int, error) {
	gitPath = strings.Trim(path.Clean(filepath.ToSlash(gitPath)), "/")
	if gitPath == "" || gitPath == "." || strings.HasPrefix(gitPath, "..") {
		return 0, fmt.Errorf("git.path %q is not a relative path inside a repository", gitPath)
	}
	ents, err := os.ReadDir(gitDir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return 0, fmt.Errorf("%s has no manifests: run `roksbnkargoctl render`", gitDir)
	}
	sort.Strings(names)
	zw := zip.NewWriter(w)
	add := func(name string, b []byte) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: exportEpoch})
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		return err
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(gitDir, n))
		if err != nil {
			return 0, err
		}
		var obj struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(b, &obj); err != nil {
			return 0, fmt.Errorf("%s: %w", n, err)
		}
		if obj.Kind == "Secret" {
			return 0, fmt.Errorf("%s is a Secret; refusing to export it for Git", n)
		}
		if err := add(gitPath+"/"+n, b); err != nil {
			return 0, err
		}
	}
	if err := add(exportReadme, []byte(readme)); err != nil {
		return 0, err
	}
	if len(application) > 0 {
		if err := add(exportApplication, application); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	return len(names), nil
}

// stale reports whether config was modified after the render wrote dir.
func stale(config, dir string) bool {
	c, err1 := os.Stat(config)
	d, err2 := os.Stat(dir)
	if errors.Join(err1, err2) != nil {
		return false
	}
	return c.ModTime().After(d.ModTime())
}

func exportInstructions(ws, url, branch, gitPath string) string {
	return fmt.Sprintf(`# roksbnkargoctl export: workspace %[1]s

This zip is the complete Git content of the BNK install for workspace %[1]s.
Argo CD syncs it from:

    repository: %[2]s
    branch:     %[3]s
    path:       %[4]s

1. Unzip it at the root of that repository. The manifests land in %[4]s/.
   Replace that directory's previous contents: files left over from an earlier
   export would be synced too.
2. Commit and push to %[3]s.
3. Run:

       roksbnkargoctl install --no-publish -w %[1]s

   install does everything except push to Git: the IAM trusted profile, the
   Secrets written straight into ROKS, the cluster registration in Argo CD, the
   Application and the sync.

Not in this zip, by design: Secrets (the registry pull secret, the license JWT,
the FLP CA). install writes them into ROKS directly; they never go to Git.

%[5]s is the Argo CD Application install creates, for reference.
Do not apply it by hand: install also registers the cluster and writes the
Secrets it depends on.
`, ws, url, branch, gitPath, exportApplication)
}
