package backends_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
)

// secondPrefixRow is what TestEachStoreTakesAStandOnTheSecondPrefix needs of one store: a
// configuration its ParseConfig accepts (prefix and env), a second prefix to set beside it, whether
// the store refuses every second prefix, and, for a store that reads one in a form of its own, a
// second prefix of another form ("" for a store with no form).
type secondPrefixRow struct {
	prefix    string
	env       map[string]string
	second    string
	refuses   bool
	malformed string
}

// secondPrefixRows are the stores' rows, by Backend.ID. A store the registry holds that has no row
// here fails the test: adding a store means saying here whether it reads a second prefix.
var secondPrefixRows = map[string]secondPrefixRow{
	"aws": {
		prefix:  "example/agent-secrets/",
		env:     map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"},
		second:  "example/old-agent-secrets/",
		refuses: true,
	},
	"gcp": {
		prefix:    "projects/000000000000/secrets/agent-secrets-",
		env:       map[string]string{"BROKER_SECRETS_KMS_KEY_NAME": "projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"},
		second:    "projects/000000000000/secrets/old-agent-secrets-",
		malformed: "projects/example-project/secrets/old-agent-secrets-",
	},
	"memory": {
		prefix: "example/agent-secrets/",
		env:    map[string]string{"BROKER_MEMORY_SECRETS_FILE": "/run/agent-secrets/memory-secrets.json"},
		second: "example/old-agent-secrets/",
	},
}

// TestEachStoreTakesAStandOnTheSecondPrefix: every store the registry holds takes a stand on
// BROKER_SECRETS_SECOND_PREFIX (secretstore.SecondPrefixVariable), which config.Load reads only
// through the store (Config.SecondPrefix). With the variable set, its ParseConfig either refuses
// the start, naming the variable, or reads it and answers it as SecondPrefix, and both prefixes in
// its sources' own form (SourcePrefixes), which the grant re-checks compare in; its row says
// which. One that does neither would start a broker that counts no move and says nothing. A store
// that reads one in a form of its own refuses a value of another form, naming the variable. With
// the variable unset, every store answers no second prefix, in either form.
//
// The named reverts: have the memory store's ParseConfig drop the value, and the memory row fails
// as neither refusing nor reading; have it answer no second source prefix, and the memory row
// fails naming SourcePrefixes; take a store's row out, and the test names the store.
func TestEachStoreTakesAStandOnTheSecondPrefix(t *testing.T) {
	stores := backends.All()
	if len(stores) == 0 {
		t.Fatal("UNKNOWN: the registry holds no store, so no stand was read")
	}
	for _, b := range stores {
		t.Run(b.ID(), func(t *testing.T) {
			row, ok := secondPrefixRows[b.ID()]
			if !ok {
				t.Fatalf("store %s has no row in secondPrefixRows: say whether it reads %s (answering it as Config.SecondPrefix) or refuses it", b.ID(), secretstore.SecondPrefixVariable)
			}
			getenv := func(second string) func(string) string {
				return func(name string) string {
					if name == secretstore.SecondPrefixVariable {
						return second
					}
					return row.env[name]
				}
			}
			base, err := b.ParseConfig(row.prefix, getenv(""))
			if err != nil {
				t.Fatalf("ParseConfig with no second prefix = %v; want the row's configuration accepted", err)
			}
			if got := base.SecondPrefix(); got != "" {
				t.Fatalf("SecondPrefix with the variable unset = %q, want none", got)
			}
			if _, second := base.SourcePrefixes(); second != "" {
				t.Fatalf("SourcePrefixes with the variable unset answers the second source prefix %q, want none", second)
			}
			cfg, err := b.ParseConfig(row.prefix, getenv(row.second))
			switch {
			case err != nil && !strings.Contains(err.Error(), secretstore.SecondPrefixVariable):
				t.Fatalf("ParseConfig with %s=%s = %v; a refusal must name the variable", secretstore.SecondPrefixVariable, row.second, err)
			case err != nil && !row.refuses:
				t.Fatalf("ParseConfig with %s=%s refused it (%v); its row says the store reads one", secretstore.SecondPrefixVariable, row.second, err)
			case err != nil:
				return
			case row.refuses:
				t.Fatalf("ParseConfig with %s=%s accepted it, answering %q; its row says the store refuses every one", secretstore.SecondPrefixVariable, row.second, cfg.SecondPrefix())
			case cfg.SecondPrefix() != row.second:
				t.Fatalf("ParseConfig with %s=%s accepted it and answers %q: the store neither refuses the variable nor reads it", secretstore.SecondPrefixVariable, row.second, cfg.SecondPrefix())
			}
			// A store that reads one hands the grant re-checks both prefixes in its sources' own
			// form (SourcePrefixes); with either empty, or the two the same, they count no move.
			if prefix, second := cfg.SourcePrefixes(); prefix == "" || second == "" || prefix == second {
				t.Fatalf("SourcePrefixes = %q, %q; a store that reads %s answers both prefixes in its sources' form, and two of them", prefix, second, secretstore.SecondPrefixVariable)
			}
			if row.malformed == "" {
				return
			}
			if _, err := b.ParseConfig(row.prefix, getenv(row.malformed)); err == nil || !strings.Contains(err.Error(), secretstore.SecondPrefixVariable) {
				t.Fatalf("ParseConfig with %s=%s, of another form than the store's = %v; want a refusal naming the variable", secretstore.SecondPrefixVariable, row.malformed, err)
			}
		})
	}
}
