package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/jgruberf5/roksbnkargoctl/"

// The image builds from a context limited to what the binary needs. Every
// package of this module the binary imports must be copied by the Dockerfile
// and let through by Dockerfile.dockerignore: internal/singlecert was not,
// and the image failed to build while `go build ./...` passed.
func TestImageContextHoldsEveryImportedPackage(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dockerfile, ignore := read("build/check/Dockerfile"), read("build/check/Dockerfile.dockerignore")
	seen := map[string]bool{}
	var walk func(dir string)
	walk = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if p := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(p, module) {
					walk(strings.TrimPrefix(p, module))
				}
			}
		}
	}
	walk("cmd/check")
	workflow := read(".github/workflows/check-image.yml")
	for dir := range seen {
		if rule := excludedBy(ignore, dir); rule != "" {
			t.Errorf("the check binary imports %s, which Dockerfile.dockerignore excludes (%s)", dir, rule)
		}
		if strings.HasPrefix(dir, "cmd/check") {
			continue
		}
		if strings.Count(workflow, "'"+dir+"/**'") != 2 {
			t.Errorf("the check-image workflow's push and pull_request paths do not both name %s/**", dir)
		}
		if !strings.Contains(dockerfile, "COPY "+dir+"/ ./"+dir+"/") {
			t.Errorf("the Dockerfile does not copy %s, which the check binary imports", dir)
		}
	}
	if !seen["internal/singlecert"] {
		t.Error("the walk did not reach internal/singlecert: the test is not reading imports")
	}
}

// excludedBy applies a .dockerignore to a package directory as Docker does:
// rules in order, the last that matches wins, "!" re-includes, a leading "/"
// is the context root. File patterns (*_test.go) do not exclude a package;
// "*" excludes everything. It returns the deciding rule when the directory is
// excluded, "" when it is in the context.
func excludedBy(ignore, dir string) string {
	decided := ""
	for _, line := range strings.Split(ignore, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		neg := strings.HasPrefix(line, "!")
		pat := strings.TrimPrefix(strings.TrimPrefix(line, "!"), "/")
		var match bool
		switch {
		case pat == "*" || pat == "**":
			match = true
		case strings.Contains(pat, "*") || strings.HasSuffix(pat, ".go"):
			continue // a file pattern
		default:
			pat = strings.TrimSuffix(pat, "/")
			match = dir == pat || strings.HasPrefix(dir+"/", pat+"/")
		}
		if match {
			if neg {
				decided = ""
			} else {
				decided = line
			}
		}
	}
	return decided
}
