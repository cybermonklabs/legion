package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The negative controls of the check script's judges: the three bun scripts of
// packages/envoy/scripts/check-redact-remote-text.sh that read go test's events, Bun's JUnit
// reports and the print test's REDACT-PRINT lines and print the verdicts the script counts, and
// its judged function, which refuses (exit 2) a judge that failed or printed fewer verdict lines
// than it owes. The script counts a judge's FAIL lines, and an empty or short output holds none,
// so without that refusal it passes the rows no judge judged. They are added to controls in init,
// so runControls runs them with the rest.
//
// Each case runs the script whole, as HEAD commits it, in a worktree of HEAD, so it reads HEAD's
// inventory and runs its own judge lines: no line of it is copied. Stand-ins take the place of what
// lies outside the judges. The go it is given (judgesGo) answers each package's go test with
// recorded events; the gate that go builds (judgesGate) passes its plan and its stages 4 and 5; the
// bun first on its PATH (judgesBun) answers bun test with a recorded JUnit report and bun run
// typecheck with a pass, and hands each bun -e (the inventory's parser and the three judges) to the
// real bun, unless the case plants a fault in that judge. The recorded events and reports pass
// every row of HEAD's inventory, and each control first runs the script on them with no plant and
// requires it to pass, so the refusal each case then requires is its plant's.
func init() {
	controls = append(controls, judgesControls...)
}

var judgesControls = []control{
	{id: "31a", what: "the check script's Go-row judge reads a go.json whose last line a killed go command cut short ({\"Action\":\"output\",\"Package\":, its go exiting 137), and the script must refuse it as a judge that failed",
		own: judgesControl(judgesCase{name: "cut-short", cut: true, expect: judgesFailed("Go-row judge", "")})},
	{id: "31b", what: "each of the check script's three judges fails, bun failing to start it or the judge exiting 1 after it printed every verdict line, and the script must refuse it as a judge that failed",
		own: judgesControl(
			judgesCase{name: "nostart-go", plant: "nostart:go", expect: judgesFailed("Go-row judge", "126")},
			judgesCase{name: "nostart-bun", plant: "nostart:bun", expect: judgesFailed("Bun-row judge", "126")},
			judgesCase{name: "nostart-print", plant: "nostart:print", expect: judgesFailed("print judge", "126")},
			judgesCase{name: "late-go", plant: "late:go", expect: judgesFailed("Go-row judge", "1")},
			judgesCase{name: "late-bun", plant: "late:bun", expect: judgesFailed("Bun-row judge", "1")},
			judgesCase{name: "late-print", plant: "late:print", expect: judgesFailed("print judge", "1")},
		)},
	{id: "31c", what: "each of the check script's three judges exits 0 and prints one verdict line fewer than it owes (a Go row's, a package's step line, a Bun row's, the print test's), and the script must refuse it naming both counts",
		own: judgesControl(
			judgesCase{name: "short-go-row", plant: "short:go-row", expect: judgesShort("Go-row judge", "Go row lines", func(n judgesCounts) int { return n.goRows })},
			judgesCase{name: "short-go-step", plant: "short:go-step", expect: judgesShort("Go-row judge", "package step lines", func(n judgesCounts) int { return n.packages })},
			judgesCase{name: "short-bun", plant: "short:bun", expect: judgesShort("Bun-row judge", "Bun row lines", func(n judgesCounts) int { return n.bunRows })},
			judgesCase{name: "short-print", plant: "short:print", expect: judgesShort("print judge", "PRINT or FAIL print-test lines", func(judgesCounts) int { return 1 })},
		)},
}

// judgesCase is one run of the check script. plant is judgesBun's JUDGES_PLANT: nostart:<judge>,
// where bun fails to start the judge (exit 126); late:<judge>, where the judge runs and prints
// every line and then exits 1; short:<go-row|go-step|bun|print>, where the judge runs and the last
// of those verdict lines drops from its output, its exit status kept. With cut, the first package's
// recorded events end in a line cut short and its go exits 137. expect is the text the script's
// output must hold; the script must exit 2, or 0 for the unplanted run.
type judgesCase struct {
	name, plant string
	cut         bool
	expect      func(n judgesCounts) []string
}

// judgesCounts is what HEAD's inventory owes the judges: a row line for each Go row, a step line
// for each Go package and a row line for each Bun row.
type judgesCounts struct{ goRows, packages, bunRows int }

// judgesPassed is the unplanted run each control starts from.
var judgesPassed = judgesCase{name: "unplanted", expect: func(judgesCounts) []string {
	return []string{"check c-redact-remote-text: passed ("}
}}

func judgesFailed(judge, code string) func(judgesCounts) []string {
	return func(judgesCounts) []string {
		text := "check c-redact-remote-text: refused: the " + judge + " failed (exit "
		if code != "" {
			text += code + ")"
		}
		return []string{text}
	}
}

func judgesShort(judge, what string, owed func(judgesCounts) int) func(judgesCounts) []string {
	return func(n judgesCounts) []string {
		return []string{
			fmt.Sprintf("check c-redact-remote-text: refused: the %s printed %d %s", judge, owed(n)-1, what),
			fmt.Sprintf("where it owes %d", owed(n)),
		}
	}
}

// envoyModule is the module the check script names a package relative to packages/envoy in (its
// module variable).
const envoyModule = "github.com/sjawhar/envoy"

// judgesTimeout bounds one run of the check script on recorded outputs, which takes seconds.
const judgesTimeout = 10 * time.Minute

// judgesControl answers the own function of a judges control: in a worktree of HEAD, the unplanted
// run, which must pass, and then each case.
func judgesControl(cases ...judgesCase) func(string, inventory) (string, string) {
	return func(repo string, _ inventory) (string, string) {
		work, err := os.MkdirTemp(os.Getenv("TMPDIR"), "redact-judges.")
		if err != nil {
			return err.Error(), ""
		}
		defer os.RemoveAll(work)
		wt := filepath.Join(work, "wt")
		if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
			return fmt.Sprintf("the worktree: %v: %s", err, out), ""
		}
		defer exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
		top, err := exec.Command("git", "-C", wt, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return "the worktree's top level: " + err.Error(), ""
		}
		root := strings.TrimSpace(string(top))
		var inv inventory
		data, err := os.ReadFile(filepath.Join(root, inventoryFile))
		if err != nil || json.Unmarshal(data, &inv) != nil {
			return "HEAD's inventory does not parse", ""
		}
		bun, err := exec.LookPath("bun")
		if err != nil {
			return "no bun on PATH to run the judges", ""
		}
		var failed, out []string
		for i, c := range append([]judgesCase{judgesPassed}, cases...) {
			verdict, shown := judgesOnce(work, root, bun, inv, c)
			out = append(out, fmt.Sprintf("judges %s: %s", c.name, shown))
			if verdict == "" {
				continue
			}
			out = append(out, "judges "+c.name+" did not do as it must: "+verdict)
			if i == 0 {
				return "the unplanted run did not pass, so no plant's refusal is its own: " + verdict, strings.Join(out, "\n")
			}
			failed = append(failed, c.name+": "+verdict)
		}
		return strings.Join(failed, "; "), strings.Join(out, "\n")
	}
}

// judgesOnce runs the check script of the worktree at root once, for c, with the stand-ins and the
// recorded outputs in a directory of its own. It answers "" when the script exited as c must and
// printed what c expects, and the reason otherwise, and the exit status with the script's last
// line.
func judgesOnce(work, root, bun string, inv inventory, c judgesCase) (string, string) {
	dir, err := os.MkdirTemp(work, c.name+".")
	if err != nil {
		return err.Error(), ""
	}
	counts, err := judgesRecord(dir, inv, c.cut)
	if err != nil {
		return "the recorded outputs: " + err.Error(), ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), judgesTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "packages/envoy/scripts/check-redact-remote-text.sh"))
	cmd.Dir = root
	cmd.WaitDelay = time.Minute
	cmd.Env = append(os.Environ(),
		"GO="+filepath.Join(dir, "goroot", "bin", "go"),
		"PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+filepath.Join(dir, "tmp"),
		"JUDGES_RUN="+dir,
		"JUDGES_ROOT="+root,
		"JUDGES_PLANT="+c.plant,
		"JUDGES_BUN="+bun,
	)
	output, err := cmd.CombinedOutput()
	text := string(output)
	if ctx.Err() != nil {
		return fmt.Sprintf("the script ran past %s, so this run neither passed nor failed: %s", judgesTimeout, lastLines(text, 6)), "ran past its bound"
	}
	code := 0
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		return "the script did not run: " + err.Error(), ""
	}
	shown := fmt.Sprintf("exit %d: %s", code, lastLines(text, 1))
	want := 2
	if c.plant == "" && !c.cut {
		want = 0
	}
	var wrong []string
	if fired, _ := os.ReadFile(filepath.Join(dir, "fired")); c.plant != "" && !strings.Contains(string(fired), c.plant) {
		wrong = append(wrong, "the plant "+c.plant+" did not take effect")
	}
	if code != want {
		wrong = append(wrong, fmt.Sprintf("the script exited %d, not %d", code, want))
	}
	for _, e := range c.expect(counts) {
		if !strings.Contains(text, e) {
			wrong = append(wrong, fmt.Sprintf("it printed no %q", e))
		}
	}
	if len(wrong) > 0 {
		return strings.Join(wrong, ", ") + "; its last lines: " + lastLines(text, 4), shown
	}
	return "", shown
}

// judgesEvent is one event of go test -json.
type judgesEvent struct {
	Action  string
	Package string
	Test    string `json:",omitempty"`
	Output  string `json:",omitempty"`
}

// judgesKey is the name the stand-ins file a package's or a Bun file's recorded output under:
// every byte other than an ASCII letter or digit made '_', as judgesGo and judgesBun make it.
func judgesKey(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, name)
}

// judgesRecord writes the stand-ins and the recorded outputs of one run into dir: for each Go
// package of inv, events/<key>.json, go test's events of a run in which each row's test and each
// test above it passed and the package passed, and events/<key>.code, its exit status; for each Bun
// file, junit/<key>.xml, a report holding a passing testcase for each row's test under its full
// name. The first package also logs one REDACT-PRINT line, as redacttest.PrintTest does. With cut,
// its events end in a line cut short and its exit status is 137, as a killed go command leaves
// them.
func judgesRecord(dir string, inv inventory, cut bool) (judgesCounts, error) {
	counts := judgesCounts{goRows: len(inv.Go), bunRows: len(inv.Bun)}
	for _, d := range []string{"goroot/bin", "bin", "tmp", "events", "junit"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return counts, err
		}
	}
	for path, text := range map[string]string{"goroot/bin/go": judgesGo, "bin/bun": judgesBun, "gate": judgesGate} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(text), 0o755); err != nil {
			return counts, err
		}
	}
	keys := map[string]string{}
	key := func(name string) (string, error) {
		k := judgesKey(name)
		if other, ok := keys[k]; ok && other != name {
			return "", fmt.Errorf("%s and %s are both filed under %s", other, name, k)
		}
		keys[k] = name
		return k, nil
	}
	tests := map[string][]string{}
	for _, r := range inv.Go {
		tests[r.Package] = append(tests[r.Package], r.Test)
	}
	packages := make([]string, 0, len(tests))
	for p := range tests {
		packages = append(packages, p)
	}
	sort.Strings(packages)
	counts.packages = len(packages)
	for i, p := range packages {
		name := p
		if strings.HasPrefix(p, "./") {
			name = strings.TrimSuffix(envoyModule+"/"+strings.TrimPrefix(p, "./"), "/")
		}
		var all []string
		seen := map[string]bool{}
		for _, t := range tests[p] {
			parts := strings.Split(t, "/")
			for j := 1; j <= len(parts); j++ {
				if up := strings.Join(parts[:j], "/"); !seen[up] {
					seen[up] = true
					all = append(all, up)
				}
			}
		}
		events := []judgesEvent{{Action: "start", Package: name}}
		for _, t := range all {
			events = append(events, judgesEvent{Action: "run", Package: name, Test: t})
		}
		if i == 0 {
			events = append(events, judgesEvent{Action: "output", Package: name, Test: all[0], Output: "    printtest.go:237: REDACT-PRINT JudgesStandIn rows=1 same=1 reshaped=0 relocated=0 blind=0\n"})
		}
		for j := len(all) - 1; j >= 0; j-- {
			events = append(events, judgesEvent{Action: "pass", Package: name, Test: all[j]})
		}
		events = append(events, judgesEvent{Action: "pass", Package: name})
		var b strings.Builder
		for _, e := range events {
			line, err := json.Marshal(e)
			if err != nil {
				return counts, err
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		code := "0"
		if cut && i == 0 {
			b.WriteString(`{"Action":"output","Package":`)
			code = "137"
		}
		k, err := key(p)
		if err != nil {
			return counts, err
		}
		if err := os.WriteFile(filepath.Join(dir, "events", k+".json"), []byte(b.String()), 0o644); err != nil {
			return counts, err
		}
		if err := os.WriteFile(filepath.Join(dir, "events", k+".code"), []byte(code+"\n"), 0o644); err != nil {
			return counts, err
		}
	}
	files := map[string][]string{}
	for _, r := range inv.Bun {
		files[r.File] = append(files[r.File], r.Test)
	}
	for file, names := range files {
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<testsuites name=\"bun test\">\n  <testsuite name=\"")
		if err := xml.EscapeText(&b, []byte(file)); err != nil {
			return counts, err
		}
		b.WriteString("\">\n")
		for _, t := range names {
			b.WriteString("    <testcase name=\"")
			if err := xml.EscapeText(&b, []byte(t)); err != nil {
				return counts, err
			}
			b.WriteString("\" />\n")
		}
		b.WriteString("  </testsuite>\n</testsuites>\n")
		k, err := key(file)
		if err != nil {
			return counts, err
		}
		if err := os.WriteFile(filepath.Join(dir, "junit", k+".xml"), []byte(b.String()), 0o644); err != nil {
			return counts, err
		}
	}
	return counts, nil
}

// judgesGo is the go a judges control gives the check script (GO). It names its directory's parent
// as its GOROOT, so the script puts it first on PATH; builds cmd/redact-gate as judgesGate; passes
// a start-up run (-run '^$'); and answers each other go test with the recorded events of its
// package and that package's recorded exit status.
const judgesGo = `#!/usr/bin/env bash
set -uo pipefail
run=$JUDGES_RUN
case "${1:-}" in
env)
  if [ "${2:-}" = GOROOT ]; then
    echo "$run/goroot"
    exit 0
  fi
  ;;
version)
  echo "go version: the judges controls' stand-in"
  exit 0
  ;;
build)
  out=
  while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then
      out=$2
      shift
    fi
    shift
  done
  if [ -n "$out" ]; then
    cp "$run/gate" "$out" && chmod +x "$out"
    exit $?
  fi
  ;;
test)
  pkg=${!#}
  prev=
  for arg in "$@"; do
    if [ "$prev" = -run ] && [ "$arg" = '^$' ]; then
      echo '{"Action":"start","Package":"the start-up run of the judges stand-in go"}'
      exit 0
    fi
    prev=$arg
  done
  key=${pkg//[^A-Za-z0-9]/_}
  if [ ! -f "$run/events/$key.json" ]; then
    echo "stand-in go: no recorded events for $pkg" >&2
    exit 98
  fi
  cat "$run/events/$key.json"
  exit "$(cat "$run/events/$key.code")"
  ;;
esac
echo "stand-in go: unexpected arguments: $*" >&2
exit 97
`

// judgesGate is the gate judgesGo builds: its plan names each build target the check requires on a
// BOUNDARY api line, and its stages 4 and 5 pass.
const judgesGate = `#!/usr/bin/env bash
case "$*" in
"-stage plan")
  echo "BOUNDARY api linux/amd64 darwin/arm64 linux/arm64 darwin/amd64 (the judges controls' stand-in gate)"
  exit 0
  ;;
"-stage mutate")
  echo "the judges controls' stand-in gate: stages 4 and 5 passed"
  exit 0
  ;;
esac
echo "stand-in gate: unexpected arguments: $*" >&2
exit 97
`

// judgesBun is the bun first on the check script's PATH. It hands each bun -e to the real bun
// (JUDGES_BUN) unless JUDGES_PLANT names that judge, which it tells by the first line of its
// source: the Go-row judge's "const [events, rows, module, steps]", the Bun-row judge's
// "const [rows, ...reports]" and the print judge's "const [events, ...texts]". A plant that took
// effect appends itself to $JUDGES_RUN/fired. bun test writes the file's recorded report where
// --reporter-outfile names, and bun run typecheck passes.
const judgesBun = `#!/usr/bin/env bash
set -uo pipefail
run=$JUDGES_RUN
if [ "${1:-}" = -e ]; then
  judge=
  case "${2:-}" in
  *'const [events, rows, module, steps]'*) judge=go ;;
  *'const [rows, ...reports]'*) judge=bun ;;
  *'const [events, ...texts]'*) judge=print ;;
  esac
  plant=${JUDGES_PLANT:-}
  kind=${plant%%:*}
  target=${plant#*:}
  if [ -z "$judge" ] || [ -z "$plant" ] || [ "${target%%-*}" != "$judge" ]; then
    exec "$JUDGES_BUN" "$@"
  fi
  case "$kind" in
  nostart)
    echo "$plant" >>"$run/fired"
    echo "stand-in bun: the $judge judge did not start (planted)" >&2
    exit 126
    ;;
  late)
    echo "$plant" >>"$run/fired"
    "$JUDGES_BUN" "$@" || exit
    exit 1
    ;;
  short)
    case "$target" in
    go-row) want='^(PASS|FAIL|UNKNOWN) ' skip='^[A-Z]+ step go test ' ;;
    go-step) want='^(PASS|FAIL|UNKNOWN) step go test ' skip= ;;
    bun) want='^(PASS|FAIL) ' skip= ;;
    print) want='^(PRINT |FAIL print-test: )' skip= ;;
    *)
      echo "stand-in bun: no such plant $plant" >&2
      exit 96
      ;;
    esac
    out=$(mktemp "$run/judge.XXXXXX")
    "$JUDGES_BUN" "$@" >"$out"
    status=$?
    last=$(awk -v want="$want" -v skip="$skip" '$0 ~ want && (skip == "" || $0 !~ skip) { last = NR } END { print last + 0 }' "$out")
    if [ "$last" -gt 0 ]; then
      echo "$plant" >>"$run/fired"
    fi
    awk -v last="$last" 'NR != last' "$out"
    exit "$status"
    ;;
  esac
  echo "stand-in bun: no such plant $plant" >&2
  exit 96
fi
if [ "${1:-}" = test ]; then
  outfile=
  file=
  for arg in "$@"; do
    case "$arg" in
    test | --reporter=*) ;;
    --reporter-outfile=*) outfile=${arg#*=} ;;
    *) file=$arg ;;
    esac
  done
  rel=${PWD#"$JUDGES_ROOT"/}/$file
  cp "$run/junit/${rel//[^A-Za-z0-9]/_}.xml" "$outfile" || exit 98
  echo "stand-in bun test $rel: the recorded report"
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = typecheck ]; then
  echo "stand-in bun run typecheck: passed"
  exit 0
fi
echo "stand-in bun: unexpected arguments: $*" >&2
exit 97
`
