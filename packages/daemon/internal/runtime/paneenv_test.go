package runtime_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A pane variable that would move the session a resume reads, override what the runtime sets on a
// Claude Code pane or a resumed Oh My Pi pane, take a launch secret's pointer, or put a password on
// tmux's command line is refused with its reason, which never holds the value; any other variable
// is the operator's.
func TestPaneVariableRefusalNamesWhyAndNeverTheValue(t *testing.T) {
	const (
		password   = "hunter2-pane"
		session    = "it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"
		claude     = "the runtime sets it on every Claude Code pane itself, after the operator's variables"
		resumed    = "the runtime sets it on every resumed Oh My Pi pane itself, after the operator's variables"
		withSecret = "its value is a URL carrying a password: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line"
	)
	for _, tc := range []struct {
		name, value, want string
	}{
		{"OMP_SESSION_STORAGE", "sql", session},
		{"OMP_SESSION_SQL_DSN_FILE", "/etc/acme/dsn", session},
		{"OMP_SESSION_SQL_DSN", "/etc/acme/dsn", session},
		{"PI_CODING_AGENT_DIR", "/srv/agent", session},
		{"PI_CONFIG_DIR", ".elsewhere", session},
		{"OMP_CONFIG_ROOT", "/srv/omp", session},
		{"CLAUDE_CONFIG_DIR", "/srv/claude", claude},
		{"CLAUDE_CODE_DISABLE_CRON", "0", claude},
		// A resumed Oh My Pi pane is held to the session it resumes; the operator may not turn that
		// off for every pane, nor set it on the fresh ones.
		{"OMP_SESSION_EXCLUSIVE", "0", resumed},
		{"OMP_SESSION_EXCLUSIVE", "1", resumed},
		// Claude Code's background tasks are the operator's to set, as its other variables are: the
		// runtime sets nothing that turns them off, so no value of the operator's is overridden.
		{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "0", ""},
		{"ENVOY_TOKEN_FILE", "/etc/acme/envoy", "it points at the launch secret ENVOY_TOKEN, which the runtime hands every launch as a file of the launch's own"},
		{"LEGION_PRIVATE_NAME_RULES_FILE", "/etc/acme/private-names.toml", "it points at the launch secret LEGION_PRIVATE_NAME_RULES, which the runtime hands every launch as a file of the launch's own"},
		{"NATS_NKEY_SEED_FILE", "/etc/acme/seed", "it points at the launch secret NATS_NKEY_SEED, which the runtime hands every launch as a file of the launch's own"},
		{"DATABASE_URL", "postgres://acme:" + password + "@db.internal.example/widgets", withSecret},
		{"WIDGETS_NATS", "nats://h1.internal.example:4222,nats://acme:" + password + "@h2.internal.example:4222", withSecret},
		{"WIDGETS_ONLY_PASSWORD", "redis://:" + password + "@cache.internal.example:6379", withSecret},
		{"ELAN_HOME", "/opt/elan", ""},
		{"DISPATCH_PUBLIC_URL", "https://dispatch.example:8443/acme@widgets", ""},
		{"WIDGETS_MIRROR", "https://acme@mirror.internal.example/widgets", ""},
		{"WIDGETS_NATS_PLAIN", "nats://h1.internal.example:4222", ""},
		{"WIDGETS_QUERY", "https://widgets.example/search?q=a:b@c", ""},
		{"LEDGER_DSN_FILE", "/etc/legion-ledger/dsn", ""},
	} {
		got := runtime.PaneVariableRefusal(tc.name, tc.value)
		if got != tc.want {
			t.Errorf("PaneVariableRefusal(%s) = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, password) {
			t.Errorf("PaneVariableRefusal(%s) = %q holds the value's password", tc.name, got)
		}
	}
}

// The names that place a session are the ones the Sandbox runtime refuses in an operator's pod for
// the same reason, read from the constants it builds its refusal from (ompsessions' session
// variables, the pod baseline's reserved names, and ompdirs' config root variable). ompsessions
// imports package runtime, so runtime spells its names out.
func TestPaneVariableRefusalCoversTheSandboxRuntimesSessionNames(t *testing.T) {
	names := []string{ompsessions.StorageVariable, ompsessions.DSNFileVariable, ompsessions.DSNFileName, ompdirs.ConfigRootVariable}
	for _, v := range podsafety.Variables() {
		if v.Reserved != "" {
			names = append(names, v.Name)
		}
	}
	for _, name := range names {
		if runtime.PaneVariableRefusal(name, "x") == "" {
			t.Errorf("PaneVariableRefusal(%s) refuses nothing; the Sandbox runtime refuses it as placing the session a resume reads", name)
		}
	}
}
