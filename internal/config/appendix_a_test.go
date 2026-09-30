package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// appendixA is the book's config.yaml reference. It promises an override
// variable for every key, so it must list exactly the ones EnvOverrides has.
var appendixA = filepath.Join("..", "..", "book", "src", "appendix-a-config.md")

var (
	appendixKeyCell = regexp.MustCompile("^`([a-z0-9_.]+)`$")
	appendixEnvVar  = regexp.MustCompile(EnvPrefix + `[A-Z0-9_]+`)
)

// appendixProblems reads the key tables of appendix A and returns everything
// wrong with the override variables they list: a key whose row is missing or
// names another variable, a row naming a variable no key has (such as
// ROKSBNKARGOCTL_BNK_VERSION, which belongs to the installers), and a key
// listed twice.
//
// A row is read under the nearest "## <section>" heading: "## Top level" is the
// root, "## flp" makes `vsi.zone` flp.vsi.zone, and any other heading ends the
// key tables until the next section heading. Only rows that name a
// ROKSBNKARGOCTL_* variable count, so the map-valued rows of "Top level" and
// the other tables are ignored.
func appendixProblems(doc string) []string {
	sections := map[string]bool{}
	for _, k := range EnvOverrides() {
		if i := strings.Index(k.Path, "."); i > 0 {
			sections[k.Path[:i]] = true
		}
	}
	rows := map[string]string{}
	var problems []string
	section, inKeys := "", false
	overrideCol := -1 // the Override column of the table being read
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimRight(line, "\r")
		if h, ok := strings.CutPrefix(line, "## "); ok {
			h = strings.TrimSpace(h)
			switch {
			case h == "Top level":
				section, inKeys = "", true
			case sections[h]:
				section, inKeys = h, true
			default:
				inKeys = false
			}
			continue
		}
		if !inKeys || !strings.HasPrefix(line, "|") {
			overrideCol = -1
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cells[0] == "Key" {
			overrideCol = -1
			for i, c := range cells {
				if c == "Override" {
					overrideCol = i
				}
			}
			continue
		}
		m := appendixKeyCell.FindStringSubmatch(cells[0])
		if m == nil || overrideCol < 0 || overrideCol >= len(cells) {
			continue
		}
		path := m[1]
		if section != "" {
			path = section + "." + path
		}
		vars := appendixEnvVar.FindAllString(cells[overrideCol], -1)
		switch {
		case len(vars) == 0:
			continue
		case len(vars) > 1:
			problems = append(problems, path+": the Override cell names more than one variable: "+strings.Join(vars, ", "))
			continue
		}
		if prev, dup := rows[path]; dup {
			problems = append(problems, path+": listed twice ("+prev+", "+vars[0]+")")
		}
		rows[path] = vars[0]
	}
	want := map[string]bool{}
	for _, k := range EnvOverrides() {
		want[k.Path] = true
		got, ok := rows[k.Path]
		switch {
		case !ok:
			problems = append(problems, "no row for "+k.Path+" naming its variable "+k.Env)
		case got != k.Env:
			problems = append(problems, "the row for "+k.Path+" names "+got+"; the variable is "+k.Env)
		}
	}
	var extra []string
	for path, env := range rows {
		if !want[path] {
			extra = append(extra, "the row for "+path+" names "+env+", but "+path+" has no override variable")
		}
	}
	sort.Strings(extra)
	return append(problems, extra...)
}

// The book lists every ROKSBNKARGOCTL_* override variable, each on its key's row.
func TestAppendixAListsEveryOverrideVariable(t *testing.T) {
	b, err := os.ReadFile(appendixA)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range appendixProblems(string(b)) {
		t.Errorf("%s: %s", appendixA, p)
	}
}

// The check must fail when a row is deleted, names the wrong variable, or
// invents one; otherwise the test above could pass against a book that lists
// nothing.
func TestAppendixACheckFailsOnABrokenBook(t *testing.T) {
	b, err := os.ReadFile(appendixA)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	row := func(t *testing.T, env string) string {
		t.Helper()
		for _, line := range strings.SplitAfter(doc, "\n") {
			if strings.HasPrefix(line, "|") && strings.Contains(line, "`"+env+"`") {
				return line
			}
		}
		t.Fatalf("no row names %s", env)
		return ""
	}
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string) string
	}{
		{"a deleted row", func(t *testing.T, d string) string {
			return strings.Replace(d, row(t, "ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST"), "", 1)
		}},
		{"a deleted forge row", func(t *testing.T, d string) string {
			return strings.Replace(d, row(t, "ROKSBNKARGOCTL_FORGE_CA_FILE"), "", 1)
		}},
		{"a misspelt variable", func(t *testing.T, d string) string {
			r := row(t, "ROKSBNKARGOCTL_FLP_VSI_ALLOWED_CIDRS")
			return strings.Replace(d, r, strings.Replace(r, "_FLP_VSI_", "_FLP_", 1), 1)
		}},
		{"a variable for bnk.version", func(t *testing.T, d string) string {
			r := row(t, "ROKSBNKARGOCTL_BNK_MODE")
			return strings.Replace(d, r, r+"| `version` | string | `2.4.0` | `ROKSBNKARGOCTL_BNK_VERSION` | x |\n", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := tc.mutate(t, doc)
			if changed == doc {
				t.Fatal("the mutation changed nothing")
			}
			if len(appendixProblems(changed)) == 0 {
				t.Errorf("the appendix check passed a book with %s", tc.name)
			}
		})
	}
}
