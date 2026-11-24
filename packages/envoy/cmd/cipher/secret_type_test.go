package main

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/buildversion"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

func TestSecretTypeWritesAndDisplaysBothStores(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStdin, secretStdin set at secret_test.go:385).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))
		if _, stderr, code := runSecret("create", "TYPE_NOT_A_SECRET", "--owner", "me", "--tier", "agent", "--type", "google-login"); code != 0 {
			t.Fatalf("create --type: exit %d stderr %q", code, stderr)
		}
		tags := store.tags(t, "TYPE_NOT_A_SECRET")
		if tags["type"] != "google-login" || tags["kind"] != "" {
			t.Fatalf("stored tags = %v; want type google-login and no old classifier", tags)
		}
		for _, form := range []string{"show", "list"} {
			args := []string{form}
			if form == "show" {
				args = append(args, "TYPE_NOT_A_SECRET")
			}
			stdout, stderr, code := runSecret(args...)
			if code != 0 || !strings.Contains(stdout, "google-login") || (form == "show" && !strings.Contains(stdout, "type: google-login")) || (form == "list" && !strings.Contains(stdout, "TYPE")) {
				t.Fatalf("%s: exit %d stdout %q stderr %q", form, code, stdout, stderr)
			}
			stdout, stderr, code = runSecret(append(args, "--json")...)
			var rows []map[string]any
			if form == "show" {
				var row map[string]any
				if err := json.Unmarshal([]byte(stdout), &row); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row)
			} else if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
				t.Fatal(err)
			}
			if code != 0 || len(rows) != 1 || rows[0]["type"] != "google-login" {
				t.Fatalf("%s --json: exit %d rows %v stderr %q", form, code, rows, stderr)
			}
			if _, old := rows[0]["kind"]; old {
				t.Fatalf("%s --json retains the old classifier: %v", form, rows)
			}
		}
		store.put(t, "TYPE_NOT_A_SECRET", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent, "kind": "azure-login"}, policytest.TypeValue(policy.TypeGoogleLogin))
		if _, stderr, code := runSecret("retag", "TYPE_NOT_A_SECRET", "--type", "google-login"); code != 0 {
			t.Fatalf("retag --type over an old tag: exit %d stderr %q", code, stderr)
		}
		if got := store.tags(t, "TYPE_NOT_A_SECRET")["type"]; got != "google-login" {
			t.Fatalf("retagged type = %q, want google-login", got)
		}
		if _, stderr, code := runSecret("retag", "TYPE_NOT_A_SECRET", "--type", "none"); code != 0 {
			t.Fatalf("retag --type none: exit %d stderr %q", code, stderr)
		}
		tags = store.tags(t, "TYPE_NOT_A_SECRET")
		if tags["type"] != "" || tags["kind"] != "" {
			t.Fatalf("removal leaves classifier tags: %v", tags)
		}
	})
}

func TestSecretOldClassifierFlagIsUnknown(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"create", "retag", "set", "show", "list", "delete", "restore"} {
		for _, old := range [][]string{{"--kind", "google-login"}, {"--kind=google-login"}} {
			args := append([]string{form, "TYPE_NOT_A_SECRET"}, old...)
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "flag provided but not defined") || !strings.Contains(stderr, "--type") {
				t.Errorf("%v: exit %d stderr %q; want an unknown flag naming --type", args, code, stderr)
			}
		}
	}
}

// TestATypedWriteIsRefusedByABrokerThatDoesNotReadTheTypeTag: a broker from before the type tag
// answers GET /v1/settings with no secret_type_tag. Every such broker from legion-envoy-v16.0.0
// on reads only the old kind tag and ignores the rollback guard, so it serves a secret that
// carries type alone as a stored secret and every grant of it would release the key it stores;
// only the brokers from legion-envoy-v13.19.0 to the release before v16.0.0 read the guard, and
// refuse the secret. The first release that reads the type tag is therefore the rollback floor.
// On both stores, against a broker that sends no secret_type_tag, a create, set or retag after
// which the secret holds a type exits 1 before it reads standard input or the stored value, writes
// anything or asks for a reread, naming the setting and the release this agent-secrets is from,
// and a write that leaves no type goes ahead. Against a broker that answers secret_type_tag, every
// row is written and exits 0, so the refusal is the setting's and not the row's.
func TestATypedWriteIsRefusedByABrokerThatDoesNotReadTheTypeTag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStdin, secretStdin set at secret_test.go:385).
	login := policytest.TypeValue(policy.TypeGoogleLogin)
	rotated := appValue(t, map[string]string{"type": "authorized_user", "client_id": "client", "client_secret": "secret", "refresh_token": "rotated"})
	tags := func(extra ...string) map[string]string {
		held := map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}
		for i := 0; i < len(extra); i += 2 {
			held[extra[i]] = extra[i+1]
		}
		return held
	}
	typed := tags(policy.TagType, policy.TypeGoogleLogin, policy.TagRollbackGuard, policy.RollbackGuardValue)
	for _, row := range []struct {
		name  string
		held  map[string]string // the secret's tags before the form runs, holding login; nil: no secret
		stdin string
		args  []string
		typed bool // the write leaves the secret a type
	}{
		{"create --type", nil, login, []string{"create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin}, true},
		{"set on a type secret", typed, rotated, []string{"set", "LOGIN_KEY"}, true},
		{"set on a secret of the old tag", tags(policy.LegacyTypeTag, policy.TypeGoogleLogin), rotated, []string{"set", "LOGIN_KEY"}, true},
		{"retag to a type", tags(), "", []string{"retag", "LOGIN_KEY", "--type", policy.TypeGoogleLogin}, true},
		{"retag of a type secret's tier", typed, "", []string{"retag", "LOGIN_KEY", "--tier", policy.TierHuman}, true},
		{"create with no type", nil, "stored-value", []string{"create", "LOGIN_KEY", "--owner", "me", "--tier", "agent"}, false},
		{"set on a stored secret", tags(), "stored-value", []string{"set", "LOGIN_KEY"}, false},
		{"retag --type none", typed, "", []string{"retag", "LOGIN_KEY", "--type", "none"}, false},
	} {
		for _, readsType := range []bool{false, true} {
			t.Run(row.name+map[bool]string{false: "/a broker before the type tag", true: "/a broker that reads it"}[readsType], func(t *testing.T) {
				onEachStore(t, func(t *testing.T, store typeStore, broker *secretBroker) {
					if !readsType {
						broker.readNoTypeTag()
					}
					if row.held != nil {
						store.put(t, "LOGIN_KEY", row.held, login)
					}
					useStdin(t, row.stdin)
					stdout, stderr, code := runSecret(row.args...)
					rereads, _ := broker.recorded()
					if readsType || !row.typed {
						if code != 0 || len(rereads) != 1 {
							t.Fatalf("%v: exit %d, %d rereads, stdout %q, stderr %q; want 0 after one reread", row.args, code, len(rereads), stdout, stderr)
						}
						return
					}
					if code != 1 || len(rereads) != 0 {
						t.Fatalf("%v: exit %d, %d rereads, stdout %q, stderr %q; want 1 before any reread", row.args, code, len(rereads), stdout, stderr)
					}
					if reads := store.valueReads(); reads != 0 {
						t.Fatalf("%v: the refused write read the stored value %d times; the refusal comes before any value read", row.args, reads)
					}
					if left, ok := secretStdin.(*strings.Reader); ok && left.Len() != len(row.stdin) {
						t.Fatalf("%v: the refused write read %d bytes of standard input; the refusal comes before the value is asked for", row.args, len(row.stdin)-left.Len())
					}
					for _, says := range []string{"secret_type_tag", "type tag", "as a stored secret", "nothing is written", buildversion.String()} {
						if !strings.Contains(stderr, says) {
							t.Fatalf("%v: stderr %q does not say %q", row.args, stderr, says)
						}
					}
					switch {
					case row.held == nil && store.has(t, "LOGIN_KEY"):
						t.Fatalf("%v: the refused create left LOGIN_KEY in the store, tagged %v", row.args, store.tags(t, "LOGIN_KEY"))
					case row.held != nil && (!maps.Equal(store.tags(t, "LOGIN_KEY"), row.held) || store.value(t, "LOGIN_KEY") != login):
						t.Fatalf("%v: the refused write changed LOGIN_KEY: tags %v, want %v; value changed %v", row.args, store.tags(t, "LOGIN_KEY"), row.held, store.value(t, "LOGIN_KEY") != login)
					}
				})
			})
		}
	}
}

// TestACreateOrSetExitsOneWhileTheBrokerServesAnotherType: as a retag does
// (TestARetagExitsOneWhileTheBrokerServesTheOldType), create and set read the type the broker's
// reread names, and exit 1 when it is not the type the secret holds once the write lands: a broker
// that serves a google-login secret as stored releases its refresh token to every grant, and one
// that serves a stored secret as a type mints from a value nobody checked as that type's. The
// value stands, and the refusal names the type served, the type written and the retag that asks
// for a reread; the reread naming the type written exits 0.
func TestACreateOrSetExitsOneWhileTheBrokerServesAnotherType(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	login := policytest.TypeValue(policy.TypeGoogleLogin)
	for _, row := range []struct {
		name, served string
		held         *awstest.LocalSecret // nil: create
		args         []string
		code         int
		refusal      string // what a refused row's stderr names, beside the retag it asks for
		retag        string
	}{
		{"create: served as stored", "", nil, []string{"--type", policy.TypeGoogleLogin}, 1, "serves LOGIN_KEY as a stored secret, not " + policy.TypeGoogleLogin, "--type " + policy.TypeGoogleLogin},
		{"create: served as another type", policy.TypeGitHubApp, nil, []string{"--type", policy.TypeGoogleLogin}, 1, "serves LOGIN_KEY as " + policy.TypeGitHubApp + ", not " + policy.TypeGoogleLogin, "--type " + policy.TypeGoogleLogin},
		{"create: served as written", policy.TypeGoogleLogin, nil, []string{"--type", policy.TypeGoogleLogin}, 0, "", ""},
		{"create of a stored secret: served as a type", policy.TypeGoogleLogin, nil, nil, 1, "serves LOGIN_KEY as " + policy.TypeGoogleLogin + ", not a stored secret", "--type none"},
		{"create of a stored secret: served as stored", "", nil, nil, 0, "", ""},
		{"set: served as stored", "", new(policytest.TypeSecret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, login)), nil, 1, "serves LOGIN_KEY as a stored secret, not " + policy.TypeGoogleLogin, "--type " + policy.TypeGoogleLogin},
		{"set: served as written", policy.TypeGoogleLogin, new(policytest.TypeSecret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, login)), nil, 0, "", ""},
		{"set of a stored secret: served as a type", policy.TypeGoogleLogin, new(policytest.Secret("LOGIN_KEY", "ada@example.com", policy.TierAgent, "v1")), nil, 1, "serves LOGIN_KEY as " + policy.TypeGoogleLogin + ", not a stored secret", "--type none"},
		{"set of a stored secret: served as stored", "", new(policytest.Secret("LOGIN_KEY", "ada@example.com", policy.TierAgent, "v1")), nil, 0, "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := awstest.NewLocal()
			args := []string{"set", "LOGIN_KEY"}
			if row.held != nil {
				local = awstest.NewLocal(*row.held)
			} else {
				args = append([]string{"create", "LOGIN_KEY", "--owner", "me", "--tier", "agent"}, row.args...)
			}
			startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Served: true, Type: row.served} })
			useAWS(t, local, testAccount, adaSignIn)
			useStdin(t, login)
			stdout, stderr, code := runSecret(args...)
			if code != row.code {
				t.Fatalf("%v with the broker serving type %q: exit %d, stdout %q, stderr %q; want %d", args, row.served, code, stdout, stderr, row.code)
			}
			if valueOf(t, local, "LOGIN_KEY") != login {
				t.Fatalf("%v: LOGIN_KEY does not hold the value written", args)
			}
			if row.code == 0 {
				return
			}
			for _, says := range []string{"the value of LOGIN_KEY in", "stands", row.refusal, "secret retag LOGIN_KEY " + row.retag} {
				if !strings.Contains(stderr, says) {
					t.Fatalf("%v: stderr %q does not say %q", args, stderr, says)
				}
			}
		})
	}
}
