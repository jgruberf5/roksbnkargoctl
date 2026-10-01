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
	for dir := range seen {
		if strings.HasPrefix(dir, "cmd/check") {
			continue
		}
		if !strings.Contains(dockerfile, "COPY "+dir+"/ ./"+dir+"/") {
			t.Errorf("the Dockerfile does not copy %s, which the check binary imports", dir)
		}
		if !strings.Contains(ignore, "!"+dir+"/") {
			t.Errorf("Dockerfile.dockerignore keeps %s out of the build context", dir)
		}
	}
	if !seen["internal/singlecert"] {
		t.Error("the walk did not reach internal/singlecert: the test is not reading imports")
	}
}
