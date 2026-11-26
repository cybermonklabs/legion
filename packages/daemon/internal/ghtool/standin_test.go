package ghtool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The stand-in gh is this test binary, linked as `gh` into a directory of its own (standInGH): run
// under that name it answers as gh, so the environment it records is exactly the one the tool gave
// it, with no shell between. It records every run in invocations.jsonl beside it, answers `<path>
// --help` with gh 2.98.0's help of that command, and answers every other run as behaviour.json
// beside it says. Every other run of this binary takes the package's migrated Postgres template,
// which the one test that records calls in the store clones (testpg.Main), and gives the bounds
// the tool's waits on a program take through programbound, its help reads' (ghroute.ReadHelp) and
// its leak guard's reads among them, what is left of the binary's deadline
// (testwait.BinaryDeadline) less 30 s when the wait starts, so a stand-in a loaded machine makes
// slow is not stopped at a product bound. Under -timeout 0 the product's bounds hold.
//
// Only TestMain makes the stand-in of a copy a test starts: in a build of these tests without it,
// such a copy runs the tests, each of which starts copies that run them again. standInGH refuses
// to link a copy in such a process (standInCopyRefusal), and TestACopyStartedAsGhRunsAsTheStandIn
// fails in such a build by name.
func TestMain(m *testing.M) {
	if startedAsTheStandIn() {
		os.Exit(standIn())
	}
	if deadline, ok := testwait.BinaryDeadline(); ok {
		programbound.SetDeadline(deadline)
	}
	os.Exit(testpg.Main("legion_ghtool_test", m.Run))
}

// startedAsTheStandIn says whether this process was started as the stand-in gh: this test binary
// run under the name `gh`, as standInGH links it.
func startedAsTheStandIn() bool {
	return filepath.Base(os.Args[0]) == "gh"
}

// standInCopyRefusal is why this process may start no copy of this test binary, nil when it may.
// A process run under the name `gh` was started as the stand-in, and TestMain runs such a process
// as the stand-in, so one that runs the tests runs in a build without TestMain, and a copy started
// there would run the tests again.
func standInCopyRefusal() error {
	if startedAsTheStandIn() {
		return fmt.Errorf("this process was started under the name gh (%s), so a test started it as the stand-in gh, but it runs the tests: this build of the tests has no TestMain that runs the stand-in, and a copy started here would run the tests again", os.Args[0])
	}
	return nil
}

// standInCopyRefusal, which standInGH asks before it links any copy, refuses a process run under
// the name `gh` and no other. While TestMain is in the build no copy reaches it, so this test is
// what holds the refusal. It sets os.Args, so it runs alone (no t.Parallel) and puts them back
// when it ends.
func TestAProcessStartedAsTheStandInStartsNoCopy(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	if err := standInCopyRefusal(); err != nil {
		t.Fatalf("as %s: %v, want no refusal", args[0], err)
	}
	os.Args = append([]string{filepath.Join(t.TempDir(), "gh")}, args[1:]...)
	if err := standInCopyRefusal(); err == nil || !strings.Contains(err.Error(), os.Args[0]) {
		t.Fatalf("as %s: %v, want a refusal that names it", os.Args[0], err)
	}
}

// A copy linked as `gh` runs as the stand-in: it answers -h, which is no --help read, with the
// stdout its behaviour names. The stand-in does not read -h. In a build without TestMain, the
// testing package reads -h before any test runs, prints its own flag usage and exits, so this test
// fails there by name and its copy starts no copy of its own.
func TestACopyStartedAsGhRunsAsTheStandIn(t *testing.T) {
	t.Parallel()
	const answer = "the stand-in gh's answer\n"
	cmd := exec.Command(filepath.Join(standInGH(t, behaviour{Stdout: answer}), "gh"), "-h")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || string(out) != answer {
		t.Fatalf("a copy started as gh with -h: %v, printed %q, want exit 0 and %q alone\n%s", err, out, answer, stderr.String())
	}
}

// behaviour is how the stand-in answers a run: its stdout (or the response for its exact arguments,
// joined by spaces), its stderr, how long it sleeps first and its exit status; HelpDir is the saved
// help it answers --help from. Visibility is what it answers the leak guard's own read of a
// repository's visibility with, "public" where a behaviour names none; VisibilityRefused, where it
// is set, is what it prints for that read instead, exiting 1, as gh prints GitHub's refusal of the
// read (leakguardtest.GhReadNotFound).
type behaviour struct {
	Stdout    string
	Stderr    string
	Responses map[string]string
	Sleep     time.Duration
	Exit      int
	// Killed has the stand-in end itself with SIGKILL once it has printed, as a kill from outside
	// the tool would end gh.
	Killed            bool
	HelpDir           string
	Visibility        string
	VisibilityRefused string
}

// invocation is one run of the stand-in: its arguments, and for a run that is no help read, its
// whole environment, its working directory, and which of HOME, the XDG directories and the working
// directory were empty.
type invocation struct {
	Help  bool
	Argv  []string
	Env   []string
	Cwd   string
	Empty map[string]bool
}

// guardRead is whether this run is the leak guard's own read of a repository's visibility rather
// than a call of the tool's: the guard names the repository it reads in its child's environment
// (leakguard.VisibilityReadVar), which is how `legion gh` tells the two apart as well.
func (i invocation) guardRead() bool {
	return slices.ContainsFunc(i.Env, func(pair string) bool {
		return strings.HasPrefix(pair, leakguard.VisibilityReadVar+"=")
	})
}

func standIn() int {
	dir := filepath.Dir(os.Args[0])
	data, err := os.ReadFile(filepath.Join(dir, "behaviour.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "stand-in gh:", err)
		return 101
	}
	var b behaviour
	if err := json.Unmarshal(data, &b); err != nil {
		fmt.Fprintln(os.Stderr, "stand-in gh:", err)
		return 101
	}
	args := os.Args[1:]
	if n := len(args); n > 0 && args[n-1] == "--help" {
		recordInvocation(dir, invocation{Help: true, Argv: args})
		text, err := standInHelp(b.HelpDir, args[:n-1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Print(text)
		return 0
	}
	cwd, _ := os.Getwd()
	inv := invocation{Argv: args, Env: slices.Sorted(slices.Values(os.Environ())), Cwd: cwd, Empty: map[string]bool{}}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		inv.Empty[name] = isEmptyDir(os.Getenv(name))
	}
	inv.Empty["cwd"] = isEmptyDir(cwd)
	recordInvocation(dir, inv)
	// The guard's own visibility read is answered with the visibility and nothing else: the
	// run's stdout, sleep and exit are a call's, and this read is no call of the tool's.
	if inv.guardRead() {
		if b.VisibilityRefused != "" {
			os.Stdout.WriteString(b.VisibilityRefused)
			return 1
		}
		visibility := b.Visibility
		if visibility == "" {
			visibility = "public"
		}
		fmt.Println(visibility)
		return 0
	}
	time.Sleep(b.Sleep)
	out := b.Stdout
	if response, ok := b.Responses[strings.Join(args, " ")]; ok {
		out = response
	}
	os.Stdout.WriteString(out)
	os.Stderr.WriteString(b.Stderr)
	if b.Killed {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return b.Exit
}

func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// recordInvocation appends inv to the stand-in's invocations.jsonl in dir.
func recordInvocation(dir string, inv invocation) {
	line, _ := json.Marshal(inv)
	file, err := os.OpenFile(filepath.Join(dir, "invocations.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// standInHelp is gh 2.98.0's help of path: the saved help of that command when there is one (gh's
// own for none), and otherwise the help gh prints, rebuilt from its saved reference: a group's
// usage and its commands, or a leaf's usage, its options and the options it inherits (--help, and
// -R for the commands of a group that reads a repository). A command neither knows is gh's
// `unknown command`.
func standInHelp(helpDir string, path []string) (string, error) {
	name := "gh"
	if len(path) > 0 {
		name = strings.Join(path, "_")
	}
	if data, err := os.ReadFile(filepath.Join(helpDir, name+".txt")); err == nil {
		return string(data), nil
	}
	reference, err := os.Open(filepath.Join(helpDir, "reference.txt"))
	if err != nil {
		return "", err
	}
	defer reference.Close()
	spelled := "gh " + strings.Join(path, " ")
	var usage string
	var flags, children []string
	inside := false
	scanner := bufio.NewScanner(reference)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			heading := strings.Trim(strings.TrimSpace(strings.TrimLeft(line, "#")), "`")
			inside = false
			rest, ok := strings.CutPrefix(heading, spelled)
			if !ok || (rest != "" && rest[0] != ' ') {
				continue
			}
			if words := strings.Fields(rest); len(words) > 0 && referenceCommandWord.MatchString(words[0]) {
				children = append(children, words[0])
				continue
			}
			usage, inside = heading, true
			continue
		}
		if inside && referenceFlagLine.MatchString(line) {
			flags = append(flags, line)
		}
	}
	if usage == "" {
		return "", fmt.Errorf("unknown command %q for %q", path[len(path)-1], "gh "+strings.Join(path[:len(path)-1], " "))
	}
	if len(children) > 0 {
		text := "USAGE\n  " + spelled + " <command> [flags]\n\nAVAILABLE COMMANDS\n"
		for _, child := range children {
			text += "  " + child + ":  " + child + "\n"
		}
		return text, nil
	}
	text := "USAGE\n  " + usage + "\n\nFLAGS\n" + strings.Join(flags, "\n") + "\n\nINHERITED FLAGS\n      --help                     Show help for command\n"
	if slices.Contains([]string{"pr", "issue", "release", "run", "workflow", "label", "cache"}, path[0]) {
		text += "  -R, --repo [HOST/]OWNER/REPO   Select another repository using the [HOST/]OWNER/REPO format\n"
	}
	return text, nil
}

// referenceCommandWord is a word of a command path in gh's reference; referenceFlagLine one of its
// option lines (internal/ghroute's ghCommandWord and ghFlagLine).
var (
	referenceCommandWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	referenceFlagLine    = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}(.*)|\s*)$`)
)
