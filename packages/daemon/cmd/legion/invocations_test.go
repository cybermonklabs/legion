package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// recordInvocations names the `legion` of the release before this table's change: with it, the
// test runs each row's argv with that binary and writes what it did into the table's old_exit
// and old_stdout_sha256 columns, in place of comparing.
var recordInvocations = flag.String("record-invocations", "", "a legion binary of the release before: write its exit and stdout digest for each row's argv into testdata/invocations.tsv")

// invocationsTable is the calls of `legion` outside tests that one search found when `legion`
// moved to cobra, read at this repository's 0af7286346, the deployment repository's c76d6d5e59 and
// the dotfiles' b975ec0c72: `git grep` of each for `legion` followed by one of its command words,
// and of the daemon for its argv builders (`exec.Command`, `[]string{` with a command word), each
// hit read in its file — the daemon's rendered argv, the image's wrappers and hooks, CI, the docs
// generators and walkthroughs, and the deployment repository's programs and workflows. Nothing
// finds a caller added later: by convention, whoever adds one adds its row. Its columns: the
// caller (repository, file:line and symbol or step); the argv as the caller writes it; the exit
// status and stdout sha256 the release before gave it (`-` for a caller that reads the new
// output); the argv the caller runs now; and where that argv moved — `this-pr` in the change that
// moved the command, `pin-pr:<pin>` in the deployment repository's pin of the release that holds
// it, or `unchanged`. A row whose exit this repository changed on purpose carries the new exit:
// the two `workspace-init fetch` rows carry 3, the exit for a cause outside the issue's volume
// (workspace.InitExitOutsideVolume).
const invocationsTable = "testdata/invocations.tsv"

// absentRoot is where every absolute path a row names is moved to, a directory no machine has:
// a call reads or makes nothing of the machine it runs on, and fails where it would touch one.
const absentRoot = "/nonexistent-legion-invocations"

// placeholderValues is what each `<name>` of a row stands for in its run, one value a name, so
// that the run before and the run after are the same call.
var placeholderValues = map[string]string{
	"streamURL":        "tcp://127.0.0.1:1",
	"generationDir":    "generation-1",
	"terminationGrace": "30s",
	"agentSecretsBin":  absentRoot + "/agent-secrets",
	"agentArgv":        "true",
	"owner/repo":       "acme/widgets",
	"issue":            "DEMO-1",
	"branch":           "main",
	"sandbox":          "legion-demo-1",
	"role":             "tester",
	"instance":         "acme:region:sessions",
	"database":         "sessions",
	"user":             "legion-sessions@acme.iam",
	"account":          "legion-sessions@acme.iam.gserviceaccount.com",
	"audience":         "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/legion-pool/providers/legion-cluster",
	"workloadPool":     "acme.svc.id.goog",
	"identityProvider": "https://container.googleapis.com/v1/projects/acme/locations/region/clusters/legion",
	"contract":         "1",
	"budget":           "1m",
	"roleReferences":   "x",
	"roleConfigFiles":  "tester=" + absentRoot + "/overlay.yml",
	"refresh":          "10m",
	"claude":           absentRoot + "/claude",
	"mcpConfig":        absentRoot + "/mcp.json",
	"pluginDir":        absentRoot + "/plugin",
	"session":          "session-1",
	"promptFile":       absentRoot + "/prompt.md",
	"rolesDir":         absentRoot + "/roles",
	"generation":       "1",
	"home":             absentRoot + "/home",
	"homeMounts":       absentRoot + "/home-mounts",
	"fixedRoot":        absentRoot + "/fixed",
	"fd":               "3",
	"shimArgv":         "true",
	"args":             "version",
	"ghArgs":           "--version",
	"root":             absentRoot + "/bin-root",
	"gitArgs":          "status",
	"bootTokenFile":    absentRoot + "/boot-token",
	"providerEnvDir":   absentRoot + "/providers",
	"remote":           "origin",
	"remoteURL":        "https://example.invalid/acme/widgets.git",
	"probe":            absentRoot + "/probe",
	"command":          "claims",
}

// absolutePrefixes are the machine paths a row names literally, each moved under absentRoot.
var absolutePrefixes = []string{"/var/run/legion", "/opt/legion", "/opt/omp", "/etc/legion-config", "/legion"}

// singleDashLong is a single-dash flag of two or more letters, which Go's flag package read and
// pflag reads as a cluster of one-letter flags.
var singleDashLong = regexp.MustCompile(`^-[A-Za-z][A-Za-z-]+$`)

// placeholder is a `<name>` of a row, which placeholderValues gives the value of.
var placeholder = regexp.MustCompile(`<([^<>]+)>`)

// parserRefusal is a line in which `legion` refuses a call's words before it runs anything: a word
// it does not take, a stray word before a wrapper's `--` among them (afterDash), a flag it does not
// have or that lacks its value, a command it does not have, a count of words it does not take, or
// a form that moved. Every row is a call its caller makes and the release before ran, so such a
// line is a difference, whatever the exit status: `claude-bridge` exits 2 and `launcher role-init`
// 71 whether they refuse a stray word or fail further on, as they did under Go's flag package.
var parserRefusal = regexp.MustCompile(`(?m)^legion( [a-z-]+)*: (unexpected argument|unknown (shorthand )?flag|unknown command|flag needs an argument|invalid argument|bad flag syntax|name .*\(got [0-9]+ words\)|.* is now "legion )`)

// invocation is one row of the table.
type invocation struct {
	caller, argv, oldExit, oldDigest, newArgv, movedIn string
}

// Each row of the table runs, in its new form, against the built binary with the exit status and
// standard output the release before gave the old form, and with no refusal of its words on
// standard error (parserRefusal): a caller that relied on how Go's flag package read its words (a
// single-dash long flag, a flag after a positional, a word before a wrapper's `--`) is a row whose
// run differs. Each run reads and writes nothing of the machine: its paths are under absentRoot,
// its HOME and working directory are the test's own, and nothing it calls answers. A row whose
// caller this change moved to a new output carries `-` for the digest: its exit is compared, and
// the caller's own test reads the output. A row whose exit was changed on purpose carries the new
// exit in old_exit (invocationsTable), and the run is compared with that.
func TestInvocationTable(t *testing.T) {
	rows := readInvocations(t)
	if len(rows) == 0 {
		t.Fatalf("%s holds no row: UNKNOWN, nothing was compared", invocationsTable)
	}
	if *recordInvocations != "" {
		recordOld(t, rows, *recordInvocations)
		return
	}
	legion := buildLegion(t)
	for _, row := range rows {
		args := concrete(t, row.newArgv)
		for _, arg := range args {
			if singleDashLong.MatchString(arg) {
				t.Errorf("%s: %q holds the single-dash long flag %q, which pflag does not read", row.caller, row.newArgv, arg)
			}
		}
		if row.oldExit == "?" || row.oldDigest == "?" {
			t.Errorf("%s: no recorded run of the release before (old_exit %q, old_stdout_sha256 %q)", row.caller, row.oldExit, row.oldDigest)
			continue
		}
		code, digest, stderr, err := runInvocation(t, legion, args)
		switch {
		case err != nil:
			t.Errorf("%s: %s: %v", row.caller, row.newArgv, err)
		case strconv.Itoa(code) != row.oldExit:
			t.Errorf("%s: %s exited %d, the release before %s", row.caller, row.newArgv, code, row.oldExit)
		case row.oldDigest != "-" && digest != row.oldDigest:
			t.Errorf("%s: %s printed other than the release before (stdout sha256 %s, want %s)", row.caller, row.newArgv, digest, row.oldDigest)
		}
		if refusal := parserRefusal.FindString(stderr); refusal != "" {
			t.Errorf("%s: %s is refused by its words, which the release before ran: %q", row.caller, row.newArgv, refusal)
		}
	}
}

// readInvocations reads the table, refusing a header or a row it cannot read whole.
func readInvocations(t *testing.T) []invocation {
	t.Helper()
	data, err := os.ReadFile(invocationsTable)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lines[0] != "caller\targv\told_exit\told_stdout_sha256\tnew_argv\tmoved_in" {
		t.Fatalf("%s: header %q", invocationsTable, lines[0])
	}
	var rows []invocation
	for i, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			t.Fatalf("%s:%d: %d columns, want 6: %q", invocationsTable, i+2, len(f), line)
		}
		if f[5] != "this-pr" && f[5] != "unchanged" && (!strings.HasPrefix(f[5], "pin-pr:") || f[5] == "pin-pr:") {
			t.Fatalf("%s:%d: moved_in %q", invocationsTable, i+2, f[5])
		}
		if f[5] == "unchanged" && f[1] != f[4] {
			t.Fatalf("%s:%d: argv and new_argv differ, but the row says unchanged", invocationsTable, i+2)
		}
		rows = append(rows, invocation{f[0], f[1], f[2], f[3], f[4], f[5]})
	}
	return rows
}

// recordOld runs each row's old argv with legion, the release before, and writes its exit and
// stdout digest into the table, a `-` digest staying `-`.
func recordOld(t *testing.T, rows []invocation, legion string) {
	t.Helper()
	var out strings.Builder
	out.WriteString("caller\targv\told_exit\told_stdout_sha256\tnew_argv\tmoved_in\n")
	for _, row := range rows {
		code, digest, _, err := runInvocation(t, legion, concrete(t, row.argv))
		if err != nil {
			t.Fatalf("%s: %s: %v", row.caller, row.argv, err)
		}
		if row.oldDigest == "-" {
			digest = "-"
		}
		fmt.Fprintf(&out, "%s\t%s\t%d\t%s\t%s\t%s\n", row.caller, row.argv, code, digest, row.newArgv, row.movedIn)
	}
	if err := os.WriteFile(invocationsTable, []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// concrete is argv as a run gives it to the binary: its words after `legion`, each `<name>`
// replaced by its placeholderValues entry and each machine path moved under absentRoot. Words are
// split on spaces, and a single-quoted word is one word.
func concrete(t *testing.T, argv string) []string {
	t.Helper()
	words, err := splitWords(argv)
	if err != nil || len(words) == 0 || words[0] != "legion" {
		t.Fatalf("%q is no legion command line: %v", argv, err)
	}
	var args []string
	for _, word := range words[1:] {
		var unknown []string
		word = placeholder.ReplaceAllStringFunc(word, func(m string) string {
			value, ok := placeholderValues[m[1:len(m)-1]]
			if !ok {
				unknown = append(unknown, m)
			}
			return value
		})
		if len(unknown) > 0 {
			t.Fatalf("%q names %v, which placeholderValues holds no value for", argv, unknown)
		}
		for _, prefix := range absolutePrefixes {
			if word == prefix || strings.HasPrefix(word, prefix+"/") {
				word = absentRoot + word
				break
			}
		}
		args = append(args, word)
	}
	return args
}

// splitWords splits a command line on spaces, reading a single-quoted span as part of one word.
func splitWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for _, r := range line {
		switch {
		case r == '\'':
			quoted, inWord = !quoted, true
		case r == ' ' && !quoted:
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quoted {
		return nil, errors.New("an unclosed quote")
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

// runInvocation runs legion with args in a directory and a HOME of the test's own, with PATH and
// nothing else of the test process's environment but the deployment checker a role of a
// configured deployment is told (leakguard.CheckerVar), standard input empty, and answers its
// exit status, the sha256 of its standard output and its standard error. The checker keeps the
// image hook's row comparable with the release before: under it the guard refuses a push for
// want of a check, as every release did, where with no checker it would run its rules alone. A
// run still going after twenty seconds is an error: a call that waits on something is no call the
// table can compare.
func runInvocation(t *testing.T, legion string, args []string) (code int, digest, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, legion, args...)
	home := t.TempDir()
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_STATE_HOME=" + home + "/state", "XDG_CONFIG_HOME=" + home + "/config",
		leakguard.CheckerVar + "=" + strings.Repeat("5e", 32)}
	var stdout, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &errb
	err = cmd.Run()
	sum := sha256.Sum256(stdout.Bytes())
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return 0, "", "", fmt.Errorf("still running after 20s")
	case err == nil:
		return 0, hex.EncodeToString(sum[:]), errb.String(), nil
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode(), hex.EncodeToString(sum[:]), errb.String(), nil
	}
	return 0, "", "", err
}
