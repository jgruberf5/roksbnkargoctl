package forge

import (
	"context"
	"net/http/httptest"
	"testing"
)

// ── #223: the provider value Forge actually matches ──────────────────────────

// Forge compares `provider == "ibm"` in at least seven places and lowercases
// none of them. "IBM" is accepted by the API and then matches nothing: no
// credential injection, no blueprint input resolution, no IBM lookup, and the
// "IBM templates must carry an API key" validation never fires. Nothing errors —
// the template just does nothing.
func TestTheCredentialTemplateIsCreatedWithTheProviderForgeMatches(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	if _, err := c.EnsureIBMCredentialTemplate(context.Background(), IBMCredentialTemplate{
		Name: "roksbnkargoctl-ws", APIKey: "K", ResourceGroup: "default",
		Region: "us-east", COSInstance: "bnk-orchestration",
	}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(m.credTemplates) != 1 {
		t.Fatalf("expected one created template, got %v", m.credTemplates)
	}
	got := m.credTemplates[0]
	if got["provider"] != "ibm" {
		t.Errorf(`provider = %q, want "ibm" exactly.`+"\n"+
			`Forge matches this case-sensitively; "IBM" is stored happily and then matches `+
			`nothing, so the template is inert in both directions (#223).`, got["provider"])
	}
	if got["region"] != "us-east" {
		t.Errorf(`region = %v, want "us-east" — blueprint inputs with `+
			"`source: credential_template, source_field: region` have nothing to inherit otherwise", got["region"])
	}
	if got["ibm_cos_instance_name"] != "bnk-orchestration" {
		t.Errorf("ibm_cos_instance_name = %v, want bnk-orchestration", got["ibm_cos_instance_name"])
	}
}

// The reported symptom is a template that ALREADY exists with provider "IBM".
// Fixing only the create path would repair new installs and abandon every
// existing one — including the one in the issue.
func TestAnExistingTemplateIsRepairedRatherThanLeftInert(t *testing.T) {
	m := &mockForge{
		token:         "t",
		nextID:        40,
		credTemplates: []map[string]any{{"id": 3, "name": "roksbnkargoctl-bnk", "provider": "IBM"}},
	}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	id, err := c.EnsureIBMCredentialTemplate(context.Background(), IBMCredentialTemplate{
		Name: "roksbnkargoctl-bnk", APIKey: "K", ResourceGroup: "default",
		Region: "us-east", COSInstance: "bnk-orchestration",
	})
	if err != nil || id != 3 {
		t.Fatalf("ensure existing: id=%d err=%v (want 3)", id, err)
	}
	if m.credUpdate == nil {
		t.Fatal("no PUT was sent to the existing template")
	}
	if m.credUpdate["provider"] != "ibm" {
		t.Errorf(`the update sent provider = %v, want "ibm".`+"\n"+
			`Without it a template a previous roksbnkctl wrote as "IBM" stays broken forever, `+
			`and that template is the reported defect (#223).`, m.credUpdate["provider"])
	}
	if m.credUpdate["region"] != "us-east" || m.credUpdate["ibm_cos_instance_name"] != "bnk-orchestration" {
		t.Errorf("the update did not backfill region/cos: %v", m.credUpdate)
	}
}

// An unset region must not be sent. Writing "" would overwrite a value an
// operator set by hand with an empty one, which is worse than leaving it.
func TestUnsetFieldsAreNotSentAsEmptyStrings(t *testing.T) {
	m := &mockForge{token: "t", nextID: 40}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	c := mustNew(t, srv.URL, Options{})
	c.Token = "t"

	if _, err := c.EnsureIBMCredentialTemplate(context.Background(), IBMCredentialTemplate{
		Name: "roksbnkargoctl-ws", APIKey: "K", ResourceGroup: "default",
	}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := m.credTemplates[0]
	if _, present := got["region"]; present {
		t.Errorf("region was sent as %v despite being unset — that overwrites a hand-set value", got["region"])
	}
	if _, present := got["ibm_cos_instance_name"]; present {
		t.Errorf("ibm_cos_instance_name was sent despite being unset: %v", got["ibm_cos_instance_name"])
	}
	if got["provider"] != "ibm" {
		t.Errorf("provider = %v, want ibm even with the optional fields absent", got["provider"])
	}
}
