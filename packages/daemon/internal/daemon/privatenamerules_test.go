package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// privateNameRulesNamers are the two settings that name the deployment's rule file, as a refusal
// of the file names them (config.Config.PrivateNameRulesNamedBy).
var privateNameRulesNamers = []string{"private_name_rules_file", leakguard.RulesFileVar}

// namedRulesPrefix is how the boot's line for a deployment that names a rule file starts, whatever
// file it names.
const namedRulesPrefix = "daemon: the deployment's private-name rules are "

// namedRulesLine is the boot's line for a deployment that names the rule file at path holding
// content, worked out here from the content itself: its sha256 and its count of rules.
func namedRulesLine(path, content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf(namedRulesPrefix+"%s (sha256 %s, %d rules), handed to every launch",
		path, hex.EncodeToString(sum[:]), strings.Count(content, "[[rules]]"))
}

// rulesLines reads log, one JSON object a line, and answers how many lines say the deployment
// names no rule file, how many say it names one, whichever, and how many say it names exactly
// named (namedRulesLine).
func rulesLines(t *testing.T, log, named string) (none, anyNamed, exactly int) {
	t.Helper()
	for line := range strings.Lines(log) {
		var entry struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("a log line that is no JSON: %q: %v", line, err)
		}
		switch {
		case entry.Msg == privateNameRulesNone:
			none++
		case strings.HasPrefix(entry.Msg, namedRulesPrefix):
			anyNamed++
			if entry.Msg == named {
				exactly++
			}
		}
	}
	return none, anyNamed, exactly
}

// A rule file the guard could not run refuses the boot before the store is touched, and `legion
// config check` (CheckStart) refuses it in the same words: a file that is not there, one a byte
// past leakguard.MaxRulesFile, one that is no TOML, one that extends another, and one that holds
// no rule, also at that bound, which boot reads whole and so refuses for what it holds, not its
// length. Each refusal names the setting that named the file, the key or the variable, and the
// file, and the boot logs neither of its lines on the rules: the refusal is the boot's word on
// them.
func TestBootRefusesAPrivateNameRuleFileItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		// content is the file's, and "" writes no file at all.
		content string
		want    string
	}{
		{name: "a missing file", want: "{named} names {file}, which could not be opened: no such file or directory"},
		{name: "a file one byte past the bound", content: leakguardtest.PaddedTo(leakguardtest.Rules, leakguard.MaxRulesFile+1),
			want: "{named} names {file}, which is longer than 262144 bytes"},
		{name: "a file that is no TOML", content: "title = = [\n",
			want: "{named} names {file}, which is no private-name rule file the guard can run: the rules are not TOML"},
		{name: "an [extend] section", content: leakguardtest.Rules + "\n[extend]\nuseDefault = true\n",
			want: "{named} names {file}, which is no private-name rule file the guard can run: the rules hold an [extend] section"},
		{name: "no rule", content: "title = \"no rules at all\"\n",
			want: "{named} names {file}, which is no private-name rule file the guard can run: the rules hold no rule"},
		{name: "no rule in a file at the bound", content: leakguardtest.PaddedTo("title = \"no rules at all\"\n", leakguard.MaxRulesFile),
			want: "{named} names {file}, which is no private-name rule file the guard can run: the rules hold no rule"},
	} {
		for _, named := range privateNameRulesNamers {
			t.Run(tc.name+" named by "+named, func(t *testing.T) {
				cfg := testConfig(t)
				cfg.PrivateNameRulesFile = filepath.Join(t.TempDir(), "private-names.toml")
				if tc.content != "" {
					if err := os.WriteFile(cfg.PrivateNameRulesFile, []byte(tc.content), 0o600); err != nil {
						t.Fatalf("write the rule file: %v", err)
					}
				}
				cfg.PrivateNameRulesNamedBy = named
				want := strings.NewReplacer("{named}", named, "{file}", cfg.PrivateNameRulesFile).Replace(tc.want)
				var logged testlog.Buffer

				err := run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logged, nil)), fakeRuntime(fake.NewRuntime(), &built{}))

				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("run = %v, want a refusal starting %q", err, want)
				}
				if count, _ := boots(t, cfg); count != 0 {
					t.Fatalf("boots after a refused start = %d, want 0", count)
				}
				if none, anyNamed, _ := rulesLines(t, logged.String(), ""); none != 0 || anyNamed != 0 {
					t.Errorf("the refused boot logged the no-file line %d times and a file's line %d times, want neither; log:\n%s", none, anyNamed, logged.String())
				}
				if _, _, _, checked := CheckStart(cfg, lookup(nil)); checked == nil || checked.Error() != err.Error() {
					t.Errorf("CheckStart = %v, want the boot's own refusal %q", checked, err)
				}
			})
		}
	}
}

// The deployment's rule file reaches every launch, a root's and a worker's alike, as the launch
// secret LEGION_PRIVATE_NAME_RULES, which each runtime hands the agent behind its
// LEGION_PRIVATE_NAME_RULES_FILE pointer: its bytes exactly as the file holds them, whichever
// setting named it. A deployment that names none hands no such secret.
func TestThePrivateNameRulesAreALaunchSecretWhereTheDeploymentNamesAFile(t *testing.T) {
	for _, tc := range []struct {
		name, named string
	}{
		{name: "named by the key", named: "private_name_rules_file"},
		{name: "named by the variable", named: leakguard.RulesFileVar},
		{name: "none named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			want := ""
			if tc.named != "" {
				cfg.PrivateNameRulesFile, cfg.PrivateNameRulesNamedBy = leakguardtest.WriteRules(t, t.TempDir()), tc.named
				want = leakguardtest.Rules
			}
			rt := fake.NewRuntime()
			d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

			root := lastLaunch(t, rt, d.spawn(architect()))
			worker := lastLaunch(t, rt, d.spawn(api.SpawnRequest{Tree: "LEGION-1", Issue: "LEGION-2", Role: claim.RoleImplementer, Prompt: "Wait."}))
			for kind, spec := range map[string]runtime.SpawnSpec{"root": root, "worker": worker} {
				got, carried := spec.Secrets[leakguard.RulesSecret]
				if carried != (want != "") || got != want {
					t.Errorf("the %s launch's %s secret is %q (carried: %t), want %q", kind, leakguard.RulesSecret, got, carried, want)
				}
			}
		})
	}
}

// The boot says once what private-name rules the deployment hands its roles: where it names no
// rule file, privateNameRulesNone, logged before anything that can stop the boot, so a boot that
// then fails on its port has logged it all the same; where it names one, the file, the sha256 of
// its bytes and its number of rules, once boot has compiled it. Neither boot logs the other's line.
func TestRunLogsThePrivateNameRulesLineAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named bool
	}{
		{name: "no rule file"},
		{name: "the deployment's rule file", named: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			if tc.named {
				cfg.PrivateNameRulesFile, cfg.PrivateNameRulesNamedBy = leakguardtest.WriteRules(t, t.TempDir()), "private_name_rules_file"
			}
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.listen = nil
			var logged testlog.Buffer

			if err := run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logged, nil)), o); err == nil {
				t.Fatal("run returned no error although another listener held its port")
			}

			want := namedRulesLine(cfg.PrivateNameRulesFile, leakguardtest.Rules)
			none, anyNamed, exactly := rulesLines(t, logged.String(), want)
			if tc.named && (exactly != 1 || anyNamed != 1 || none != 0) {
				t.Fatalf("the boot logged %q %d times, a file's line %d times and the no-file line %d times, want 1, 1 and 0; log:\n%s",
					want, exactly, anyNamed, none, logged.String())
			}
			if !tc.named && (none != 1 || anyNamed != 0) {
				t.Fatalf("the boot logged the no-file line %d times and a file's line %d times, want 1 and 0; log:\n%s", none, anyNamed, logged.String())
			}
		})
	}
}
