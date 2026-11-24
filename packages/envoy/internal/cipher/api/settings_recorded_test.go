package api_test

// This file holds GET /v1/settings to what main answered for one broker on each store, before the
// store packages moved: an AWS or a GCP deployment upgrades with no change to the fields its CLIs
// read. testdata/settings-recorded-on-main-<store>.json was written once, on main, by this file as
// it was then:
//
//	go test ./internal/cipher/api/ -run TestSettingsRecordedOnMain -args -record
//
// The flag that wrote them went with the store packages' move, so no later code can write them:
// a record written by the code it checks compares that code with itself and proves nothing. A
// change that means to move a field of either body is a change to the settings every deployed CLI
// reads, which the records exist to catch.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
)

// TestSettingsRecordedOnMain: GET /v1/settings for a broker on AWS Secrets Manager and for one on
// Google Secret Manager, each configured by its registry entry from the variables a deployment
// sets, answers, field for field, the body main answered for the same configuration.
func TestSettingsRecordedOnMain(t *testing.T) {
	for _, row := range []struct {
		store, name, prefix string
		env                 map[string]string
	}{
		{"aws", aws.Name, "example/agent-secrets/",
			map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"}},
		{"gcp", gcp.Name, "projects/000000000000/secrets/agent-secrets-",
			map[string]string{"BROKER_SECRETS_KMS_KEY_NAME": "projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"}},
	} {
		t.Run(row.store, func(t *testing.T) {
			record := filepath.Join("testdata", "settings-recorded-on-main-"+row.store+".json")
			b, err := backends.Lookup(row.name)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := b.ParseConfig(row.prefix, func(name string) string { return row.env[name] })
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			api.Register(mux, api.Deps{SecretsStore: b.Name(), SecretsPrefix: cfg.Prefix(), SettingsFields: cfg.Settings()})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/v1/settings")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /v1/settings = %d %s, want 200", resp.StatusCode, body)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode GET /v1/settings %s: %v", body, err)
			}
			raw, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("read %s: %v", record, err)
			}
			var want map[string]any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode %s: %v", record, err)
			}
			if len(want) == 0 {
				t.Fatalf("UNKNOWN: %s records no field, so the check compares nothing", record)
			}
			var keys []string
			for key := range got {
				keys = append(keys, key)
			}
			for key := range want {
				if _, both := got[key]; !both {
					keys = append(keys, key)
				}
			}
			slices.Sort(keys)
			for _, key := range keys {
				gotValue, answered := got[key]
				wantValue, recorded := want[key]
				if answered != recorded || !reflect.DeepEqual(gotValue, wantValue) {
					t.Errorf("settings field %q = %s, recorded on main %s", key, fieldText(gotValue, answered), fieldText(wantValue, recorded))
				}
			}
		})
	}
}

// fieldText is a settings field's value as a failure prints it, and "absent" for a field the body
// does not hold, which a JSON null would otherwise print the same as.
func fieldText(value any, present bool) string {
	if !present {
		return "absent"
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}
