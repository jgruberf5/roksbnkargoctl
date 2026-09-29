package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jgruberf5/roksbnkargoctl/cmd/check/internal/checks"
	"github.com/jgruberf5/roksbnkargoctl/internal/config"
	"github.com/jgruberf5/roksbnkargoctl/internal/far"
	"github.com/jgruberf5/roksbnkargoctl/internal/render"
)

// The renderer writes the check container's arguments; this binary parses them.
// Each side had its own tests and both passed, yet the first live sync failed
// with "flag provided but not defined: -timeout" (post-install) — the seam
// between them was untested. This renders every install shape and parses every
// check container's args with the real flag sets.
//
// (This test imports internal/render; test-only imports do not reach the
// shipped binary, which CI keeps standard-library only.)
func TestRenderedArgsParse(t *testing.T) {
	shapes := map[string]func(*config.Config, *render.Inputs){
		"connected/far": func(*config.Config, *render.Inputs) {},
		"disconnected/far": func(c *config.Config, in *render.Inputs) {
			c.BNK.Mode = config.ModeDisconnected
			in.FLPURL = "https://10.248.0.4:8443"
			in.Secrets.FLPCAPEM = "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"
		},
		"connected/mirror-with-ca": func(c *config.Config, in *render.Inputs) {
			c.Registry.Source = config.SourceMirror
			c.Registry.Mirror = config.Mirror{Host: "harbor.x:8443", Prefix: "m", Username: "robot", CAFile: "ca.pem"}
			in.MirrorCAPEM = "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"
			in.NodeResolverImage = "quay.io/openshift/node-resolver@sha256:0"
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			c := &config.Config{IBMCloud: config.IBMCloud{Region: "us-east"}, Cluster: "c", TransitGateway: "t",
				COS: config.COS{Bucket: "b"}, ArgoCD: config.ArgoCD{Server: "https://a"}, Git: config.Git{URL: "https://g/r.git"}}
			in := render.Inputs{Config: c, Workspace: "ws", CheckImage: "ghcr.io/x/check:dev", RunID: "r",
				Manifest: &far.Manifest{Version: config.BNKVersion,
					Charts: []far.Artifact{{Name: "charts/f5-lifecycle-operator", Version: "1"}},
					Images: []far.Artifact{{Name: "images/f5-lifecycle-operator", Version: "1"}}},
				FLOChart: tinyChart(t, "flo"), CertManagerChart: tinyChart(t, "cm"),
				Secrets: render.Secrets{PullHost: "h", PullUsername: "u", PullPassword: "pppppppp", JWT: "a.b.c"}}
			shape(c, &in)
			c.Defaults("ws")
			c.Resolved = &config.Resolved{VPCName: "v", TrustedProfileID: "p", ArgoCDClusterServer: "https://k"}
			out, err := render.Render(in)
			if err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, o := range out.Git {
				for _, args := range checkArgs(o, in.CheckImage) {
					seen++
					mode := args[0]
					m, ok := modes[mode]
					if !ok {
						t.Errorf("%s %s runs unknown mode %q", o.Kind(), o.Name(), mode)
						continue
					}
					fs := flag.NewFlagSet(mode, flag.ContinueOnError)
					fs.SetOutput(io.Discard)
					m(fs)
					if err := fs.Parse(args[1:]); err != nil {
						t.Errorf("%s %s: `check %s` rejects its rendered args: %v\n  args: %v", o.Kind(), o.Name(), mode, err, args[1:])
					}
				}
			}
			// pre-install, node-probe, sweep, license, post-install, pre- and post-uninstall.
			if seen < 7 {
				t.Fatalf("found %d check containers; expected every mode to be rendered", seen)
			}
		})
	}
}

// checkArgs returns the args of every container running the check image.
func checkArgs(o render.Object, image string) [][]string {
	spec, _ := o["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	cs, _ := pod["containers"].([]any)
	var out [][]string
	for _, c := range cs {
		m, _ := c.(map[string]any)
		if m["image"] != image {
			continue
		}
		var args []string
		for _, a := range m["args"].([]any) {
			args = append(args, a.(string))
		}
		out = append(out, args)
	}
	return out
}

func tinyChart(t *testing.T, name string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml":        "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n",
		name + "/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\ndata: {}\n",
		name + "/values.yaml":       "{}\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(b))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// #11: the renderer named the CNEInstance with a constant, the check derives the
// name from the namespace it is told; they agreed only for f5-bnk. Render a
// non-default namespace and hold every check that looks the CR up by name to
// the name actually rendered.
func TestChecksLookUpTheRenderedCNEInstance(t *testing.T) {
	c := &config.Config{IBMCloud: config.IBMCloud{Region: "us-east"}, Cluster: "c", TransitGateway: "t",
		COS: config.COS{Bucket: "b"}, ArgoCD: config.ArgoCD{Server: "https://a"}, Git: config.Git{URL: "https://g/r.git"},
		BNK: config.BNK{Namespace: "bnk", UtilsNamespace: "bnk-utils"}}
	c.Defaults("ws")
	c.Resolved = &config.Resolved{VPCName: "v", TrustedProfileID: "p", ArgoCDClusterServer: "https://k"}
	in := render.Inputs{Config: c, Workspace: "ws", CheckImage: "ghcr.io/x/check:dev", RunID: "r",
		Manifest: &far.Manifest{Version: config.BNKVersion,
			Charts: []far.Artifact{{Name: "charts/f5-lifecycle-operator", Version: "1"}},
			Images: []far.Artifact{{Name: "images/f5-lifecycle-operator", Version: "1"}}},
		FLOChart: tinyChart(t, "flo"), CertManagerChart: tinyChart(t, "cm"),
		Secrets: render.Secrets{PullHost: "h", PullUsername: "u", PullPassword: "pppppppp", JWT: "a.b.c"}}
	out, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var cneName, cneNS string
	for _, o := range out.Git {
		if o.Kind() == "CNEInstance" {
			cneName = o.Name()
			cneNS, _ = o["metadata"].(map[string]any)["namespace"].(string)
		}
	}
	if cneNS != "bnk" {
		t.Fatalf("CNEInstance rendered in %q, want bnk", cneNS)
	}
	// The flag each mode takes the CNEInstance's namespace from.
	nsFlag := map[string]string{"license": "--cne-namespace=", "post-install": "--bnk-namespace="}
	checked := 0
	for _, o := range out.Git {
		for _, args := range checkArgs(o, in.CheckImage) {
			prefix, ok := nsFlag[args[0]]
			if !ok {
				continue
			}
			for _, a := range args[1:] {
				if strings.HasPrefix(a, prefix) {
					checked++
					if got := checks.CNEInstanceName(strings.TrimPrefix(a, prefix)); got != cneName {
						t.Errorf("check %s looks for CNEInstance %q; the render names it %q", args[0], got, cneName)
					}
				}
			}
		}
	}
	if checked != len(nsFlag) {
		t.Fatalf("checked %d modes, want %d", checked, len(nsFlag))
	}
}

// #12: the license hook's deadline was 40m while its own waits could reach 65m,
// so a slow install died of DeadlineExceeded with no finding. Parse the
// rendered args with the real flag set and hold the Job's deadline above the
// sum of the waits the check will actually use.
func TestLicenseDeadlineCoversItsWaits(t *testing.T) {
	c := &config.Config{IBMCloud: config.IBMCloud{Region: "us-east"}, Cluster: "c", TransitGateway: "t",
		COS: config.COS{Bucket: "b"}, ArgoCD: config.ArgoCD{Server: "https://a"}, Git: config.Git{URL: "https://g/r.git"}}
	c.Defaults("ws")
	c.Resolved = &config.Resolved{VPCName: "v", TrustedProfileID: "p", ArgoCDClusterServer: "https://k"}
	in := render.Inputs{Config: c, Workspace: "ws", CheckImage: "ghcr.io/x/check:dev", RunID: "r",
		Manifest: &far.Manifest{Version: config.BNKVersion,
			Charts: []far.Artifact{{Name: "charts/f5-lifecycle-operator", Version: "1"}},
			Images: []far.Artifact{{Name: "images/f5-lifecycle-operator", Version: "1"}}},
		FLOChart: tinyChart(t, "flo"), CertManagerChart: tinyChart(t, "cm"),
		Secrets: render.Secrets{PullHost: "h", PullUsername: "u", PullPassword: "pppppppp", JWT: "a.b.c"}}
	out, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out.Git {
		for _, args := range checkArgs(o, in.CheckImage) {
			if args[0] != "license" {
				continue
			}
			fs := flag.NewFlagSet("license", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			modes["license"](fs)
			if err := fs.Parse(args[1:]); err != nil {
				t.Fatal(err)
			}
			var waits time.Duration
			for _, f := range []string{"crd-timeout", "apply-retry", "timeout", "cne-timeout"} {
				waits += fs.Lookup(f).Value.(flag.Getter).Get().(time.Duration)
			}
			deadline := time.Duration(o["spec"].(map[string]any)["activeDeadlineSeconds"].(int)) * time.Second
			if deadline <= waits {
				t.Fatalf("check-license deadline %s does not cover its waits (%s)", deadline, waits)
			}
			return
		}
	}
	t.Fatal("no license check rendered")
}
