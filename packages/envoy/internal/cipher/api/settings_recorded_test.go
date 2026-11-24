package api_test

// This file holds GET /v1/settings to what main answered for one broker on each store, before the
// store packages moved: an AWS or a GCP deployment upgrades with no change to the fields its CLIs
// read. testdata/settings-recorded-on-main-<store>.json was written once, on main, by this file as
// it was then:
//
//	go test ./internal/cipher/api/ -run TestSettingsRecordedOnMain -args -record
//
// Never write the records again from later code: a record written by the code it checks compares
// that code with itself and proves nothing. A change that means to move a field of either body is
// a change to the settings every deployed CLI reads, which the records exist to catch.

import (
	"encoding/json"
	"flag"
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
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
)

var recordSettings = flag.Bool("record", false, "write testdata/settings-recorded-on-main-*.json instead of comparing; run only on main, as settings_recorded_test.go's head comment says")

// TestSettingsRecordedOnMain: GET /v1/settings for a broker on AWS Secrets Manager and for one on
// Google Secret Manager answers, field for field, the body main answered for the same
// configuration. With -record it writes the records instead, which the head comment allows on main
// alone.
func TestSettingsRecordedOnMain(t *testing.T) {
	for _, row := range []struct {
		store string
		deps  api.Deps
	}{
		{"aws", api.Deps{SecretsStore: aws.Name, SecretsPrefix: "example/agent-secrets/",
			SecretsKMSKey: "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"}},
		{"gcp", api.Deps{SecretsStore: gcp.Name, SecretsPrefix: "projects/000000000000/secrets/agent-secrets-",
			SecretsKMSKey: "projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"}},
	} {
		t.Run(row.store, func(t *testing.T) {
			record := filepath.Join("testdata", "settings-recorded-on-main-"+row.store+".json")
			mux := http.NewServeMux()
			api.Register(mux, row.deps)
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
			if *recordSettings {
				raw, err := json.MarshalIndent(got, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(record, append(raw, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %d fields to %s", len(got), record)
				return
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
