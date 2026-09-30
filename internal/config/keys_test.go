package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// The names are the ones the documentation promises, derived from the yaml tags.
func TestKeyNamesFollowTheYAMLPaths(t *testing.T) {
	for path, want := range map[string]struct {
		env  string
		kind Kind
	}{
		"registry.mirror.host":         {"ROKSBNKARGOCTL_REGISTRY_MIRROR_HOST", KindString},
		"flp.vsi.allowed_cidrs":        {"ROKSBNKARGOCTL_FLP_VSI_ALLOWED_CIDRS", KindStringList},
		"cluster":                      {"ROKSBNKARGOCTL_CLUSTER", KindString},
		"bnk.tmm_replicas":             {"ROKSBNKARGOCTL_BNK_TMM_REPLICAS", KindInt},
		"argocd.insecure":              {"ROKSBNKARGOCTL_ARGOCD_INSECURE", KindBool},
		"flp.vsi.floating_ip":          {"ROKSBNKARGOCTL_FLP_VSI_FLOATING_IP", KindBool},
		"bnk.cert_manager.install":     {"ROKSBNKARGOCTL_BNK_CERT_MANAGER_INSTALL", KindBool},
		"test_hub.name":                {"ROKSBNKARGOCTL_TEST_HUB_NAME", KindString},
		"registry.mirror.password_env": {"ROKSBNKARGOCTL_REGISTRY_MIRROR_PASSWORD_ENV", KindString},
		"ibmcloud.api_key_env":         {"ROKSBNKARGOCTL_IBMCLOUD_API_KEY_ENV", KindString},
		"flp.external.root_ca_file":    {"ROKSBNKARGOCTL_FLP_EXTERNAL_ROOT_CA_FILE", KindString},
		"git.known_hosts_file":         {"ROKSBNKARGOCTL_GIT_KNOWN_HOSTS_FILE", KindString},
		"test_hub.attach_to_tgw":       {"ROKSBNKARGOCTL_TEST_HUB_ATTACH_TO_TGW", KindBool},
		"cos.local_far_auth_file":      {"ROKSBNKARGOCTL_COS_LOCAL_FAR_AUTH_FILE", KindString},
		"argocd.cluster_endpoint":      {"ROKSBNKARGOCTL_ARGOCD_CLUSTER_ENDPOINT", KindString},
		"bnk.cert_manager.version":     {"ROKSBNKARGOCTL_BNK_CERT_MANAGER_VERSION", KindString},
		"test_hub.node_port":           {"ROKSBNKARGOCTL_TEST_HUB_NODE_PORT", KindInt},
		"transit_gateway":              {"ROKSBNKARGOCTL_TRANSIT_GATEWAY", KindString},
		"ibmcloud.resource_group":      {"ROKSBNKARGOCTL_IBMCLOUD_RESOURCE_GROUP", KindString},
		"registry.far_host":            {"ROKSBNKARGOCTL_REGISTRY_FAR_HOST", KindString},
		"check.image":                  {"ROKSBNKARGOCTL_CHECK_IMAGE", KindString},
		"git.author_email":             {"ROKSBNKARGOCTL_GIT_AUTHOR_EMAIL", KindString},
		"bnk.storage_class":            {"ROKSBNKARGOCTL_BNK_STORAGE_CLASS", KindString},
		"flp.vsi.ssh_key":              {"ROKSBNKARGOCTL_FLP_VSI_SSH_KEY", KindString},
		"registry.mirror.ca_file":      {"ROKSBNKARGOCTL_REGISTRY_MIRROR_CA_FILE", KindString},
		"argocd.ca_file":               {"ROKSBNKARGOCTL_ARGOCD_CA_FILE", KindString},
		"cos.jwt_object":               {"ROKSBNKARGOCTL_COS_JWT_OBJECT", KindString},
		"test_hub.allowed_cidr":        {"ROKSBNKARGOCTL_TEST_HUB_ALLOWED_CIDR", KindString},
		"flp.vsi.vpc":                  {"ROKSBNKARGOCTL_FLP_VSI_VPC", KindString},
		"git.token_env":                {"ROKSBNKARGOCTL_GIT_TOKEN_ENV", KindString},
		"argocd.token_env":             {"ROKSBNKARGOCTL_ARGOCD_TOKEN_ENV", KindString},
		"bnk.nad_address":              {"ROKSBNKARGOCTL_BNK_NAD_ADDRESS", KindString},
		"cos.far_auth_object":          {"ROKSBNKARGOCTL_COS_FAR_AUTH_OBJECT", KindString},
		"test_hub.k3s_channel":         {"ROKSBNKARGOCTL_TEST_HUB_K3S_CHANNEL", KindString},
		"registry.source":              {"ROKSBNKARGOCTL_REGISTRY_SOURCE", KindString},
		"bnk.utils_namespace":          {"ROKSBNKARGOCTL_BNK_UTILS_NAMESPACE", KindString},
		"argocd.application":           {"ROKSBNKARGOCTL_ARGOCD_APPLICATION", KindString},
		"git.ssh_key_file":             {"ROKSBNKARGOCTL_GIT_SSH_KEY_FILE", KindString},
		"registry.mirror.prefix":       {"ROKSBNKARGOCTL_REGISTRY_MIRROR_PREFIX", KindString},
		"flp.external.url":             {"ROKSBNKARGOCTL_FLP_EXTERNAL_URL", KindString},
		"test_hub.region":              {"ROKSBNKARGOCTL_TEST_HUB_REGION", KindString},
		"cos.local_jwt_file":           {"ROKSBNKARGOCTL_COS_LOCAL_JWT_FILE", KindString},
		"registry.mirror.username":     {"ROKSBNKARGOCTL_REGISTRY_MIRROR_USERNAME", KindString},
		"bnk.mode":                     {"ROKSBNKARGOCTL_BNK_MODE", KindString},
		"ibmcloud.region":              {"ROKSBNKARGOCTL_IBMCLOUD_REGION", KindString},
		"git.url":                      {"ROKSBNKARGOCTL_GIT_URL", KindString},
		"argocd.server":                {"ROKSBNKARGOCTL_ARGOCD_SERVER", KindString},
		"cos.bucket":                   {"ROKSBNKARGOCTL_COS_BUCKET", KindString},
		"bnk.namespace":                {"ROKSBNKARGOCTL_BNK_NAMESPACE", KindString},
		"flp.vsi.cidr":                 {"ROKSBNKARGOCTL_FLP_VSI_CIDR", KindString},
		"test_hub.version":             {"ROKSBNKARGOCTL_TEST_HUB_VERSION", KindString},
	} {
		k, ok := LookupKey(path)
		if !ok {
			t.Errorf("no key %s", path)
			continue
		}
		if k.Env != want.env || k.Kind != want.kind {
			t.Errorf("%s: env %s kind %s, want %s %s", path, k.Env, k.Kind, want.env, want.kind)
		}
	}
	for _, k := range Keys() {
		if strings.HasPrefix(k.Path, "resolved") {
			t.Errorf("resolved state must not be overridable: %s", k.Path)
		}
	}
	if _, ok := LookupKey("bnk.version"); ok {
		t.Error("bnk.version must have no override (one valid value; the name is install.sh's)")
	}
	if n := len(Keys()); n != 64 {
		t.Errorf("%d keys; update this count (and the documentation) when adding one", n)
	}
}

// Every key carries help text: flags and the generated docs show it.
func TestEveryKeyHasHelp(t *testing.T) {
	for _, k := range Keys() {
		if strings.TrimSpace(k.Help) == "" {
			t.Errorf("%s has no help tag", k.Path)
		}
	}
}

// Every variable reaches exactly its own field: set each one, write config.yaml,
// read it back, and find the value under the key's path and nowhere else.
func TestEveryVariableSetsItsOwnKey(t *testing.T) {
	for _, k := range Keys() {
		raw, want := map[Kind]string{KindString: "v-" + k.Path, KindInt: "4242", KindBool: "true", KindStringList: "a, b"}[k.Kind], any(nil)
		ovs, err := FromEnv(envMap(map[string]string{k.Env: raw}))
		if err != nil || len(ovs) != 1 {
			t.Fatalf("%s: %v %v", k.Env, ovs, err)
		}
		c := &Config{}
		if err := Apply(c, ovs); err != nil {
			t.Fatal(err)
		}
		y, err := Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Parse(y)
		if err != nil {
			t.Fatalf("%s: %v\n%s", k.Env, err, y)
		}
		switch k.Kind {
		case KindString:
			want = "v-" + k.Path
		case KindInt:
			want = 4242
		case KindBool:
			want = true
		case KindStringList:
			want = []string{"a", "b"}
		}
		if got, _ := back.Get(k.Path); !SameValue(got, want) {
			t.Errorf("%s: %s reads back %v, want %v", k.Env, k.Path, got, want)
		}
		if d := DiffKeys(back, &Config{}); len(d) != 1 || d[0] != k.Path {
			t.Errorf("%s changed %v, want only %s", k.Env, d, k.Path)
		}
	}
}

func TestFromEnvParsesEachType(t *testing.T) {
	ovs, err := FromEnv(envMap(map[string]string{
		"ROKSBNKARGOCTL_CLUSTER":               "  c1 ",
		"ROKSBNKARGOCTL_BNK_TMM_REPLICAS":      " 5",
		"ROKSBNKARGOCTL_ARGOCD_INSECURE":       "TRUE",
		"ROKSBNKARGOCTL_FLP_VSI_FLOATING_IP":   "false",
		"ROKSBNKARGOCTL_FLP_VSI_ALLOWED_CIDRS": " 10.0.0.0/8, ,192.168.0.0/16,, ",
		"ROKSBNKARGOCTL_GIT_BRANCH":            "", // empty: unset
		"ROKSBNKARGOCTL_GIT_PATH":              "   ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{Git: Git{Branch: "keep", Path: "keep"}}
	if err := Apply(c, ovs); err != nil {
		t.Fatal(err)
	}
	if c.Cluster != "c1" || c.BNK.TMMReplicas != 5 || !c.ArgoCD.Insecure {
		t.Errorf("string/int/bool: %+v", c)
	}
	if c.FLP.VSI.FloatingIP == nil || *c.FLP.VSI.FloatingIP {
		t.Errorf("*bool false must be set to a pointer to false, got %v", c.FLP.VSI.FloatingIP)
	}
	if got := strings.Join(c.FLP.VSI.AllowedCIDRs, "|"); got != "10.0.0.0/8|192.168.0.0/16" {
		t.Errorf("list: %q", got)
	}
	if c.Git.Branch != "keep" || c.Git.Path != "keep" {
		t.Errorf("an empty variable must be treated as unset: %+v", c.Git)
	}
}

// A malformed value is an error naming the variable — every bad one, not the
// first — and is never silently ignored.
func TestFromEnvRejectsMalformedValues(t *testing.T) {
	_, err := FromEnv(envMap(map[string]string{
		"ROKSBNKARGOCTL_BNK_TMM_REPLICAS":       "three",
		"ROKSBNKARGOCTL_ARGOCD_INSECURE":        "yes please",
		"ROKSBNKARGOCTL_TEST_HUB_ATTACH_TO_TGW": "maybe",
		"ROKSBNKARGOCTL_CLUSTER":                "fine",
	}))
	if err == nil {
		t.Fatal("malformed values accepted")
	}
	for _, want := range []string{"ROKSBNKARGOCTL_BNK_TMM_REPLICAS", `"three"`, "ROKSBNKARGOCTL_ARGOCD_INSECURE", "ROKSBNKARGOCTL_TEST_HUB_ATTACH_TO_TGW"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// Overrides are applied to a copy; the original's slices and pointers must not
// change underneath it (the saved config would pick the override up).
func TestCloneIsDeep(t *testing.T) {
	f := false
	orig := &Config{FLP: FLP{VSI: FLPVSI{AllowedCIDRs: []string{"a"}, FloatingIP: &f}}, Resolved: &Resolved{Zones: []string{"z1"}}}
	cp := orig.Clone()
	cp.FLP.VSI.AllowedCIDRs[0] = "changed"
	*cp.FLP.VSI.FloatingIP = true
	cp.Resolved.Zones[0] = "changed"
	cp.Resolved.ClusterID = "changed"
	if orig.FLP.VSI.AllowedCIDRs[0] != "a" || *orig.FLP.VSI.FloatingIP || orig.Resolved.Zones[0] != "z1" || orig.Resolved.ClusterID != "" {
		t.Errorf("clone shares state with the original: %+v %+v", orig.FLP.VSI, orig.Resolved)
	}
}

// ROKSBNKARGOCTL_* names the tool (or its installers) already read must not
// also be generated for a config key: the variable would silently do two
// things. The scan covers code and scripts, not docs (which will list the
// generated names) and not tests.
func TestOverrideNamesDoNotCollideWithExistingVariables(t *testing.T) {
	generated := map[string]string{}
	for _, k := range Keys() {
		generated[k.Env] = k.Path
	}
	re := regexp.MustCompile(`ROKSBNKARGOCTL_[A-Z0-9_]+`)
	root := filepath.Join("..", "..")
	var scanned int
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", "book", "docs", "bin", "dist", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		code := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		script := strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".ps1") || name == "Makefile" ||
			strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") || name == "pre-push"
		if !code && !script {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		scanned++
		for _, v := range re.FindAllString(string(b), -1) {
			if path, clash := generated[v]; clash {
				t.Errorf("%s reads %s, which is also the override of config key %s", p, v, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files; the walk is not seeing the repository", scanned)
	}
}
