package secrets_test

// This file holds the source of each secret the GCP store lists, and the source the policy serves
// it by, to what main answered before the store packages moved. Postgres holds a grant's source
// (request_secrets.source), so a source whose format changes ends every live grant of a GCP
// deployment that upgrades. testdata/source-recorded-on-main-gcp.json was written once, on main,
// by this file as it was then:
//
//	go test ./internal/cipher/secrets/ -run TestSourceFormatRecordedOnMain -args -record
//
// Never write the record again from later code: a record written by the code it checks compares
// that code with itself and proves nothing. A change that means to move a source ends the grants
// that hold it, which the record exists to catch.

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

var recordSources = flag.Bool("record", false, "write testdata/source-recorded-on-main-*.json instead of comparing; run only on main, as source_recorded_test.go's head comment says")

// recordedSource is one secret's sources as a record holds them: the source its store's listing
// answered, and the one the policy serves it by, which a grant stores.
type recordedSource struct {
	Listed string `json:"listed"`
	Source string `json:"source"`
}

// recordedSecrets are the two secrets each store holds under its prefix, on the agent-secrets key:
// a stored secret, and one whose type tag makes the policy serve it by a type source. The policy
// loads a google-login secret with no profile, so it is the typed one.
var recordedSecrets = []struct {
	slug  string
	tags  map[string]string
	value string
}{
	{"deel-api-key", map[string]string{"owner": "ada@example.com", "tier": "agent"}, "deel-value"},
	{"google-login", map[string]string{"owner": "ada@example.com", "tier": "human", "type": "google-login"},
		`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`},
}

// TestSourceFormatRecordedOnMain: the GCP store lists each secret with the source main listed it
// with, and the policy serves it by the source main served it by. With -record it writes the
// record instead, which the head comment allows on main alone.
func TestSourceFormatRecordedOnMain(t *testing.T) {
	for _, row := range []struct {
		store  string
		prefix string
		open   func(t *testing.T, prefix string) secretstore.Store
	}{
		{"gcp", "projects/000000000000/secrets/agent-secrets-", func(t *testing.T, prefix string) secretstore.Store {
			const key = "projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
			held := map[string]secrets.FakeGCPSecretForTest{}
			for _, s := range recordedSecrets {
				held[prefix+s.slug] = secrets.FakeGCPSecretForTest{Tags: s.tags, Value: s.value}
			}
			return secrets.NewGCP(secrets.StartFakeGCPForTest(t, key, held), key)
		}},
	} {
		t.Run(row.store, func(t *testing.T) {
			record := filepath.Join("testdata", "source-recorded-on-main-"+row.store+".json")
			store := row.open(t, row.prefix)
			ctx := context.Background()
			listings, err := store.List(ctx, row.prefix, secretstore.ReadOptions{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			set, err := policy.Loader{Store: store, Prefix: row.prefix}.Load(ctx)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := map[string]recordedSource{}
			for _, ls := range listings {
				slug, ok := strings.CutPrefix(ls.ID, row.prefix)
				if !ok {
					t.Fatalf("List answered %s, which is not under the prefix %s", ls.ID, row.prefix)
				}
				name := policy.SlugToName(slug)
				secret, served := set.Secrets[name]
				if !served {
					t.Fatalf("%s is not served (refused: %+v); want each recorded secret served", name, set.Refused)
				}
				got[name] = recordedSource{Listed: ls.Source, Source: secret.Source}
			}
			if len(got) != len(recordedSecrets) {
				t.Fatalf("List answered %d secrets, want the %d this test holds: %+v", len(got), len(recordedSecrets), listings)
			}
			if *recordSources {
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
				t.Logf("wrote %d secrets' sources to %s", len(got), record)
				return
			}
			raw, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("read %s: %v", record, err)
			}
			var want map[string]recordedSource
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode %s: %v", record, err)
			}
			if len(want) == 0 {
				t.Fatalf("UNKNOWN: %s records no secret, so the check compares nothing", record)
			}
			var names []string
			for name := range got {
				names = append(names, name)
			}
			for name := range want {
				if _, both := got[name]; !both {
					names = append(names, name)
				}
			}
			slices.Sort(names)
			for _, name := range names {
				g, w := got[name], want[name]
				if g.Listed != w.Listed {
					t.Errorf("%s: listed source %q, recorded on main %q", name, g.Listed, w.Listed)
				}
				if g.Source != w.Source {
					t.Errorf("%s: source %q, recorded on main %q", name, g.Source, w.Source)
				}
			}
		})
	}
}
