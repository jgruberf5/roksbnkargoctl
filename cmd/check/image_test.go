package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
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
	// What the Dockerfile copies from the context: COPY lines, not comments,
	// and not from another stage.
	copied := map[string]bool{}
	for _, line := range strings.Split(dockerfile, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.EqualFold(f[0], "COPY") || slices.ContainsFunc(f, func(x string) bool { return strings.HasPrefix(x, "--from") }) {
			continue
		}
		for _, src := range f[1 : len(f)-1] {
			if !strings.HasPrefix(src, "--") {
				copied[strings.TrimSuffix(src, "/")] = true
			}
		}
	}
	// The module files the build needs, beside the packages.
	for _, f := range []string{"go.mod", "go.sum"} {
		if !copied[f] {
			t.Errorf("the Dockerfile does not copy %s", f)
		}
	}
	if pm, err := patternmatcher.New(mustPatterns(t, ignore)); err != nil {
		t.Fatal(err)
	} else {
		for _, f := range []string{"go.mod", "go.sum"} {
			if m, _ := pm.MatchesOrParentMatches(f); m {
				t.Errorf("Dockerfile.dockerignore keeps %s out of the build context", f)
			}
		}
	}
	workflow := read(".github/workflows/check-image.yml")
	for dir := range seen {
		if f := excluded(t, root, ignore, dir); f != "" {
			t.Errorf("the check binary imports %s, but Dockerfile.dockerignore keeps %s out of the build context", dir, f)
		}
		if strings.HasPrefix(dir, "cmd/check") {
			continue
		}
		if strings.Count(workflow, "'"+dir+"/**'") != 2 {
			t.Errorf("the check-image workflow's push and pull_request paths do not both name %s/**", dir)
		}
		if !copied[dir] {
			t.Errorf("the Dockerfile does not copy %s, which the check binary imports", dir)
		}
	}
	if !seen["internal/singlecert"] {
		t.Error("the walk did not reach internal/singlecert: the test is not reading imports")
	}
}

// excluded returns the first of a package's non-test source files the
// dockerignore keeps out of the build context, judged by Docker's own matcher
// (moby/patternmatcher, as BuildKit reads .dockerignore); "" when all are in.
func excluded(t *testing.T, root, ignore, dir string) string {
	t.Helper()
	patterns, err := ignorefile.ReadAll(strings.NewReader(ignore))
	if err != nil {
		t.Fatal(err)
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f := dir + "/" + e.Name()
		if m, err := pm.MatchesOrParentMatches(f); err != nil {
			t.Fatal(err)
		} else if m {
			return f
		}
	}
	return ""
}

func mustPatterns(t *testing.T, ignore string) []string {
	t.Helper()
	p, err := ignorefile.ReadAll(strings.NewReader(ignore))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
