package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// conversationsDSN is the conversation store's URL a test's answers.conversation_store.dsn_file
// holds; its password opens nothing.
const conversationsDSN = "postgres://legion-daemon:fixture-conversations-password@conversations.internal.example:5432/omp_sessions"

// answersConfig is an `answers` block whose one provider's key command, were it ever run, would
// leave marker behind, and whose conversation store's URL is in dsnFile ("" for none).
func answersConfig(marker, dsnFile string) *config.Answers {
	return &config.Answers{
		Providers: map[config.AnswerProvider]config.AnswerEndpoint{
			config.AnswerOpenAI: {BaseURL: "https://gateway.internal.example/openai", Credential: config.AnswerCredential{Command: "touch " + marker}},
		},
		Prices:            map[string]config.AnswerPrice{"openai/gpt-6.1": {Input: 1.25, Output: 10}},
		ConversationStore: config.ConversationStore{DSNFile: dsnFile},
	}
}

// dsnFileOf writes contents to a conversation store URL file of its own and answers its path.
func dsnFileOf(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conversations-dsn")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A Kubernetes daemon runs no host gh of its own, so without `answers` it boots on a host with
// none; with `answers` it resolves the real gh the answer loop's gh tool runs at boot, as a tmux
// daemon resolves its own, and refuses to start, naming gh and its override, when there is none.
// The gh it resolves, LEGION_GH_PATH's here, the shared public leak check's pin and the
// conversation store's URL are boot's reads, and neither boot nor `legion config check` runs the
// provider's key command.
func TestAKubernetesDaemonWithAnswersRefusesToStartWithoutGH(t *testing.T) {
	t.Parallel()
	noGH := []string{"PATH=" + t.TempDir()}
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.LeakCheck = leakCheckSetting
	if _, err := prepare(cfg, quietLogger(), overrides{environ: noGH}); err != nil {
		t.Fatalf("prepare with no answers and no gh = %v, want the boot to go on", err)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	cfg.Answers = answersConfig(marker, dsnFileOf(t, conversationsDSN+"\n"))
	_, err := prepare(cfg, quietLogger(), overrides{environ: noGH})
	const want = "answers needs gh on the daemon's host, which the answer loop's gh tool runs, and none was found: set LEGION_GH_PATH to an absolute executable path, or put gh on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("prepare with answers and no gh = %v, want %q", err, want)
	}

	gh := binary(t, t.TempDir(), "gh")
	withGH := environLookup(append(noGH, "LEGION_GH_PATH="+gh, leakguard.TagVariable+"="+leakCheckTag))
	reads, err := readBoot(cfg, withGH, os.Getenv, true, quietLogger())
	if err != nil {
		t.Fatalf("readBoot with answers and LEGION_GH_PATH: %v", err)
	}
	if reads.answers.gh != gh || reads.answers.conversationDSN != conversationsDSN {
		t.Errorf("boot's answer reads = gh %q, dsn %q; want %q and the file's URL", reads.answers.gh, reads.answers.conversationDSN, gh)
	}
	if _, _, err := CheckStart(cfg, withGH); err != nil {
		t.Fatalf("CheckStart with answers and LEGION_GH_PATH: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("boot or the check ran api_key_command (stat %s: %v)", marker, err)
	}
}

// A tmux daemon with `answers` and no repository, whose own tools boot does not resolve, still
// resolves gh for the loop, the first gh on PATH, and refuses to start without one; with a
// repository the loop's gh is the one boot resolved for the daemon's own tools. The machine's
// shared public leak check is on the same PATH.
func TestATmuxDaemonWithAnswersResolvesGHWithOrWithoutARepository(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	cfg := testConfig(t)
	cfg.Answers = answersConfig(marker, "")
	empty := t.TempDir()
	if _, err := readBoot(cfg, environLookup([]string{"PATH=" + empty}), os.Getenv, false, quietLogger()); err == nil || !strings.HasPrefix(err.Error(), "answers needs gh on the daemon's host") {
		t.Fatalf("readBoot with answers and no gh = %v, want the refusal naming gh", err)
	}
	onPath := t.TempDir()
	gh := binary(t, onPath, "gh")
	leakguardtest.Write(t, onPath, leakguardtest.Passes)
	reads, err := readBoot(cfg, environLookup([]string{"PATH=" + onPath}), os.Getenv, false, quietLogger())
	if err != nil {
		t.Fatalf("readBoot with answers and gh on PATH: %v", err)
	}
	if reads.answers.gh != gh || reads.tmux.tools != nil {
		t.Errorf("boot's answer gh = %q and tools %v, want %q and no daemon tools without a repository", reads.answers.gh, reads.tmux.tools, gh)
	}

	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	executable(t, onPath, "git")
	fakeJJ(t, onPath, currentJJ)
	if reads, err = readBoot(cfg, environLookup([]string{"PATH=" + onPath}), os.Getenv, false, quietLogger()); err != nil {
		t.Fatalf("readBoot with answers and a repository: %v", err)
	}
	if reads.answers.gh != reads.tmux.tools["gh"] || reads.answers.gh != gh {
		t.Errorf("boot's answer gh = %q, want the daemon tools' gh %q", reads.answers.gh, reads.tmux.tools["gh"])
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("boot ran api_key_command (stat %s: %v)", marker, err)
	}
}

// atInPasswordDSN is a postgres:// URL whose password holds an unencoded "@", with fixture-head
// before it and fixture-tail after it: pgx reads fixture-tail as part of the host, which every
// connect error prints. It is joined from its parts so that no line of this file holds a URL with
// a password.
const atInPasswordDSN = "postgres://legion-daemon:fixture-head" + "@" + "fixture-tail" + "@" +
	"conversations.internal.example:5432/omp_sessions"

// carriesWhatTheFileHolds says whether a refusal carries what a DSN file of these tests holds: a
// keyword DSN's password, or either part of a password holding an unencoded "@".
func carriesWhatTheFileHolds(err error) bool {
	return slices.ContainsFunc([]string{"fixture-keyword-password", "fixture-head", "fixture-tail"},
		func(held string) bool { return strings.Contains(err.Error(), held) })
}

// The conversation store's URL file is read at boot as every other *_file is: a missing file
// refuses the boot naming the key and why, a blank file or one holding no postgres:// URL naming
// the key and the file, and one whose URL pgdsn.Refusal refuses, a password holding an unencoded
// "@" here, naming the key and the file in pgdsn.Refusal's words; no refusal carries what the file
// holds.
func TestBootRefusesAConversationStoreFileWithNoURL(t *testing.T) {
	t.Parallel()
	gh := binary(t, t.TempDir(), "gh")
	withGH := environLookup([]string{"PATH=" + t.TempDir(), "LEGION_GH_PATH=" + gh, leakguard.TagVariable + "=" + leakCheckTag})
	missing := filepath.Join(t.TempDir(), "absent")
	notAURL := dsnFileOf(t, "host=conversations.internal.example password=fixture-keyword-password\n")
	blank := dsnFileOf(t, "\n")
	atInPassword := dsnFileOf(t, atInPasswordDSN+"\n")
	for _, tc := range []struct{ name, file, want string }{
		{"missing", missing, "answers.conversation_store.dsn_file names a file that could not be read"},
		{"blank", blank, "answers.conversation_store.dsn_file names " + blank + ", which is empty"},
		{"no URL", notAURL, "answers.conversation_store.dsn_file names " + notAURL + ", which holds no postgres:// URL"},
		{"an unencoded @ in its password", atInPassword,
			"answers.conversation_store.dsn_file names " + atInPassword + `, whose URL cannot be read: it holds an "@" after its user name and password`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.LeakCheck = leakCheckSetting
			cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), tc.file)
			_, err := readBoot(cfg, withGH, os.Getenv, true, quietLogger())
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || carriesWhatTheFileHolds(err) {
				t.Fatalf("readBoot = %v, want a refusal starting %q that carries nothing the file holds", err, tc.want)
			}
		})
	}
}

// Under runtime.tmux.session_store postgres boot reads the session database's URL from
// runtime.tmux.session_dsn_file, which the boot gate probes; with `answers` set that URL is the
// answer loop's conversation store, in the one place the loop reads it from whichever key named
// the file (config settles answers.conversation_store's file to the tmux one).
func TestATmuxSessionDatabaseIsTheAnswerLoopsConversationStore(t *testing.T) {
	t.Parallel()
	onPath := t.TempDir()
	binary(t, onPath, "gh")
	leakguardtest.Write(t, onPath, leakguardtest.Passes)
	lookup := environLookup([]string{"PATH=" + onPath})
	file := dsnFileOf(t, conversationsDSN+"\n")
	cfg := testConfig(t)
	cfg.Runtime.Tmux = &config.Tmux{SessionDSNFile: file}

	reads, err := readBoot(cfg, lookup, os.Getenv, false, quietLogger())
	if err != nil {
		t.Fatalf("readBoot under a tmux session database: %v", err)
	}
	if reads.tmux.sessionDSN != conversationsDSN || reads.answers.conversationDSN != "" {
		t.Errorf("boot read session database %q and conversation store %q; want the file's URL and none without answers",
			reads.tmux.sessionDSN, reads.answers.conversationDSN)
	}

	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), file)
	if reads, err = readBoot(cfg, lookup, os.Getenv, false, quietLogger()); err != nil {
		t.Fatalf("readBoot under a tmux session database with answers: %v", err)
	}
	if reads.answers.conversationDSN != conversationsDSN {
		t.Errorf("boot's conversation store = %q, want the session database's URL", reads.answers.conversationDSN)
	}
}

// runtime.tmux.session_dsn_file is read at boot as every other *_file is: a missing file refuses
// the boot naming the key and why, a blank file or one holding no postgres:// URL naming the key
// and the file, and one whose URL pgdsn.Refusal refuses, a password holding an unencoded "@" here,
// naming the key and the file in pgdsn.Refusal's words; no refusal carries what the file holds.
func TestBootRefusesATmuxSessionDSNFileWithNoURL(t *testing.T) {
	t.Parallel()
	lookup := environLookup([]string{"PATH=" + t.TempDir()})
	missing := filepath.Join(t.TempDir(), "absent")
	notAURL := dsnFileOf(t, "host=sessions.internal.example password=fixture-keyword-password\n")
	blank := dsnFileOf(t, "\n")
	atInPassword := dsnFileOf(t, atInPasswordDSN+"\n")
	for _, tc := range []struct{ name, file, want string }{
		{"missing", missing, "runtime.tmux.session_dsn_file names a file that could not be read"},
		{"blank", blank, "runtime.tmux.session_dsn_file names " + blank + ", which is empty"},
		{"no URL", notAURL, "runtime.tmux.session_dsn_file names " + notAURL + ", which holds no postgres:// URL"},
		{"an unencoded @ in its password", atInPassword,
			"runtime.tmux.session_dsn_file names " + atInPassword + `, whose URL cannot be read: it holds an "@" after its user name and password`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Runtime.Tmux = &config.Tmux{SessionDSNFile: tc.file}
			_, err := readBoot(cfg, lookup, os.Getenv, false, quietLogger())
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || carriesWhatTheFileHolds(err) {
				t.Fatalf("readBoot = %v, want a refusal starting %q that carries nothing the file holds", err, tc.want)
			}
		})
	}
}

// A tmux daemon with `answers` resolves at boot, beside its gh, the shared public leak check the
// answer loop's gh tool runs before every call, in the three places it resolves a pane's:
// PUBLIC_LEAK_CHECK, then public-leak-check.sh on PATH, then the pointer file under the
// configuration home. With none of the three it refuses to start, naming the check and each place
// it looked.
func TestATmuxDaemonWithAnswersRefusesToStartWithoutALeakCheck(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), "")
	onPath, home := t.TempDir(), t.TempDir()
	binary(t, onPath, "gh")
	noCheck := []string{"PATH=" + onPath, "HOME=" + home}
	pointer := filepath.Join(home, ".config", "public-leak-check", "path")
	want := "answers needs the shared public leak check on the daemon's host, which the answer loop's gh tool runs before every call, and none was found: " +
		"PUBLIC_LEAK_CHECK names none, public-leak-check.sh is on no PATH entry, and the pointer file " + pointer + " could not be read: " +
		"no such file or directory: install it with the check's own installer, or set PUBLIC_LEAK_CHECK to its path"
	if _, err := readBoot(cfg, environLookup(noCheck), os.Getenv, false, quietLogger()); err == nil || err.Error() != want {
		t.Fatalf("readBoot with answers and no leak check = %v, want %q", err, want)
	}

	named := leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes)
	checkDir := t.TempDir()
	onCheckPath := leakguardtest.Write(t, checkDir, leakguardtest.Passes)
	pointed := leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes)
	if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, []byte(pointed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"PUBLIC_LEAK_CHECK", append(slices.Clone(noCheck), leakguard.CheckVar+"="+named), named},
		{"public-leak-check.sh on PATH", []string{"PATH=" + onPath + string(filepath.ListSeparator) + checkDir, "HOME=" + home}, onCheckPath},
		{"the pointer file", noCheck, pointed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, err := readBoot(cfg, environLookup(tc.env), os.Getenv, false, quietLogger())
			if err != nil || reads.answers.leakCheck != tc.want || reads.answers.leakCheckTag != "" {
				t.Fatalf("readBoot = check %q, pin %q, %v; want the check %s and no pin", reads.answers.leakCheck, reads.answers.leakCheckTag, err, tc.want)
			}
		})
	}
}

// A Kubernetes daemon with `answers` reads at boot, beside its gh, the pin it installs the shared
// public leak check at for the answer loop's gh tool: its environment's LEGION_LEAK_CHECK_TAG,
// which the worker image sets. Without it, or with a pin not of the published shape, it refuses to
// start, naming the check and the variable, and so does `legion config check`; a check on the
// daemon's host is none of a Kubernetes daemon's.
func TestAKubernetesDaemonWithAnswersRefusesToStartWithoutTheCheckPin(t *testing.T) {
	t.Parallel()
	gh := binary(t, t.TempDir(), "gh")
	machine := leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes)
	noPin := []string{"PATH=" + t.TempDir(), "LEGION_GH_PATH=" + gh, leakguard.CheckVar + "=" + machine}
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.LeakCheck = leakCheckSetting
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), dsnFileOf(t, conversationsDSN+"\n"))
	const needs = "answers needs the shared public leak check, which the answer loop's gh tool runs before every call, " +
		"and a Kubernetes daemon installs it at the tag LEGION_LEAK_CHECK_TAG names"
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"no pin", noPin, needs + ", which the daemon's environment does not set: run the daemon from the worker image, " +
			"which sets it to the image's pin, or set it to the tag runtime.kubernetes.image pins (its ARG LEAK_CHECK_TAG)"},
		{"a pin of another shape", append(slices.Clone(noPin), leakguard.TagVariable+"=v18"), needs + `, which is "v18", not of the published shape leak-check-v<n>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prepare(cfg, quietLogger(), overrides{environ: tc.env}); err == nil || err.Error() != tc.want {
				t.Fatalf("prepare = %v, want %q", err, tc.want)
			}
			if _, _, err := CheckStart(cfg, environLookup(tc.env)); err == nil || err.Error() != tc.want {
				t.Fatalf("CheckStart = %v, want %q", err, tc.want)
			}
		})
	}
	reads, err := readBoot(cfg, environLookup(append(slices.Clone(noPin), leakguard.TagVariable+"="+leakCheckTag)), os.Getenv, true, quietLogger())
	if err != nil || reads.answers.leakCheckTag != leakCheckTag || reads.answers.leakCheck != "" {
		t.Fatalf("readBoot = pin %q, check %q, %v; want the pin %s and no check of the host's", reads.answers.leakCheckTag, reads.answers.leakCheck, err, leakCheckTag)
	}
}

// A Kubernetes daemon with `answers` refuses to start where the deployment names no shared leak
// check (config's leak_check, leakguard.Setting): its gh tool installs the check from the
// repository that setting names and holds it to that manifest digest, and a deployment with none
// has neither. The refusal names the setting's two variables and its legion.yaml block, and comes
// though the pin is set and a check is on the daemon's host; `legion config check` makes it too.
// The same configuration with the setting reads the pin.
func TestAKubernetesDaemonWithAnswersRefusesToStartWithoutTheLeakCheckSetting(t *testing.T) {
	t.Parallel()
	gh := binary(t, t.TempDir(), "gh")
	machine := leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes)
	env := []string{"PATH=" + t.TempDir(), "LEGION_GH_PATH=" + gh, leakguard.CheckVar + "=" + machine, leakguard.TagVariable + "=" + leakCheckTag}
	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), dsnFileOf(t, conversationsDSN+"\n"))
	const want = "answers needs the shared public leak check, which the answer loop's gh tool runs before every call, " +
		"and a Kubernetes daemon reads it from the repository the deployment's leak check setting names, which names none: " +
		"set LEGION_LEAK_CHECK_REPOSITORY and LEGION_LEAK_CHECK_MANIFEST_SHA256 in the daemon's environment, " +
		"or leak_check.repository and leak_check.manifest_sha256 in legion.yaml"
	if _, err := prepare(cfg, quietLogger(), overrides{environ: env}); err == nil || err.Error() != want {
		t.Fatalf("prepare with no leak check setting = %v, want %q", err, want)
	}
	if _, _, err := CheckStart(cfg, environLookup(env)); err == nil || err.Error() != want {
		t.Fatalf("CheckStart with no leak check setting = %v, want %q", err, want)
	}
	cfg.LeakCheck = leakCheckSetting
	if reads, err := readBoot(cfg, environLookup(env), os.Getenv, true, quietLogger()); err != nil || reads.answers.leakCheckTag != leakCheckTag {
		t.Fatalf("readBoot under the setting = pin %q, %v; want the pin %s", reads.answers.leakCheckTag, err, leakCheckTag)
	}
}
