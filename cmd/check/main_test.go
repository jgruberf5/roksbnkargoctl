package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"strings"
	"testing"
)

func TestEnvDefaultsAndRepeatableFlags(t *testing.T) {
	t.Setenv("CHECK_BNK_NAMESPACE", "bnk-one")
	t.Setenv("CHECK_TARGET", "cr.f5.com:443,tls; 10.243.0.5:8443,insecure")
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	bnk, _ := namespaces(fs)
	targets := &listFlag{split: func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ' ' })
	}}
	fs.Var(targets, "target", "")
	secrets := &listFlag{}
	fs.Var(secrets, "require-secret", "")
	applyEnvDefaults(fs)
	if err := fs.Parse([]string{"--require-secret=a/b", "--require-secret=c/d,e/f"}); err != nil {
		t.Fatal(err)
	}
	if *bnk != "bnk-one" {
		t.Errorf("env default not applied: %s", *bnk)
	}
	if strings.Join(targets.vals, "|") != "cr.f5.com:443,tls|10.243.0.5:8443,insecure" {
		t.Errorf("targets from env: %v", targets.vals)
	}
	if strings.Join(secrets.vals, "|") != "a/b|c/d|e/f" {
		t.Errorf("repeatable: %v", secrets.vals)
	}
	// A command-line value replaces the env list rather than appending to it.
	fs2 := flag.NewFlagSet("y", flag.ContinueOnError)
	tl := &listFlag{}
	fs2.Var(tl, "target", "")
	applyEnvDefaults(fs2)
	_ = fs2.Parse([]string{"--target=x:1"})
	if strings.Join(tl.vals, "|") != "x:1" {
		t.Errorf("cli did not replace env: %v", tl.vals)
	}
}

func TestOutsideClusterFailsWithJSON(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	var out, errb bytes.Buffer
	if code := run([]string{"post-install"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res["ok"] != false || res["mode"] != "post-install" {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("unknown mode exit %d", code)
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: %d %q", code, out.String())
	}
}
