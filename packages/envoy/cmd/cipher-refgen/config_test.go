package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoRoot is the repository root these tests read the broker's source from, as the docs build
// reads it.
func repoRoot() string { return filepath.Join("..", "..", "..", "..") }

// TestConfigReadsEachStoresVariables: a variable only one store's package parses is read and
// documented from that package, as one in config.Load or cmd/cipherd is, and BROKER_SECRETS_STORE
// carries the default its registry gives it.
func TestConfigReadsEachStoresVariables(t *testing.T) {
	vars, err := readConfig(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]variable{}
	for _, v := range vars {
		read[v.Name] = v
	}
	for name, where := range map[string]string{
		"BROKER_SECRETS_KMS_KEY_ARN":  envoyDir + "/internal/cipher/secretstore/aws/",
		"BROKER_SECRETS_KMS_KEY_NAME": envoyDir + "/internal/cipher/secretstore/gcp/",
		"BROKER_MEMORY_SECRETS_FILE":  envoyDir + "/internal/cipher/secretstore/memory/",
	} {
		v, ok := read[name]
		if !ok {
			t.Errorf("%s is read by no package the config page reads", name)
			continue
		}
		if !strings.HasPrefix(v.At, where) {
			t.Errorf("%s is read at %s, want its own store's package under %s", name, v.At, where)
		}
		if strings.TrimSpace(v.Doc) == "" {
			t.Errorf("%s has no documentation", name)
		}
	}
	store, ok := read["BROKER_SECRETS_STORE"]
	if !ok {
		t.Fatal("BROKER_SECRETS_STORE is not read")
	}
	if store.Default != "aws-secrets-manager" {
		t.Errorf("BROKER_SECRETS_STORE's default is %q, want aws-secrets-manager, the registry's", store.Default)
	}
	if gone := slices.IndexFunc(vars, func(v variable) bool { return v.Name == "BROKER_FAKE_SECRETS_FILE" }); gone < 0 || !vars[gone].Removed {
		t.Error("BROKER_FAKE_SECRETS_FILE is not among the removed variables")
	}
}

// TestSettingsFieldsAreDocumented: GET /v1/settings's body lists each store's own fields, each
// with the text its SettingsFields struct documents it by, beside the fields every store answers.
func TestSettingsFieldsAreDocumented(t *testing.T) {
	g, err := newGraph(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := g.localStruct("settingsResponse")
	if err != nil {
		t.Fatal(err)
	}
	read, err := g.fields(ref)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, f := range read {
		fields[f.Name] = f.Doc
	}
	if len(fields) == 0 {
		t.Fatal("UNKNOWN: settingsResponse has no field, so this checks nothing")
	}
	for _, name := range []string{
		"store", "secrets_prefix", "secret_type_tag",
		"kms_key_arn", "aws_account_id", "aws_region",
		"kms_key_name", "gcp_project",
		"memory_secrets_file",
	} {
		doc, ok := fields[name]
		if !ok {
			t.Errorf("GET /v1/settings documents no field %s", name)
			continue
		}
		if strings.TrimSpace(doc) == "" {
			t.Errorf("GET /v1/settings's field %s has no documentation", name)
		}
	}
}
