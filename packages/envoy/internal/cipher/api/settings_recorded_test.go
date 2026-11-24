package api_test

// This file holds GET /v1/settings to what main answered for one broker on each store, before the
// store packages moved: an AWS or a GCP deployment upgrades with no change to the fields its CLIs
// read. testdata/settings-recorded-on-main-<store>.json was written at main 67bcd7c78393 by this
// file as it was at 414d2709f4b4, in a worktree of that commit, from the root of the checkout that
// holds this file:
//
//	git worktree add --detach <tmp>/main 67bcd7c78393
//	git show 414d2709f4b4:packages/envoy/internal/cipher/api/settings_recorded_test.go > <tmp>/main/packages/envoy/internal/cipher/api/settings_recorded_test.go
//	go test -C <tmp>/main/packages/envoy ./internal/cipher/api/ -run TestSettingsRecordedOnMain -args -record
//	cp <tmp>/main/packages/envoy/internal/cipher/api/testdata/settings-recorded-on-main-*.json packages/envoy/internal/cipher/api/testdata/
//	git worktree remove --force <tmp>/main
//
// The -record flag went with the store packages' move, so this file writes no record: a record
// written by the code it checks compares that code with itself and proves nothing. A change to main
// that adds, drops or changes a field of either body fails this test in its own pull request: that
// change edits the record there, in the form -record wrote it (json.MarshalIndent with two-space
// indents and a closing newline), which the test holds the file to, and names itself here with the
// lines of the records that moved. The field is part of the settings every deployed CLI reads, so
// that pull request's review decides whether it may move.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
)

// TestSettingsRecordedOnMain: GET /v1/settings for a broker on AWS Secrets Manager and for one on
// Google Secret Manager, each configured by its registry entry from the variables a deployment
// sets, answers, field for field, the body main answered for the same configuration. Each broker
// also names a Google exchange audience and serves keys the model judge decides (judgedPolicy), so
// each field the body can carry is in its record: the comparison walks the fields of the body and
// the record, and a field neither holds is never compared.
func TestSettingsRecordedOnMain(t *testing.T) {
	judged := &requests.Machine{Policy: judgedPolicy(t)}
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
			api.Register(mux, api.Deps{SecretsStore: b.Name(), SecretsPrefix: cfg.Prefix(), SettingsFields: cfg.Settings(),
				GoogleExchangeAudience: "agent-secrets-vm", Machine: judged})
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
			if again, err := json.MarshalIndent(want, "", "  "); err != nil || !bytes.Equal(append(again, '\n'), raw) {
				t.Fatalf("%s holds an entry or a form -record did not write (%s), which the comparison would not read; write it as -record did", record, recordForm(raw, "field"))
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

// judgedPolicy is a live policy serving two keys the model judge decides, so a body's judge_keys
// carries each of its fields: DEEL_READ, a profile made from the stored secret DEEL_API_KEY, at that
// secret's 900 seconds and guidance, naming it as its source; and SHARED_KEY, at 300 seconds with
// no guidance. The keys' store is the AWS fake for both rows: the settings name the judge keys
// from the policy alone, whatever store the broker reads.
func judgedPolicy(t *testing.T) *policy.Current {
	t.Helper()
	local := awstest.NewLocal()
	local.Put(policytest.Judged(policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierHuman, "deel-v1"), 900, "grant for the payroll export job"))
	local.Put(policytest.Judged(policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1"), 300, ""))
	return policytest.ProfiledCurrent(t, local, policytest.KindsFile(`{"name": "DEEL_READ", "source": "DEEL_API_KEY", "proxy": "example"}`))
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

// recordForm names what makes a record other than what -record wrote: each top-level key it holds
// more than once, of which decoding keeps the last, or else a form written by hand.
func recordForm(raw []byte, entry string) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return err.Error()
	}
	seen, repeated := map[string]bool{}, []string{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err.Error()
		}
		if name, _ := key.(string); seen[name] {
			repeated = append(repeated, fmt.Sprintf("the %s %q more than once", entry, name))
		} else {
			seen[name] = true
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err.Error()
		}
	}
	if len(repeated) == 0 {
		return "an entry or a layout written by hand"
	}
	return strings.Join(repeated, ", ")
}
