package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// The answers block's pieces a table row assembles: one provider of each credential source, a
// price for each provider's model, the two database stores' runtime.kubernetes lines, and the
// warning each of those stores gets for a block that names no connection to it, and the one a
// block that names no model provider gets under any runtime.
const (
	anthropicByFile   = "    anthropic: {base_url: \"https://gateway.internal.example/anthropic/\", api_key_file: keys/anthropic}\n"
	openaiByCommand   = "    openai: {base_url: https://gateway.internal.example/openai, api_key_command: printf openai-key}\n"
	opusPrice         = "    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n"
	gptPrice          = "    openai/gpt-6.1: {input: 1.25, output: 10, cache_write: 0, cache_read: 0.125}\n"
	cloudSQLStore     = "    session_store: cloudsql-iam\n    session_database: {instance: example-project:us-central1:example-instance, name: legion_sessions, user: legion-sessions@example-project.iam}\n"
	daemonDSNFile     = "  conversation_store: {dsn_file: conversations-dsn}\n"
	daemonIAMLogin    = "  conversation_store: {user: legion-daemon@example-project.iam}\n"
	noStoreKey        = "  conversation_store: {}\n"
	answersOffDSN     = "answers is set, but the answer loop is off: under runtime.kubernetes.session_store postgres, answers.conversation_store.dsn_file names the daemon's connection to the conversation store, and the file sets none, so a message to a stopped agent is refused and dropped as it is without answers; to turn the loop on, set answers.conversation_store.dsn_file to the file holding the postgres:// URL of the database the conversations live in"
	answersOffUser    = "answers is set, but the answer loop is off: under runtime.kubernetes.session_store cloudsql-iam, answers.conversation_store.user names the daemon's connection to the conversation store, and the file sets none, so a message to a stopped agent is refused and dropped as it is without answers; to turn the loop on, set answers.conversation_store.user to the IAM login the daemon signs in to runtime.kubernetes.session_database's instance as"
	answersOffNoModel = "answers is set, but the answer loop is off: answers.providers names no model provider the loop may call, so a message to a stopped agent is refused and dropped as it is without answers; to turn the loop on, set answers.providers to at least one provider (anthropic, openai) with its base_url and its key, and answers.prices to the price of each model the loop may answer on"
	// answersOffNoStoreEither ends the provider line where a Kubernetes file names no connection to
	// the conversation store either.
	answersOffNoStoreEither = ", and answers.conversation_store to the daemon's connection to the conversation store as well"
	tmuxPostgresStore       = "runtime: {tmux: {session_store: postgres, session_dsn_file: session-dsn}}\n"
)

// answersBlock is an `answers` block of providers, prices and whatever else rest adds.
func answersBlock(providers, prices, rest string) string {
	return "answers:\n  providers:\n" + providers + "  prices:\n" + prices + rest
}

// No `answers`, or `answers: null`, leaves the loop off under either runtime: the daemon runs as
// it did before the block existed.
func TestNoAnswersBlockLeavesTheLoopOff(t *testing.T) {
	for name, body := range map[string]string{
		"tmux, absent":         minimalFile,
		"tmux, null":           minimalFile + "answers: null\n",
		"kubernetes, null":     "answers:\n" + kubernetesFile,
		"kubernetes, postgres": kubernetesFile,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, body), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Answers != nil {
				t.Errorf("Answers = %+v, want nil: no block, no loop", cfg.Answers)
			}
		})
	}
}

// Both providers settle, one keyed by a file resolved against the file's directory and one by a
// command kept as written, with their base URLs' trailing slashes dropped; every price is read;
// the bounds a file leaves out are the defaults; and under tmux the daemon needs no connection to
// the conversations, which are files on its own host.
func TestAnswersSettlesBothProvidersTheirPricesAndTheDefaultBounds(t *testing.T) {
	path := writeConfigFile(t, minimalFile+answersBlock(anthropicByFile+openaiByCommand, opusPrice+gptPrice, ""))
	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.Answers == nil {
		t.Fatal("Answers = nil, want the block settled")
	}
	want := Answers{
		Providers: map[AnswerProvider]AnswerEndpoint{
			AnswerAnthropic: {BaseURL: "https://gateway.internal.example/anthropic", Credential: AnswerCredential{
				File: filepath.Join(filepath.Dir(path), "keys/anthropic"), key: "answers.providers.anthropic"}},
			AnswerOpenAI: {BaseURL: "https://gateway.internal.example/openai", Credential: AnswerCredential{
				Command: "printf openai-key", key: "answers.providers.openai"}},
		},
		Prices: map[string]AnswerPrice{
			"anthropic/claude-opus-5-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5},
			"openai/gpt-6.1":            {Input: 1.25, Output: 10, CacheWrite: 0, CacheRead: 0.125},
		},
		Bounds: AnswerBounds{
			Answer: 180 * time.Second, ModelCalls: 8, OutputTokens: 4000, GHCall: 30 * time.Second,
			GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 50,
		},
	}
	if !reflect.DeepEqual(*cfg.Answers, want) {
		t.Errorf("Answers = %+v\nwant %+v", *cfg.Answers, want)
	}
}

// Each bound the file sets is the file's, a decimal daily limit included, and each it leaves out
// keeps its default.
func TestAnswersBoundsTheFileSetsOverrideTheDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, bounds string
		want         AnswerBounds
	}{
		{
			"every bound",
			"  bounds: {answer_seconds: 60, model_calls: 1, output_tokens: 16, gh_seconds: 5, gh_output_chars: 100, gh_calls: 2, concurrent: 1, daily_usd: 0.01}\n",
			AnswerBounds{Answer: time.Minute, ModelCalls: 1, OutputTokens: 16, GHCall: 5 * time.Second, GHOutputChars: 100, GHCalls: 2, Concurrent: 1, DailyUSD: 0.01},
		},
		{
			"two bounds",
			"  bounds: {model_calls: 3, daily_usd: 75}\n",
			AnswerBounds{Answer: 180 * time.Second, ModelCalls: 3, OutputTokens: 4000, GHCall: 30 * time.Second, GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 75},
		},
		{
			"an empty mapping",
			"  bounds: {}\n",
			defaultAnswerBounds(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+answersBlock(anthropicByFile, opusPrice, tc.bounds)), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Answers.Bounds != tc.want {
				t.Errorf("Bounds = %+v, want %+v", cfg.Answers.Bounds, tc.want)
			}
		})
	}
}

// conversation_store is the key the runtime's own store needs, and only that one: dsn_file under
// session_store postgres, user under cloudsql-iam (with session_database's instance and database),
// neither under tmux, whose postgres store's runtime.tmux.session_dsn_file is the conversation
// store's. A Kubernetes file that sets the block and leaves that key out, the block's
// conversation_store with it or empty, loads with the loop off, as with no block, and one warning
// saying so; a file that names the key raises none.
func TestAnswersConversationStoreFollowsTheRuntimesStore(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       ConversationStore // with dsn_file relative to the file's directory
		refusal    string
		warning    string // the one warning of a block the loader leaves off
	}{
		{
			name: "postgres, dsn_file",
			body: kubernetesFile + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			want: ConversationStore{DSNFile: "conversations-dsn"},
		},
		{
			name:    "postgres, no conversation_store",
			body:    kubernetesFile + answersBlock(anthropicByFile, opusPrice, ""),
			warning: answersOffDSN,
		},
		{
			name:    "postgres, an empty conversation_store",
			body:    kubernetesFile + answersBlock(anthropicByFile, opusPrice, noStoreKey),
			warning: answersOffDSN,
		},
		{
			name:    "postgres, user alone",
			body:    kubernetesFile + answersBlock(anthropicByFile, opusPrice, daemonIAMLogin),
			refusal: "answers.conversation_store.user is not used when runtime.kubernetes.session_store is postgres; remove it",
		},
		{
			name: "cloudsql-iam, user",
			body: withSessionStore(cloudSQLStore) + answersBlock(anthropicByFile, opusPrice, daemonIAMLogin),
			want: ConversationStore{Database: ompsessions.Database{
				Instance: "example-project:us-central1:example-instance", Name: "legion_sessions", User: "legion-daemon@example-project.iam",
			}},
		},
		{
			name:    "cloudsql-iam, no conversation_store",
			body:    withSessionStore(cloudSQLStore) + answersBlock(anthropicByFile, opusPrice, ""),
			warning: answersOffUser,
		},
		{
			name:    "cloudsql-iam, an empty conversation_store",
			body:    withSessionStore(cloudSQLStore) + answersBlock(anthropicByFile, opusPrice, noStoreKey),
			warning: answersOffUser,
		},
		{
			name:    "cloudsql-iam, dsn_file",
			body:    withSessionStore(cloudSQLStore) + answersBlock(anthropicByFile, opusPrice, "  conversation_store: {dsn_file: conversations-dsn, user: legion-daemon@example-project.iam}\n"),
			refusal: "answers.conversation_store.dsn_file is not used when runtime.kubernetes.session_store is cloudsql-iam; remove it",
		},
		{
			name:    "cloudsql-iam, dsn_file alone",
			body:    withSessionStore(cloudSQLStore) + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			refusal: "answers.conversation_store.dsn_file is not used when runtime.kubernetes.session_store is cloudsql-iam; remove it",
		},
		{
			name: "tmux, no conversation_store",
			body: minimalFile + answersBlock(anthropicByFile, opusPrice, ""),
		},
		{
			name:    "tmux, a conversation_store",
			body:    minimalFile + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			refusal: "answers.conversation_store is not used under runtime tmux: every conversation is a file on the daemon's own host, which it reads where the pane wrote it; remove it",
		},
		{
			name:    "tmux, an empty conversation_store",
			body:    minimalFile + answersBlock(anthropicByFile, opusPrice, noStoreKey),
			refusal: "answers.conversation_store is not used under runtime tmux: every conversation is a file on the daemon's own host, which it reads where the pane wrote it; remove it",
		},
		{
			name: "tmux postgres, no conversation_store",
			body: minimalFile + tmuxPostgresStore + answersBlock(anthropicByFile, opusPrice, ""),
			want: ConversationStore{DSNFile: "session-dsn"},
		},
		{
			name:    "tmux postgres, a conversation_store",
			body:    minimalFile + tmuxPostgresStore + answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			refusal: "answers.conversation_store is not used under runtime tmux: the daemon reads every conversation from the database runtime.tmux.session_dsn_file names, where its panes keep them; remove it",
		},
		{
			name: "tmux files named, no conversation_store",
			body: minimalFile + "runtime: {tmux: {session_store: files}}\n" + answersBlock(anthropicByFile, opusPrice, ""),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.body)
			cfg, err := LoadForValidation(path, noEnv)
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("LoadForValidation = %v, want %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if tc.warning != "" {
				if cfg.Answers != nil {
					t.Errorf("Answers = %+v, want nil: no connection to the conversation store, no loop", cfg.Answers)
				}
				if !reflect.DeepEqual(cfg.Warnings, []string{tc.warning}) {
					t.Errorf("Warnings = %#v, want the one line %q", cfg.Warnings, tc.warning)
				}
				return
			}
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %#v, want none for a block that names its store", cfg.Warnings)
			}
			want := tc.want
			if want.DSNFile != "" {
				want.DSNFile = filepath.Join(filepath.Dir(path), want.DSNFile)
			}
			if cfg.Answers.ConversationStore != want {
				t.Errorf("ConversationStore = %+v, want %+v", cfg.Answers.ConversationStore, want)
			}
		})
	}
}

// A block that names no connection to the conversation store is still read whole and held to its
// rules before the store is looked at: a malformed provider, price or bound under either database
// store is refused naming its key, never loaded with the loop off.
func TestAnswersWithNoConversationStoreStillRefusesAMalformedBlock(t *testing.T) {
	for _, store := range []struct{ name, file string }{
		{"postgres", kubernetesFile},
		{"cloudsql-iam", withSessionStore(cloudSQLStore)},
	} {
		for _, tc := range []struct{ name, block, want string }{
			{"a provider with no key", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic}\n", opusPrice, ""),
				"answers.providers.anthropic requires exactly one of api_key_file or api_key_command"},
			{"a price of a provider not configured, conversation_store empty", answersBlock(anthropicByFile, opusPrice+gptPrice, noStoreKey),
				"answers.prices.openai/gpt-6.1 prices a model of openai, which answers.providers does not name, so the loop never calls it; remove it or configure the provider"},
			{"a zero daily limit", answersBlock(anthropicByFile, opusPrice, "  bounds: {daily_usd: 0}\n"), "answers.bounds.daily_usd must be a positive number"},
			{"an unknown conversation_store key", answersBlock(anthropicByFile, opusPrice, "  conversation_store: {password: hunter2}\n"),
				"unknown key answers.conversation_store.password"},
		} {
			t.Run(store.name+", "+tc.name, func(t *testing.T) {
				_, err := LoadForValidation(writeConfigFile(t, store.file+tc.block), noEnv)
				if err == nil || err.Error() != tc.want {
					t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
				}
			})
		}
	}
}

// A Kubernetes file whose session store keeps sessions as files, `pvc` or an omitted
// session_store, is refused with the session store's own sentence whether it sets `answers` or
// not: the loop being off is no way past that refusal.
func TestAnswersUnderAFileSessionStoreIsRefusedAsWithoutTheBlock(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"session_store omitted", withSessionStore("")},
		{"session_store pvc", kubernetesWith("    session_store: postgres\n", "    session_store: pvc\n")},
	} {
		for name, block := range map[string]string{
			"without answers":                 "",
			"with answers, no store":          answersBlock(anthropicByFile, opusPrice, ""),
			"with answers and a dsn_file":     answersBlock(anthropicByFile, opusPrice, daemonDSNFile),
			"with answers and an empty store": answersBlock(anthropicByFile, opusPrice, noStoreKey),
		} {
			t.Run(tc.name+", "+name, func(t *testing.T) {
				_, err := LoadForValidation(writeConfigFile(t, tc.file+block), noEnv)
				if err == nil || err.Error() != sessionStoreRefusal {
					t.Fatalf("LoadForValidation = %v, want %q", err, sessionStoreRefusal)
				}
			})
		}
	}
}

// A block whose `providers` is absent, null or empty names no model the loop may call, so it loads
// with the loop off, as with no block, under either runtime and either database store, with or
// without its conversation store: one warning, the provider line, which is decided before the
// store, so a block that names neither gets that line alone. With no provider, `prices` may be
// absent, null or empty, since there is nothing to price.
func TestAnswersThatNameNoProviderLeaveTheLoopOff(t *testing.T) {
	// want is the one line: the provider line, which under Kubernetes names the conversation
	// store too where the file names no connection to it either.
	type runtimeFile struct{ name, file, store, want string }
	runtimes := []runtimeFile{
		{"tmux", minimalFile, "", answersOffNoModel},
		{"postgres, no conversation_store", kubernetesFile, "", answersOffNoModel + answersOffNoStoreEither},
		{"postgres, an empty conversation_store", kubernetesFile, noStoreKey, answersOffNoModel + answersOffNoStoreEither},
		{"postgres, dsn_file", kubernetesFile, daemonDSNFile, answersOffNoModel},
		{"cloudsql-iam, no conversation_store", withSessionStore(cloudSQLStore), "", answersOffNoModel + answersOffNoStoreEither},
		{"cloudsql-iam, user", withSessionStore(cloudSQLStore), daemonIAMLogin, answersOffNoModel},
	}
	blocks := map[string]string{
		"providers absent":                 "answers:\n  bounds: {concurrent: 2}\n",
		"providers null":                   "answers:\n  providers: null\n",
		"providers empty":                  "answers:\n  providers: {}\n",
		"providers absent, prices empty":   "answers:\n  prices: {}\n",
		"providers empty, prices null":     "answers:\n  providers: {}\n  prices: null\n",
		"providers null, every bound read": "answers:\n  providers: null\n  bounds: {answer_seconds: 60, daily_usd: 0.5}\n",
	}
	for _, rt := range runtimes {
		for name, block := range blocks {
			t.Run(rt.name+", "+name, func(t *testing.T) {
				cfg, err := LoadForValidation(writeConfigFile(t, rt.file+block+rt.store), noEnv)
				if err != nil {
					t.Fatalf("LoadForValidation: %v", err)
				}
				if cfg.Answers != nil {
					t.Errorf("Answers = %+v, want nil: no model provider, no loop", cfg.Answers)
				}
				if !reflect.DeepEqual(cfg.Warnings, []string{rt.want}) {
					t.Errorf("Warnings = %#v, want the one line %q", cfg.Warnings, rt.want)
				}
			})
		}
	}
	for _, tc := range []struct{ name, body, want string }{
		{"tmux, an empty block", minimalFile + "answers: {}\n", answersOffNoModel},
		{"postgres, an empty block", kubernetesFile + "answers: {}\n", answersOffNoModel + answersOffNoStoreEither},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, tc.body), noEnv)
			if err != nil || cfg.Answers != nil || !reflect.DeepEqual(cfg.Warnings, []string{tc.want}) {
				t.Fatalf("LoadForValidation = answers %+v, warnings %#v, %v; want no loop and the one line %q", cfg.Answers, cfg.Warnings, err, tc.want)
			}
		})
	}
}

// Under `dispatch_token_broker: true` the daemon's Dispatch credential acts only as its own
// session, legion-daemon:<PROJECT>, so Dispatch refuses a reply the loop writes in a stopped
// agent's name, and the loop's dispatch tool has no Dispatch token for an agent's session. A
// Kubernetes file that sets the broker and a complete `answers` block loads with the loop off, as
// with no block, and one warning, the broker line, which stands alone where the block names no
// provider and no store either. Its control, the same file with dispatch_token_file in place of
// the broker, turns the loop on and warns of nothing.
func TestAnswersBesideTheDispatchTokenBrokerLeaveTheLoopOff(t *testing.T) {
	const answersOffBroker = "answers is set, but the answer loop is off: dispatch_token_broker is set, so the daemon's Dispatch credential acts only as legion-daemon:LEGSMOKE; Dispatch refuses a reply written in a stopped agent's name with it, and the loop's dispatch tool has no Dispatch token for an agent's session; a message to a stopped agent is handled as it is without answers"
	broker := kubernetesWith(dispatchTokenLine, brokerOn) + exchangeAudienceBlock
	complete := answersBlock(anthropicByFile, opusPrice, daemonDSNFile)
	for name, block := range map[string]string{
		"a complete block":                      complete,
		"a block with no provider and no store": "answers:\n  providers: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, broker+block), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if !cfg.DispatchTokenBroker || cfg.Answers != nil {
				t.Errorf("DispatchTokenBroker = %t, Answers = %+v; want the broker on and nil: under the broker, no loop", cfg.DispatchTokenBroker, cfg.Answers)
			}
			if !reflect.DeepEqual(cfg.Warnings, []string{answersOffBroker}) {
				t.Errorf("Warnings = %#v, want the one line %q", cfg.Warnings, answersOffBroker)
			}
		})
	}
	t.Run("control, dispatch_token_file in place of the broker", func(t *testing.T) {
		cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+exchangeAudienceBlock+complete), noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation: %v", err)
		}
		if cfg.DispatchTokenBroker || cfg.Answers == nil || len(cfg.Warnings) != 0 {
			t.Fatalf("DispatchTokenBroker = %t, Answers = %+v, Warnings = %#v; want the broker off, the loop on and no warning",
				cfg.DispatchTokenBroker, cfg.Answers, cfg.Warnings)
		}
	})
}

// A block that names no model provider is still read whole and held to every rule that does not
// need a provider: `providers` that is no mapping or whose providers are written with no value,
// an unknown key anywhere in it, any model `prices` names (its provider is then one the block
// does not name), a `prices` that is no mapping, a bad bound, and a conversation_store key the
// runtime's store does not use are each refused as they are with a provider, never turned off.
func TestAnswersThatNameNoProviderStillRefuseAMalformedBlock(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"providers not a mapping", minimalFile + "answers:\n  providers: [anthropic]\n",
			"answers.providers must be a mapping of provider (anthropic, openai) to its base_url and key"},
		{"a provider written with no value", kubernetesFile + "answers:\n  providers: {anthropic: null}\n",
			"answers.providers names no provider with an endpoint: give anthropic or openai its base_url and its key, or remove the key to leave the loop off"},
		{"an unknown provider written with no value", minimalFile + "answers:\n  providers: {gemini: null}\n",
			"unknown key answers.providers.gemini"},
		{"an unknown key", kubernetesFile + "answers:\n  model: anthropic/claude-opus-5-5\n", "unknown key answers.model"},
		{"a model priced", kubernetesFile + "answers:\n  prices:\n" + opusPrice,
			"answers.prices.anthropic/claude-opus-5-5 prices a model of anthropic, which answers.providers does not name, so the loop never calls it; remove it or configure the provider"},
		{"a model priced under an empty providers", minimalFile + "answers:\n  providers: {}\n  prices:\n" + gptPrice,
			"answers.prices.openai/gpt-6.1 prices a model of openai, which answers.providers does not name, so the loop never calls it; remove it or configure the provider"},
		{"a price for no model id", minimalFile + "answers:\n  prices:\n    claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n",
			`answers.prices names "claude-opus-5-5", which is no model id: <provider>/<model>, its provider anthropic or openai`},
		{"prices not a mapping", kubernetesFile + "answers:\n  prices: [anthropic/claude-opus-5-5]\n",
			"answers.prices must be a mapping of model id (<provider>/<model>) to its prices"},
		{"a zero daily limit", kubernetesFile + "answers:\n  bounds: {daily_usd: 0}\n", "answers.bounds.daily_usd must be a positive number"},
		{"an unknown bound", minimalFile + "answers:\n  providers: null\n  bounds: {retries: 2}\n", "unknown key answers.bounds.retries"},
		{"an unknown conversation_store key", kubernetesFile + "answers:\n  conversation_store: {password: hunter2}\n",
			"unknown key answers.conversation_store.password"},
		{"tmux, a conversation_store", minimalFile + "answers:\n  providers: {}\n" + daemonDSNFile,
			"answers.conversation_store is not used under runtime tmux: every conversation is a file on the daemon's own host, which it reads where the pane wrote it; remove it"},
		{"postgres, user", kubernetesFile + "answers:\n  providers: {}\n" + daemonIAMLogin,
			"answers.conversation_store.user is not used when runtime.kubernetes.session_store is postgres; remove it"},
		{"cloudsql-iam, dsn_file", withSessionStore(cloudSQLStore) + "answers:\n" + daemonDSNFile,
			"answers.conversation_store.dsn_file is not used when runtime.kubernetes.session_store is cloudsql-iam; remove it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
		})
	}
}

// Every shape the block may not take is refused naming the key, an unknown key by its full dotted
// name.
func TestAnswersRefusesEachMalformedShapeByName(t *testing.T) {
	for _, tc := range []struct{ name, block, want string }{
		{"not a mapping", "answers: on\n", "answers must be a mapping"},
		{"an unknown key", answersBlock(anthropicByFile, opusPrice, "  model: anthropic/claude-opus-5-5\n"), "unknown key answers.model"},
		{"no providers, a model priced", "answers:\n  prices:\n" + opusPrice, "answers.prices.anthropic/claude-opus-5-5 prices a model of anthropic, which answers.providers does not name, so the loop never calls it; remove it or configure the provider"},
		{"a provider written with no value", answersBlock("    anthropic: null\n", opusPrice, ""), "answers.providers names no provider with an endpoint: give anthropic or openai its base_url and its key, or remove the key to leave the loop off"},
		{"providers not a mapping", "answers:\n  providers: [anthropic]\n  prices:\n" + opusPrice, "answers.providers must be a mapping of provider (anthropic, openai) to its base_url and key"},
		{"an unknown provider", answersBlock(anthropicByFile+"    gemini: {base_url: https://gateway.internal.example/gemini, api_key_file: gemini}\n", opusPrice, ""), "unknown key answers.providers.gemini"},
		{"an unknown provider key", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key: sk-fixture}\n", opusPrice, ""), "unknown key answers.providers.anthropic.api_key"},
		{"both credentials", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key_file: anthropic-key, api_key_command: cat anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic requires exactly one of api_key_file or api_key_command"},
		{"no credential", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic}\n", opusPrice, ""), "answers.providers.anthropic requires exactly one of api_key_file or api_key_command"},
		{"a blank credential", answersBlock("    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key_command: \" \"}\n", opusPrice, ""), "answers.providers.anthropic.api_key_command must not be empty"},
		{"no base_url", answersBlock("    anthropic: {api_key_file: anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic.base_url is required: the provider's API endpoint, an http or https URL"},
		{"a relative base_url", answersBlock("    anthropic: {base_url: gateway/anthropic, api_key_file: anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic.base_url must be a valid URL"},
		{"a base_url of another scheme", answersBlock("    anthropic: {base_url: \"ftp://gateway.internal.example/anthropic\", api_key_file: anthropic-key}\n", opusPrice, ""), `answers.providers.anthropic.base_url must be http or https, not "ftp"`},
		{"a base_url with a query", answersBlock("    anthropic: {base_url: \"https://gateway.internal.example/anthropic?key=x\", api_key_file: anthropic-key}\n", opusPrice, ""), "answers.providers.anthropic.base_url must not include a query string or fragment"},
		{"no prices", "answers:\n  providers:\n" + anthropicByFile, "answers.prices is required: the USD price per million tokens of each model the loop may answer on, by the id its conversation names (<provider>/<model>)"},
		{"no price", "answers:\n  providers:\n" + anthropicByFile + "  prices: {}\n", "answers.prices must price at least one model"},
		{"a model id with no provider", answersBlock(anthropicByFile, "    claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), `answers.prices names "claude-opus-5-5", which is no model id: <provider>/<model>, its provider anthropic or openai`},
		{"a model of an unknown provider", answersBlock(anthropicByFile, "    gemini/gemini-3: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), `answers.prices names "gemini/gemini-3", which is no model id: <provider>/<model>, its provider anthropic or openai`},
		{"a model of a provider not configured", answersBlock(anthropicByFile, opusPrice+gptPrice, ""), "answers.prices.openai/gpt-6.1 prices a model of openai, which answers.providers does not name, so the loop never calls it; remove it or configure the provider"},
		{"a model priced twice", answersBlock(anthropicByFile, opusPrice+opusPrice, ""), "answers.prices names anthropic/claude-opus-5-5 twice"},
		{"a price field missing", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25}\n", ""), "answers.prices.anthropic/claude-opus-5-5.cache_read is required: USD per million tokens, 0 when the provider charges nothing for it"},
		{"a negative price", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: -5, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), "answers.prices.anthropic/claude-opus-5-5.input must be a non-negative number"},
		{"a price that is no number", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: five, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), "answers.prices.anthropic/claude-opus-5-5.input must be a number"},
		{"an infinite price", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: .inf, output: 25, cache_write: 6.25, cache_read: 0.5}\n", ""), "answers.prices.anthropic/claude-opus-5-5.input must be a number"},
		{"an unknown price key", answersBlock(anthropicByFile, "    anthropic/claude-opus-5-5: {input: 5, output: 25, cache_write: 6.25, cache_read: 0.5, cache_hit: 0.5}\n", ""), "unknown key answers.prices.anthropic/claude-opus-5-5.cache_hit"},
		{"an unknown bound", answersBlock(anthropicByFile, opusPrice, "  bounds: {retries: 2}\n"), "unknown key answers.bounds.retries"},
		{"a zero count", answersBlock(anthropicByFile, opusPrice, "  bounds: {model_calls: 0}\n"), "answers.bounds.model_calls must be a positive integer"},
		{"a decimal count", answersBlock(anthropicByFile, opusPrice, "  bounds: {gh_calls: 2.5}\n"), "answers.bounds.gh_calls must be an integer"},
		{"a duration past the timer bound", answersBlock(anthropicByFile, opusPrice, "  bounds: {answer_seconds: 2147484}\n"), "answers.bounds.answer_seconds must be at most 2147483"},
		{"a zero daily limit", answersBlock(anthropicByFile, opusPrice, "  bounds: {daily_usd: 0}\n"), "answers.bounds.daily_usd must be a positive number"},
		{"an unknown conversation_store key", answersBlock(anthropicByFile, opusPrice, "  conversation_store: {password: hunter2}\n"), "unknown key answers.conversation_store.password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.block), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
		})
	}
}

// Neither Load nor LoadForValidation (`legion config check`'s) runs api_key_command or reads
// api_key_file: a provider's key is read for each model call, never at load.
func TestLoadingAnswersReadsNoProviderKey(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	block := answersBlock(
		"    anthropic: {base_url: https://gateway.internal.example/anthropic, api_key_command: touch "+marker+"}\n"+
			"    openai: {base_url: https://gateway.internal.example/openai, api_key_file: absent-key}\n",
		opusPrice+gptPrice, "")
	for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
		if _, err := load(writeConfigFile(t, minimalFile+block), noEnv); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s ran api_key_command (stat %s: %v)", name, marker, err)
		}
	}
}

// A provider's api_key_file is read again on every call: a key rewritten between two calls is the
// second call's, trimmed. A missing or blank file is refused naming the key and the path, never
// what the file held.
func TestAnswerCredentialReadsTheFileAgainOnEveryCall(t *testing.T) {
	path := writeConfigFile(t, minimalFile+answersBlock(anthropicByFile, opusPrice, ""))
	cfg, err := LoadForValidation(path, noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	credential := cfg.Answers.Providers[AnswerAnthropic].Credential
	keyFile := filepath.Join(filepath.Dir(path), "keys", "anthropic")
	ctx := context.Background()

	if _, err := credential.Read(ctx); err == nil || !strings.Contains(err.Error(), "answers.providers.anthropic.api_key_file names "+keyFile) {
		t.Fatalf("Read with no file = %v, want a refusal naming the key and %s", err, keyFile)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ contents, want string }{
		{"sk-fixture-first\n", "sk-fixture-first"},
		{"  sk-fixture-second  \n", "sk-fixture-second"},
	} {
		if err := os.WriteFile(keyFile, []byte(tc.contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := credential.Read(ctx); err != nil || got != tc.want {
			t.Fatalf("Read = %q, %v; want %q, the file as it is now", got, err, tc.want)
		}
	}
	if err := os.WriteFile(keyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := credential.Read(ctx); err == nil || err.Error() != "answers.providers.anthropic.api_key_file names "+keyFile+", which is empty" {
		t.Fatalf("Read of a blank file = %q, %v; want it refused as empty", got, err)
	}
}

// A provider's api_key_command runs again on every call, its output trimmed. A command that prints
// nothing is refused, as is one that fails, naming its exit and stderr and never what it printed;
// and a call whose context ends ends the command.
func TestAnswerCredentialRunsTheCommandAgainOnEveryCall(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	credential := AnswerCredential{
		Command: `n=$(cat ` + count + ` 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > ` + count + `; printf ' sk-fixture-%s \n' "$n"`,
		key:     "answers.providers.openai",
	}
	ctx := context.Background()
	for _, want := range []string{"sk-fixture-1", "sk-fixture-2"} {
		if got, err := credential.Read(ctx); err != nil || got != want {
			t.Fatalf("Read = %q, %v; want %q, the command run again", got, err, want)
		}
	}

	for _, tc := range []struct{ name, command, want string }{
		{"no output", "printf '  \\n'", "answers.providers.openai.api_key_command produced empty output"},
		{"a failure", "echo sk-fixture-leaked; echo token endpoint refused >&2; exit 3", "answers.providers.openai.api_key_command failed (exit 3): token endpoint refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AnswerCredential{Command: tc.command, key: "answers.providers.openai"}.Read(ctx)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Read = %q, %v; want the refusal %q", got, err, tc.want)
			}
		})
	}

	ended, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := (AnswerCredential{Command: "sleep 30", key: "answers.providers.openai"}).Read(ended); err == nil || !strings.Contains(err.Error(), "answers.providers.openai.api_key_command did not finish") {
		t.Fatalf("Read past its context = %v, want it ended", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Read past its context took %s, want the command killed", elapsed)
	}
}
