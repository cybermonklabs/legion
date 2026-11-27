package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// stubSecrets is a `secrets` stand-in: it answers `get <NAME> --no-request` with a status and
// `get <NAME> --value` with a value for the names below, and records how it was run — its
// arguments, its stdin, and the two secretsd variables the daemon treats differently.
const stubSecrets = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
readlink "/proc/$$/fd/0" > "$STUB_DIR/stdin"
if [ -n "${SECRETSD_SESSION_TOKEN_FILE+set}" ]; then echo set; else echo unset; fi > "$STUB_DIR/session"
echo "${SECRETSD_SOCK-}" > "$STUB_DIR/sock"
case "$2:$3" in
  AGENT_KEY:--no-request) echo '{"key":"AGENT_KEY","tier":"agent"}' ;;
  AGENT_KEY:--value) printf 'agent-value\n' ;;
  HUMAN_KEY:--no-request) echo '{"key":"HUMAN_KEY","tier":"human"}' ;;
  HUMAN_KEY:--value) printf 'human-value\n' ;;
  MISSING_KEY:*) echo "AGENT NOTICE: secret 'MISSING_KEY' not found" >&2; exit 3 ;;
  GARBLED_KEY:--no-request) echo 'not json' ;;
  OTHER_KEY:--no-request) echo '{"key":"SOMETHING_ELSE","tier":"agent"}' ;;
  ODD_TIER_KEY:--no-request) echo '{"key":"ODD_TIER_KEY","tier":"admin"}' ;;
  EMPTY_KEY:--no-request) echo '{"key":"EMPTY_KEY","tier":"agent"}' ;;
  EMPTY_KEY:--value) printf '\n' ;;
  FAILING_VALUE_KEY:--no-request) echo '{"key":"FAILING_VALUE_KEY","tier":"human"}' ;;
  FAILING_VALUE_KEY:--value) echo 'AGENT NOTICE: TIMEOUT' >&2; exit 4 ;;
  KILLED_VALUE_KEY:--no-request) echo '{"key":"KILLED_VALUE_KEY","tier":"human"}' ;;
  KILLED_VALUE_KEY:--value) echo 'AGENT NOTICE: waiting for a grant' >&2; kill -KILL $$ ;;
  FOLDED_NAME_KEY_0123:--no-request) echo "secretsd: no key $(printf %s "$2" | tr A-Z a-z)" >&2; exit 5 ;;
  *) echo "stub: unexpected $*" >&2; exit 99 ;;
esac
`

// secretsStub installs the stub as `secrets` in a directory of its own and returns the daemon
// environment that finds it, carrying a session token file and a broker socket override.
func secretsStub(t *testing.T) (environ []string, stubDir string) {
	t.Helper()
	stubDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "secrets"), []byte(stubSecrets), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"STUB_DIR=" + stubDir,
		"HOME=" + t.TempDir(),
		"SECRETSD_SESSION_TOKEN_FILE=/run/user/1000/secretsd/session.token",
		"SECRETSD_SOCK=/run/user/1000/secretsd.sock",
	}, stubDir
}

func readStub(t *testing.T, stubDir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(stubDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

// Boot resolves each provider key the way the shipped daemon resolves an App key it holds in
// secretsd (config.ts runSecretsGet and resolvePrivateKeySecret): the tier first, never costing a
// tap; then the value, under the daemon's own environment less its session token, with the daemon's
// stdin — secretsd scopes a tokenless caller by its terminal. Each value lands in its own 0600 file
// in a 0700 directory, named for the variable OMP reads — not for the secretsd key, which a
// deployment names per environment. A human-tier key is announced first, and nothing logged carries
// a value.
func TestMaterializeProviderKeysWritesEachValueAsADaemonHeldFile(t *testing.T) {
	environ, stubDir := secretsStub(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = previous })
	logged := &strings.Builder{}
	log := slog.New(slog.NewTextHandler(logged, nil))

	dir, err := MaterializeProviderKeys([]ProviderKey{
		{Env: "AGENT_ENV", Secret: "AGENT_KEY"},
		{Env: "HUMAN_ENV", Secret: "HUMAN_KEY"},
	}, stateDir, environ, log)
	if err != nil {
		t.Fatalf("MaterializeProviderKeys: %v", err)
	}

	if want := filepath.Join(stateDir, "secrets", "provider-env"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Errorf("provider-env directory: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("provider-env directory mode %v, want 0700", info.Mode().Perm())
	}
	for name, want := range map[string]string{"AGENT_ENV": "agent-value", "HUMAN_ENV": "human-value"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", path, info.Mode().Perm())
		}
		if got, _ := os.ReadFile(path); string(got) != want {
			t.Errorf("%s holds the wrong bytes", name)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("provider-env holds %d entries, want exactly the two variables (no temporary files, nothing named for a secretsd key)", len(entries))
	}

	if calls := readStub(t, stubDir, "calls"); calls != "get AGENT_KEY --no-request\nget AGENT_KEY --value\nget HUMAN_KEY --no-request\nget HUMAN_KEY --value" {
		t.Errorf("secrets was run as:\n%s", calls)
	}
	if got := readStub(t, stubDir, "session"); got != "unset" {
		t.Errorf("secrets ran with SECRETSD_SESSION_TOKEN_FILE %s; the daemon's request must not land on a session it was started from", got)
	}
	if got := readStub(t, stubDir, "sock"); got != "/run/user/1000/secretsd.sock" {
		t.Errorf("secrets ran with SECRETSD_SOCK %q, want the daemon's own", got)
	}
	if got := readStub(t, stubDir, "stdin"); got != stdin.Name() {
		t.Errorf("secrets ran with stdin %q, want the daemon's own %q", got, stdin.Name())
	}
	if !strings.Contains(logged.String(), "requesting the secretsd key provider_keys.HUMAN_ENV names (human tier; a YubiKey tap may be needed)") {
		t.Errorf("no human-tier announcement for HUMAN_ENV in:\n%s", logged)
	}
	if strings.Contains(logged.String(), "provider_keys.AGENT_ENV") {
		t.Errorf("an agent-tier key was announced as needing a tap:\n%s", logged)
	}
	if strings.Contains(logged.String(), "HUMAN_KEY") {
		t.Errorf("a secretsd key's name reached the log:\n%s", logged)
	}
	if strings.Contains(logged.String(), "-value") {
		t.Errorf("a value reached the log:\n%s", logged)
	}
}

// The directory holds exactly the configured keys: one dropped from the file is removed at the
// next boot, and no keys at all is no directory — a pane is never handed a key nobody configured.
func TestMaterializeProviderKeysKeepsExactlyTheConfiguredKeys(t *testing.T) {
	environ, _ := secretsStub(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	agent := ProviderKey{Env: "AGENT_ENV", Secret: "AGENT_KEY"}
	dir, err := MaterializeProviderKeys([]ProviderKey{agent, {Env: "HUMAN_ENV", Secret: "HUMAN_KEY"}}, stateDir, environ, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeProviderKeys([]ProviderKey{agent}, stateDir, environ, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "HUMAN_ENV")); !os.IsNotExist(err) {
		t.Errorf("a key dropped from provider_keys is still on disk (stat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENT_ENV")); err != nil {
		t.Errorf("a configured key is gone: %v", err)
	}

	none, err := MaterializeProviderKeys(nil, stateDir, environ, log)
	if err != nil || none != "" {
		t.Fatalf("no provider keys = (%q, %v), want no directory", none, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the provider-env directory outlived its last key (stat: %v)", err)
	}
}

// Every way a key cannot be resolved is a refusal naming the entry by its variable alone, never its
// secretsd key's name or its value, and a boot that refuses writes nothing: the files are written
// only once every key has resolved. The name is withheld because a provider's key made only of
// letters, digits and underscores passes the name rule, so the name can be the key itself, pasted
// where its name goes; the last row pastes such a made-up key, which the stub, like secretsd, does
// not know and quotes in its refusal.
func TestMaterializeProviderKeysRefuses(t *testing.T) {
	const pasted = "MadeUpProviderKey0123456789abcdefXYZ"
	for _, tc := range []struct {
		name, env, key string
		path           bool
		want           string
	}{
		{"the status fails", "ENV_MISSING_KEY", "MISSING_KEY", true, "provider_keys.ENV_MISSING_KEY: secrets get [the key name] --no-request failed (exit 3): AGENT NOTICE: secret '[the key name]' not found"},
		{"the status is not a status", "ENV_GARBLED_KEY", "GARBLED_KEY", true, `provider_keys.ENV_GARBLED_KEY: secrets get [the key name] --no-request printed an unparsable status (expected {"key","tier"})`},
		{"the status is another key's", "ENV_OTHER_KEY", "OTHER_KEY", true, `provider_keys.ENV_OTHER_KEY: secrets get [the key name] --no-request printed an unparsable status (expected {"key","tier"})`},
		{"the tier is neither", "ENV_ODD_TIER_KEY", "ODD_TIER_KEY", true, `provider_keys.ENV_ODD_TIER_KEY: secrets get [the key name] --no-request reported tier "admin", which is neither agent nor human`},
		{"the value is empty", "ENV_EMPTY_KEY", "EMPTY_KEY", true, "provider_keys.ENV_EMPTY_KEY: secrets get [the key name] --value printed no value"},
		{"the value fails", "ENV_FAILING_VALUE_KEY", "FAILING_VALUE_KEY", true, "provider_keys.ENV_FAILING_VALUE_KEY: secrets get [the key name] --value failed (exit 4): AGENT NOTICE: TIMEOUT"},
		// A run a signal ended has no status: read as one, it was `failed (exit -1)`.
		{"the value's run is killed", "ENV_KILLED_VALUE_KEY", "KILLED_VALUE_KEY", true, "provider_keys.ENV_KILLED_VALUE_KEY: secrets get [the key name] --value failed (signal: killed): AGENT NOTICE: waiting for a grant"},
		{"no secrets command", "ENV_AGENT_KEY", "AGENT_KEY", false, "provider_keys.AGENT_ENV: the secrets command is not on PATH, so the secretsd key it names cannot be read"},
		// The pasted key's variable is a name of its own, so that only its secretsd key's name holds
		// the key.
		{"a provider's key pasted where its name goes", "GEMINI_API_KEY", pasted, true, "provider_keys.GEMINI_API_KEY: secrets get [the key name] --no-request failed (exit 99): stub: unexpected get [the key name] --no-request"},
		// A stderr that quotes the name other than exactly, here lower-cased, still holds a
		// 10-character run of it, so it is dropped and only its length printed.
		{"a stderr quoting the key name case-folded", "ENV_FOLDED_NAME_KEY", "FOLDED_NAME_KEY_0123", true, "provider_keys.ENV_FOLDED_NAME_KEY: secrets get [the key name] --no-request failed (exit 5): its stderr (37 bytes) holds part of the key name, so it is not printed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			environ, _ := secretsStub(t)
			if !tc.path {
				environ[0] = "PATH=" + t.TempDir()
			}
			stateDir := filepath.Join(t.TempDir(), "state")
			log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
			_, err := MaterializeProviderKeys([]ProviderKey{
				{Env: "AGENT_ENV", Secret: "AGENT_KEY"},
				{Env: tc.env, Secret: tc.key},
			}, stateDir, environ, log)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), pasted) {
				t.Errorf("the refusal quotes the key pasted where its name goes: %v", err)
			}
			if strings.Contains(err.Error(), "-value\n") || strings.Contains(err.Error(), "agent-value") {
				t.Errorf("the refusal carries a value: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(stateDir, "secrets", "provider-env", "AGENT_ENV")); !os.IsNotExist(statErr) {
				t.Errorf("a refused boot still wrote AGENT_ENV (stat: %v)", statErr)
			}
		})
	}
}

func TestProviderKeysMayNotNameAnAgentSecretsVariable(t *testing.T) {
	for _, name := range []string{"AGENT_SECRETS_URL", "AGENT_SECRETS_KEY_DIR", "AGENT_SECRETS_LAUNCHER_TOKEN"} {
		_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"provider_keys:\n  "+name+": x\n"), noEnv)
		if err == nil || !strings.Contains(err.Error(), "provider_keys names "+name) || !strings.Contains(err.Error(), "AGENT_SECRETS_") {
			t.Fatalf("%s: err %v, want the agent-secrets refusal", name, err)
		}
	}
}

// A provider key named for a gh variable the daemon sets on every pane and pod is refused by the
// loader, naming the key and that every pane sets it, so `legion config check` refuses the
// file under either runtime rather than the deployment stopping on every pane's shim; a key of
// another name passes.
func TestProviderKeysMayNotNameAGhVariableEveryPaneSets(t *testing.T) {
	for _, name := range []string{"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "LEGION_GH_OWNERS_DIR", "LEGION_GH_PATH"} {
		t.Run(name, func(t *testing.T) {
			want := "provider_keys names " + name + ", which the daemon sets on every pane and pod (the role's GitHub App tokens are the gh files under LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR its issue's owner's and LEGION_GH_PATH the real gh `legion gh` runs; GH_TOKEN, GITHUB_TOKEN and GH_HOST are emptied so nothing outranks them), so a provider key may not use that name"
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"provider_keys:\n  "+name+": "+name+"_TESTS\n"), noEnv)
			if err == nil || err.Error() != want {
				t.Fatalf("LoadForValidation = %v, want %q", err, want)
			}
		})
	}
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"provider_keys:\n  GEMINI_API_KEY: GEMINI_API_KEY_TESTS\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation with a provider key of another name: %v", err)
	}
	if want := []ProviderKey{{Env: "GEMINI_API_KEY", Secret: "GEMINI_API_KEY_TESTS"}}; !reflect.DeepEqual(cfg.ProviderKeys, want) {
		t.Fatalf("ProviderKeys = %+v, want %+v", cfg.ProviderKeys, want)
	}
}

// A provider key under tmux named for a variable that runtime.PaneVariableRefusal refuses with no
// value, which the tmux runtime also refuses at boot (tmux's readProviderKeyNames), as
// LEGION_ROLE_IDENTITIES, a variable a resumed Oh My Pi pane is told after the operator's, and a
// launch secret's pointer, is refused by the loader with that function's reason, a secretsd key
// and a file alike, so `legion config check` refuses the file rather than the daemon at boot.
func TestATmuxProviderKeyIsRefusedForAVariablePaneEnvMayNotSet(t *testing.T) {
	for _, tc := range []struct{ name, how, value string }{
		{runtime.RoleIdentitiesVariable, "a secretsd key", runtime.RoleIdentitiesVariable + "_TESTS"},
		{runtime.RoleIdentitiesVariable, "a file", "file:/etc/legion/providers/" + runtime.RoleIdentitiesVariable},
		{runtime.SessionExclusiveVariable, "a secretsd key", runtime.SessionExclusiveVariable + "_TESTS"},
		{"ENVOY_TOKEN_FILE", "a secretsd key", "ENVOY_TOKEN_FILE_TESTS"},
	} {
		t.Run(tc.name+" as "+tc.how, func(t *testing.T) {
			why := runtime.PaneVariableRefusal(tc.name, "")
			if why == "" {
				t.Fatalf("runtime.PaneVariableRefusal(%s, \"\") refuses nothing, so this row tests nothing", tc.name)
			}
			want := "provider_keys names " + tc.name + ": " + why
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"provider_keys:\n  "+tc.name+": "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != want {
				t.Fatalf("LoadForValidation = %v, want %q", err, want)
			}
		})
	}
}

// Under tmux a provider key may name a file (`file:/abs/path`) in place of a secretsd key, for a
// host with no secretsd: the loader keeps the file's cleaned path beside a secretsd entry, in file
// order, and the variable names stay refused as for any other entry.
func TestAProviderKeyMayNameAFileUnderTmux(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"provider_keys:\n  ANTHROPIC_API_KEY: file:/etc/legion/providers//ANTHROPIC_API_KEY\n  GEMINI_API_KEY: GEMINI_API_KEY_TESTS\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation with a file provider key: %v", err)
	}
	want := []ProviderKey{{Env: "ANTHROPIC_API_KEY", File: "/etc/legion/providers/ANTHROPIC_API_KEY"}, {Env: "GEMINI_API_KEY", Secret: "GEMINI_API_KEY_TESTS"}}
	if !reflect.DeepEqual(cfg.ProviderKeys, want) {
		t.Fatalf("ProviderKeys = %+v, want %+v", cfg.ProviderKeys, want)
	}
	_, err = LoadForValidation(writeConfigFile(t, minimalFile+"provider_keys:\n  GH_TOKEN: file:/etc/legion/providers/GH_TOKEN\n"), noEnv)
	if err == nil || !strings.HasPrefix(err.Error(), "provider_keys names GH_TOKEN, which the daemon sets on every pane and pod") {
		t.Fatalf("LoadForValidation of a file provider key named GH_TOKEN = %v, want the gh variable refused", err)
	}
}

// A file provider key is read at boot as the daemon, with no `secrets` run: its contents, trimmed,
// land in the variable's 0600 file beside the secretsd keys' values.
func TestMaterializeProviderKeysReadsAFileKey(t *testing.T) {
	environ, stubDir := secretsStub(t)
	keyFile := filepath.Join(t.TempDir(), "anthropic")
	if err := os.WriteFile(keyFile, []byte("  file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	dir, err := MaterializeProviderKeys([]ProviderKey{{Env: "ANTHROPIC_API_KEY", File: keyFile}, {Env: "AGENT_ENV", Secret: "AGENT_KEY"}}, stateDir, environ, log)
	if err != nil {
		t.Fatalf("MaterializeProviderKeys: %v", err)
	}
	for name, want := range map[string]string{"ANTHROPIC_API_KEY": "file-value", "AGENT_ENV": "agent-value"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, want 0600", path, info.Mode().Perm())
		}
		if got, _ := os.ReadFile(path); string(got) != want {
			t.Errorf("%s holds the wrong bytes", name)
		}
	}
	if calls := readStub(t, stubDir, "calls"); calls != "get AGENT_KEY --no-request\nget AGENT_KEY --value" {
		t.Errorf("secrets was run as:\n%s\nwant only the secretsd key's two reads", calls)
	}
}

// A file provider key is read by ReadPrivateSecretPointer's rules: a file that cannot be read, holds
// nothing, is not a regular file, or that its group or others can read is a refusal naming the
// variable, and the file where it was opened, never its contents, and the boot writes no key at
// all.
func TestMaterializeProviderKeysRefusesAFileKeyItCannotUse(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, "shared")
	if err := os.WriteFile(shared, []byte("file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	for _, tc := range []struct{ name, file, want string }{
		{"a missing file", missing, "provider_keys.ANTHROPIC_API_KEY names a file that could not be read: it does not exist"},
		{"an empty file", empty, "provider_keys.ANTHROPIC_API_KEY names " + empty + ", which is empty"},
		{"a file others can read", shared, "provider_keys.ANTHROPIC_API_KEY " + shared + " is readable by its group or others (mode 0644); chmod 0600 it"},
		{"a directory", dir, "provider_keys.ANTHROPIC_API_KEY names " + dir + ", which is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			environ, _ := secretsStub(t)
			stateDir := filepath.Join(t.TempDir(), "state")
			log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
			_, err := MaterializeProviderKeys([]ProviderKey{{Env: "AGENT_ENV", Secret: "AGENT_KEY"}, {Env: "ANTHROPIC_API_KEY", File: tc.file}}, stateDir, environ, log)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(stateDir, "secrets", "provider-env", "AGENT_ENV")); !os.IsNotExist(statErr) {
				t.Errorf("a refused boot still wrote AGENT_ENV (stat: %v)", statErr)
			}
		})
	}
}
