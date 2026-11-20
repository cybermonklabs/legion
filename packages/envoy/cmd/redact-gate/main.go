// Command redact-gate is the mutation gate and the two censuses of the check c-redact-remote-text
// (packages/envoy/scripts/check-redact-remote-text.sh runs it). It reads the inventory
// packages/envoy/internal/redact/testdata/redact-check.json and works on one pinned snapshot: the
// change's base (the merge base of HEAD and the inventory's gate.base) and head (HEAD, or -head),
// both printed, and a worktree of that head, which the censuses, the baseline runs and every
// mutation copy read. A change the working tree holds and head does not is not what it checks.
//
// Its stages run in order, and the first that fails stops the run and names itself:
//
//  1. inventory: the inventory parses, holds rows, and holds print-site rows;
//  2. census: the censuses parse and resolve every production file of the repository's Go
//     modules and TypeScript packages (UNKNOWN otherwise). The call census lists every call of
//     the rule the change adds, each of which a row's mutation must name (census.go, tscensus.go,
//     scripts/redact-gate/ts-census.ts). The boundary census reads the libraries' own exported
//     API and fails on any call of golang.org/x/oauth2, go-oidc, google.golang.org/api/impersonate
//     or cloud.google.com/go/cloudsqlconn that is not across redact.OAuth2 or oauth2call.Call, on a
//     call of a text-only library not read under a CallScope, and on a client or token source
//     handed a library that the boundary did not make (boundary.go). The field census lists every
//     field the library boundary sets, each a closed value or the rule's reading (fields.go);
//  3. binding: each mutation's function is in its file and its preimage occurs there once;
//  4. baseline: each row's test passes unmutated in its scratch copy (the check script runs the
//     normal verification between stage 3 and this one);
//  5. mutation: each row's mutation, applied alone, makes its test fail at its designated
//     assertion with a marker in the output the test captured there.
//
// -stage plan runs stages 1-3; -stage mutate runs 1-3 again and then 4 and 5. -report prints
// the censuses' findings without judging them against rows. -controls runs the check's negative
// controls (controls.go).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const module = "github.com/sjawhar/envoy"

// markers are the made-up credentials a row's evidence is one of in captured output: the probe's;
// -RDX-, which each credential the proxy-echo rows choose holds (internal/brokerclient,
// internal/broker/brokertest and loadtest's proxy_echo and answertext tests); and eyJhbGci, the
// start of every JWS, which brokertest's rows mint per request with a test key and its gateway
// echoes.
var markers = []string{"not-a-secret-user", "not-a-secret-password", "not-a-secret-token", "-RDX-", "eyJhbGci"}

type inventory struct {
	Gate   gateConfig    `json:"gate"`
	Go     []row         `json:"go"`
	Bun    []row         `json:"bun"`
	Recall []recallEntry `json:"recall"`
}

// recallEntry is one site an earlier review or control run of the change found, with the guard
// that holds it at the head: a mutation, leak or print-site row's id, or a holder's print-test
// row (holder). Note says why a site that is no leak is in the list.
type recallEntry struct {
	Site   string `json:"site"`
	Source string `json:"source"`
	Guard  string `json:"guard"`
	Holder string `json:"holder"`
	Note   string `json:"note"`
}

// leak is a leak row and where its leak is: control (19) applies the row's test, with the head's
// Files copied in, to Base, and requires it to fail at REDACT-ASSERT Capture with a marker in the
// output captured there. Base is a commit's full 40-character name, which a clone of origin can
// fetch where it lacks the commit (earlierTree), so origin must hold a ref that reaches it.
type leak struct {
	Row     string   `json:"row"`
	Base    string   `json:"base"`
	Files   []string `json:"files"`
	Capture string   `json:"capture"`
}

// boxConfig names the box and what may read its storage: the rule's functions its text may be the
// argument of, and the comparison functions it may be an operand of.
type boxConfig struct {
	Type        string   `json:"type"`  // the box's Go type, pkg path and name
	Field       string   `json:"field"` // its storage field
	Rule        []string `json:"rule"`
	Comparisons []string `json:"comparisons"`
	Parses      []string `json:"parses"`    // functions that parse the text, whose error must only be compared with nil
	TSParses    []string `json:"ts_parses"` // TypeScript calls and constructors that parse the text and throw no text
	TSFile      string   `json:"ts_file"`   // the TypeScript rule's module
	TSClass     string   `json:"ts_class"`  // the TypeScript box's class
	TSRule      []string `json:"ts_rule"`   // names of the TS rule's functions
}

// holderRow is a holder and the print test that covers it. Holder is the holder census's name for
// it: a package path and type name (a generic instance's with its type arguments, as
// types.TypeString writes them), a function and the anonymous struct it declares, or a
// TypeScript file#name.
type holderRow struct {
	Holder string `json:"holder"`
	Test   string `json:"test"` // the print test's row id
}

// reflectionExempt is a reflection or unsafe site the reflection census allows, with the types it
// reaches, which the census checks hold no box storage and no external error type.
type reflectionExempt struct {
	File     string   `json:"file"`
	Function string   `json:"function"`
	Operands []string `json:"operands"`
	Why      string   `json:"why"`
}

type gateConfig struct {
	Base        string             `json:"base"`
	Rule        []string           `json:"rule"`
	Printers    []string           `json:"printers"`
	TSRule      []string           `json:"ts_rule"`
	ExemptFiles []string           `json:"exempt_files"`
	Box         boxConfig          `json:"box"`
	Holders     []holderRow        `json:"holders"`
	Reflection  []reflectionExempt `json:"reflection"`
	External    []string           `json:"external_errors"` // Go types a reflection exemption may not reach
	Exempt      []struct {
		File     string `json:"file"`
		Function string `json:"function"`
		Why      string `json:"why"`
	} `json:"exempt"`
	RowTimeout     string           `json:"row_timeout"`
	Leaks          []leak           `json:"leaks"`
	Required       []string         `json:"required"` // the row ids the check requires the inventory to hold
	BoundaryFields fieldsConfig     `json:"boundary_fields"`
	BoundaryExempt []boundaryExempt `json:"boundary_exempt"`
}

// boundaryExempt is a library call the boundary census lists outside the boundary that cannot
// answer a library's error raw, in one file, with why: an interface method a library type
// implements, called where the census's other rules hold every such library value, or where
// nothing it answers prints.
type boundaryExempt struct {
	File   string `json:"file"`
	Callee string `json:"callee"`
	Why    string `json:"why"`
}

type row struct {
	ID          string    `json:"id"`
	Requirement string    `json:"requirement"`
	Package     string    `json:"package"`
	File        string    `json:"file"`
	Test        string    `json:"test"`
	Production  string    `json:"production"`
	Function    string    `json:"function"`
	Capture     string    `json:"capture"`
	Mutation    *mutation `json:"mutation"`
	bun         bool
}

type mutation struct {
	File        string `json:"file"`
	Preimage    string `json:"preimage"`
	Replacement string `json:"replacement"`
}

// entry is one rule reference the call census listed.
type entry struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Offset   int    `json:"offset"`
	Callee   string `json:"callee"`
	Function string `json:"function"`
}

// census is what both languages' censuses answer.
type census struct {
	Calls     []entry                      `json:"calls"`
	Box       []finding                    `json:"box"`
	Holders   []finding                    `json:"holders"`
	Reflect   []finding                    `json:"reflect"`
	Whole     []finding                    `json:"whole"`
	Boundary  []finding                    `json:"boundary"`
	Fields    []finding                    `json:"fields"`
	Unknown   []string                     `json:"unknown"`
	Functions map[string]map[string][2]int `json:"functions"` // file -> function -> [start, end)
}

// finding is one thing a structural census listed: a use of the box's storage (Box), a type that
// holds the box (Holders), a call of reflect's struct-field access or an unsafe pointer
// conversion (Reflect), or a use of an external error's value (Whole, wholeuse.go). OK is the
// census's own classification; Why says what it is, or why it fails.
type finding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
	What     string `json:"what"`
	Kind     string `json:"kind"`
	OK       bool   `json:"ok"`
	Why      string `json:"why"`
}

type gate struct {
	repo, inventoryPath, work string
	inv                       inventory
	base, head                string
	snapshot                  string
	rowTimeout                time.Duration
	longest                   time.Duration // the longest correct run of stage 4
	longestRow                string
	valueOf                   map[string][]valueOfSite // reflect.ValueOf sites by package
	failures                  int
	out                       io.Writer
}

func main() {
	repo := flag.String("repo", "", "the repository root (default: git's toplevel)")
	inventoryPath := flag.String("inventory", "", "the inventory (default: the repository's)")
	rowsFlag := flag.String("rows", "", "comma-separated row ids for stages 4 and 5 (default: every row with a mutation)")
	stage := flag.String("stage", "mutate", "plan (stages 1-3) or mutate (stages 1-5)")
	head := flag.String("head", "", "the head revision to pin (default: HEAD)")
	base := flag.String("base", "", "the base revision (default: the merge base of head and gate.base)")
	report := flag.Bool("report", false, "print the censuses' findings without judging them against rows")
	controlsFlag := flag.Bool("controls", false, "run the check's negative controls")
	only := flag.String("only", "", "with -controls, the comma-separated controls to run")
	flag.Parse()
	if *repo == "" {
		top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			fmt.Println("redact-gate: not in a git repository")
			os.Exit(2)
		}
		*repo = strings.TrimSpace(string(top))
	}
	if *inventoryPath == "" {
		*inventoryPath = filepath.Join(*repo, "packages/envoy/internal/redact/testdata/redact-check.json")
	}
	if why := shimGo(); why != "" {
		fmt.Println("redact-gate:", why)
		os.Exit(2)
	}
	if *controlsFlag {
		os.Exit(runControls(*repo, *inventoryPath, *only))
	}
	o := options{rows: *rowsFlag, stage: *stage, head: *head, base: *base, report: *report}
	os.Exit(run(*repo, *inventoryPath, o, os.Stdout))
}

// shimGo says why the go on PATH, which every go command the gate starts runs, is a version
// manager's shim, or answers "". A shim can put another PATH, and so another `gh` or `go`, back
// in front of the one the caller chose (check-redact-remote-text.sh puts its GO first).
func shimGo() string {
	path, err := exec.LookPath("go")
	if err != nil {
		return "no go on PATH"
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Sprintf("the go on PATH, %s: %v", path, err)
	}
	if strings.Contains(real, "/shims/") || strings.HasSuffix(real, "/bin/mise") {
		return fmt.Sprintf("the go on PATH, %s, is a shim (%s); put the toolchain's own go first", path, real)
	}
	return ""
}

type options struct {
	rows, stage, head, base string
	report                  bool
}

// run is one gate run. It answers the exit status: 0 passed, 1 a stage failed, 2 the inventory
// was refused.
func run(repo, inventoryPath string, o options, out io.Writer) int {
	g := &gate{repo: repo, inventoryPath: inventoryPath, out: out}
	if err := g.load(!o.report); err != nil {
		fmt.Fprintf(out, "FAIL stage 1 (inventory): %v\n", err)
		return 2
	}
	fmt.Fprintln(out, "PASS stage 1 (inventory)")
	work, err := os.MkdirTemp(os.Getenv("TMPDIR"), "redact-gate.")
	if err != nil {
		fmt.Fprintf(out, "FAIL stage 2 (census): scratch: %v\n", err)
		return 1
	}
	g.work = work
	defer os.RemoveAll(work)
	if err := g.pin(o.head, o.base); err != nil {
		fmt.Fprintf(out, "FAIL stage 2 (census): the snapshot: %v\n", err)
		return 1
	}
	defer g.git(g.repo, "worktree", "remove", "--force", g.snapshot)
	start := time.Now()
	// The TypeScript census resolves the workspace's imports through node_modules.
	if err := g.bunInstall(g.snapshot); err != nil {
		g.fail("stage 2 (census) UNKNOWN the TypeScript workspace: %v", err)
		return g.stop(2, "census")
	}
	c := g.census()
	if o.report {
		g.report(c)
		return 0
	}
	rows := g.mutationRows("")
	ranges := map[*row][2]int{}
	bindErr := map[*row]error{}
	for _, r := range rows {
		if at, err := g.locate(r, c); err != nil {
			bindErr[r] = err
		} else {
			ranges[r] = at
		}
	}
	if !g.judge(c, ranges, bindErr) {
		return g.stop(2, "census")
	}
	fmt.Fprintf(out, "PASS stage 2 (census): %d calls, %d box-storage uses, %d holders, %d reflection sites, %d whole-value uses, %d boundary findings, %d boundary fields, in %s\n",
		len(c.Calls), len(c.Box), len(c.Holders), len(c.Reflect), len(c.Whole), len(c.Boundary), len(c.Fields), time.Since(start).Round(time.Second))
	// What the censuses found, each with its classification: part of what the check prints as its
	// bound.
	g.report(c)
	for _, r := range rows {
		if err := bindErr[r]; err != nil {
			g.fail("stage 3 (binding) row %s: %v", r.ID, err)
		}
	}
	if g.failures > 0 {
		return g.stop(3, "binding")
	}
	fmt.Fprintf(out, "PASS stage 3 (binding): %d mutations bound\n", len(rows))
	if o.stage == "plan" {
		return 0
	}
	gated := g.mutationRows(o.rows)
	if err := g.install(gated); err != nil {
		g.fail("stage 4 (baseline): %v", err)
		return g.stop(4, "baseline")
	}
	baselines := map[string]error{}
	for _, r := range gated {
		g.baseline(r, baselines)
	}
	if g.failures > 0 {
		return g.stop(4, "baseline")
	}
	fmt.Fprintf(out, "PASS stage 4 (baseline): %d rows' tests pass unmutated; the longest correct run is %s (row %s), and row_timeout is %s\n", len(gated), g.longest.Round(time.Second), g.longestRow, g.rowTimeout)
	for _, r := range gated {
		g.gateRow(r, ranges[r])
	}
	if g.failures > 0 {
		return g.stop(5, "mutation")
	}
	fmt.Fprintf(out, "PASS stage 5 (mutation): %d mutants caught; the gate ran %s\n", len(gated), time.Since(start).Round(time.Second))
	return 0
}

func (g *gate) stop(stage int, name string) int {
	fmt.Fprintf(g.out, "gate: stopped at stage %d (%s), %d failures\n", stage, name, g.failures)
	return 1
}

func (g *gate) fail(format string, args ...any) {
	g.failures++
	fmt.Fprintf(g.out, "FAIL "+format+"\n", args...)
}

func (g *gate) load(needSites bool) error {
	data, err := os.ReadFile(g.inventoryPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &g.inv); err != nil {
		return fmt.Errorf("the inventory cannot be parsed: %v", err)
	}
	for i := range g.inv.Bun {
		g.inv.Bun[i].bun = true
	}
	if g.inv.Gate.Base == "" || len(g.inv.Gate.Rule) == 0 {
		return errors.New("the inventory names no gate.base or gate.rule")
	}
	g.rowTimeout = 10 * time.Minute
	if g.inv.Gate.RowTimeout != "" {
		d, err := time.ParseDuration(g.inv.Gate.RowTimeout)
		if err != nil {
			return fmt.Errorf("gate.row_timeout: %v", err)
		}
		g.rowTimeout = d
	}
	if !needSites {
		return nil
	}
	if len(g.inv.Go)+len(g.inv.Bun) == 0 {
		return errors.New("the inventory holds no row")
	}
	sites := 0
	for _, r := range g.allRows() {
		if r.Mutation != nil {
			sites++
			if r.Function == "" || r.Capture == "" || r.Mutation.File == "" || r.Mutation.Preimage == "" {
				return fmt.Errorf("print-site row %s lacks its function, capture, file or preimage", r.ID)
			}
		}
	}
	if sites == 0 {
		return errors.New("the inventory holds no print-site row")
	}
	ids := map[string]bool{}
	for _, r := range g.allRows() {
		ids[r.ID] = true
	}
	for _, id := range g.inv.Gate.Required {
		if !ids[id] {
			return fmt.Errorf("the inventory holds no row %s, which the check requires", id)
		}
	}
	for _, h := range g.inv.Gate.Holders {
		if !ids[h.Test] {
			return fmt.Errorf("holder %s: its print-test row %s is no row of the inventory", h.Holder, h.Test)
		}
	}
	for _, l := range g.inv.Gate.Leaks {
		if !ids[l.Row] || l.Base == "" || len(l.Files) == 0 || l.Capture == "" {
			return fmt.Errorf("leak %s: its row is no row of the inventory, or it names no base, file or capture", l.Row)
		}
		if len(l.Base) != 40 || strings.Trim(l.Base, "0123456789abcdef") != "" {
			return fmt.Errorf("leak %s: its base %q is not a commit's full 40-character name, which a clone that lacks it cannot fetch", l.Row, l.Base)
		}
	}
	if len(g.inv.Recall) == 0 {
		return errors.New("the inventory holds no recall list")
	}
	for _, r := range g.inv.Recall {
		switch {
		case r.Site == "" || r.Source == "":
			return fmt.Errorf("recall entry %q names no site or no source", r.Site)
		case (r.Guard == "") == (r.Holder == ""):
			return fmt.Errorf("recall entry %q: its guard is not one row or one holder", r.Site)
		case r.Guard != "" && !ids[r.Guard]:
			return fmt.Errorf("recall entry %q: its guard, the row %s, is no row of the inventory", r.Site, r.Guard)
		}
	}
	return nil
}

func (g *gate) allRows() []*row {
	var rows []*row
	for i := range g.inv.Go {
		rows = append(rows, &g.inv.Go[i])
	}
	for i := range g.inv.Bun {
		rows = append(rows, &g.inv.Bun[i])
	}
	return rows
}

func (g *gate) mutationRows(only string) []*row {
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		if id != "" {
			want[id] = true
		}
	}
	var rows []*row
	for _, r := range g.allRows() {
		if r.Mutation != nil && (len(want) == 0 || want[r.ID]) {
			rows = append(rows, r)
		}
	}
	return rows
}

func (g *gate) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// pin records the base and head and checks out head in a worktree of its own.
func (g *gate) pin(head, base string) error {
	if head == "" {
		head = "HEAD"
	}
	head, err := g.git(g.repo, "rev-parse", head+"^{commit}")
	if err != nil {
		return err
	}
	if base == "" {
		if base, err = g.git(g.repo, "merge-base", head, g.inv.Gate.Base); err != nil {
			return fmt.Errorf("the base %s does not resolve: %v", g.inv.Gate.Base, err)
		}
	} else if base, err = g.git(g.repo, "rev-parse", base+"^{commit}"); err != nil {
		return err
	}
	g.base, g.head = base, head
	fmt.Fprintf(g.out, "gate: base %s, head %s\n", base, head)
	if current, _ := g.git(g.repo, "rev-parse", "HEAD"); current == head {
		if dirty, _ := g.git(g.repo, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
			fmt.Fprintln(g.out, "gate: the working tree differs from head; the gate reads head")
		}
	}
	g.snapshot = filepath.Join(g.work, "snapshot")
	_, err = g.git(g.repo, "worktree", "add", "--detach", g.snapshot, head)
	return err
}

// install readies the snapshot for Bun rows: their copies need the workspace's node_modules.
func (g *gate) install(rows []*row) error {
	for _, r := range rows {
		if r.bun {
			return g.bunInstall(g.snapshot)
		}
	}
	return nil
}

func (g *gate) bunInstall(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		return nil
	}
	cmd := exec.Command("bun", "install", "--frozen-lockfile")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bun install in %s: %v: %s", dir, err, lastLines(string(out), 5))
	}
	return nil
}

// changed is each file of the change that match accepts, with the head's line numbers the
// change added or changed.
func (g *gate) changed(dir string, match func(string) bool) (map[string]map[int]bool, error) {
	names, err := g.git(dir, "diff", "--name-only", "--diff-filter=AM", g.base, g.head)
	if err != nil {
		return nil, err
	}
	files := map[string]map[int]bool{}
	hunk := regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)
	for _, name := range strings.Split(names, "\n") {
		if name == "" || !match(name) || g.exemptFile(name) {
			continue
		}
		diff, err := g.git(dir, "diff", "-U0", g.base, g.head, "--", name)
		if err != nil {
			return nil, err
		}
		lines := map[int]bool{}
		for _, l := range strings.Split(diff, "\n") {
			if m := hunk.FindStringSubmatch(l); m != nil {
				start, _ := strconv.Atoi(m[1])
				count := 1
				if m[2] != "" {
					count, _ = strconv.Atoi(m[2])
				}
				for i := range count {
					lines[start+i] = true
				}
			}
		}
		files[name] = lines
	}
	return files, nil
}

func (g *gate) exemptFile(name string) bool {
	for _, f := range g.inv.Gate.ExemptFiles {
		if f == name {
			return true
		}
	}
	return false
}

func (g *gate) exempt(file, function string) string {
	for _, e := range g.inv.Gate.Exempt {
		if e.File == file && e.Function == function {
			return e.Why
		}
	}
	return ""
}

// census runs both languages' censuses on the snapshot.
func (g *gate) census() census {
	c := census{Functions: map[string]map[string][2]int{}}
	for _, part := range []census{g.goCensus(g.snapshot), g.tsCensus(g.snapshot), g.tsStructural(g.snapshot), g.wholeUse(g.snapshot), g.boundary(g.snapshot)} {
		c.Calls = append(c.Calls, part.Calls...)
		c.Box = append(c.Box, part.Box...)
		c.Holders = append(c.Holders, part.Holders...)
		c.Reflect = append(c.Reflect, part.Reflect...)
		c.Whole = append(c.Whole, part.Whole...)
		c.Boundary = append(c.Boundary, part.Boundary...)
		c.Fields = append(c.Fields, part.Fields...)
		c.Unknown = append(c.Unknown, part.Unknown...)
		for f, fns := range part.Functions {
			c.Functions[f] = fns
		}
	}
	return c
}

// covers is the row whose mutation names the reference at offset in file's function, "pending"
// when a row binds that function but its preimage did not bind (stage 3 fails it), or "".
func covers(ranges map[*row][2]int, bindErr map[*row]error, file, function string, offset int) string {
	for r, at := range ranges {
		if r.Mutation.File == file && r.Function == function && at[0] <= offset && offset < at[1] {
			return r.ID
		}
	}
	for r := range bindErr {
		if r.Mutation.File == file && r.Function == function {
			return "pending"
		}
	}
	return ""
}

// judge fails each UNKNOWN, an empty call census and each rule call no row's mutation names. It
// answers whether the census stage passed.
func (g *gate) judge(c census, ranges map[*row][2]int, bindErr map[*row]error) bool {
	before := g.failures
	for _, u := range c.Unknown {
		g.fail("stage 2 (census) UNKNOWN %s", u)
	}
	if len(c.Calls) == 0 {
		g.fail("stage 2 (call census) UNKNOWN: it listed no call of the rule")
	}
	for _, call := range c.Calls {
		if why := g.exempt(call.File, call.Function); why != "" {
			fmt.Fprintf(g.out, "EXEMPT call %s:%d %s in %s: %s\n", call.File, call.Line, call.Callee, call.Function, why)
			continue
		}
		if covers(ranges, bindErr, call.File, call.Function, call.Offset) == "" {
			g.fail("stage 2 (call census) call %s:%d %s in %s: no row's mutation names it", call.File, call.Line, call.Callee, call.Function)
		}
	}
	g.judgeStructural(c)
	return g.failures == before
}

// judgeStructural fails each box-storage use the box census could not classify, each holder no
// print-test row names, each reflection or unsafe site it does not allow, and a census that
// listed nothing.
func (g *gate) judgeStructural(c census) {
	if len(c.Box) == 0 {
		g.fail("stage 2 (box census) UNKNOWN: it listed no use of the box's storage")
	}
	if len(c.Holders) == 0 {
		g.fail("stage 2 (holder census) UNKNOWN: it listed no holder")
	}
	if len(c.Reflect) == 0 {
		g.fail("stage 2 (reflection census) UNKNOWN: it listed no reflection or unsafe site")
	}
	for _, b := range c.Box {
		if !b.OK {
			g.fail("stage 2 (box census) %s:%d %s in %s: %s", b.File, b.Line, b.What, b.Function, b.Why)
		}
	}
	rows := map[string]bool{}
	for _, h := range g.inv.Gate.Holders {
		rows[h.Holder] = true
	}
	found := map[string]bool{}
	for _, h := range c.Holders {
		found[h.What] = true
		if !rows[h.What] {
			g.fail("stage 2 (holder census) holder %s (%s:%d): no print-test row names it", h.What, h.File, h.Line)
		}
	}
	for _, r := range g.inv.Recall {
		if r.Holder != "" && !found[r.Holder] {
			g.fail("stage 2 (holder census) recall entry %q: its guard, the holder %s, is not a holder at the head", r.Site, r.Holder)
		}
	}
	for _, r := range c.Reflect {
		if !r.OK {
			g.fail("stage 2 (reflection census) %s:%d %s in %s: %s", r.File, r.Line, r.What, r.Function, r.Why)
		}
	}
	if len(c.Whole) == 0 {
		g.fail("stage 2 (whole-value census) UNKNOWN: it listed no use of an external error")
	}
	for _, w := range c.Whole {
		if !w.OK {
			g.fail("stage 2 (whole-value census) %s:%d %s (%s): %s", w.File, w.Line, w.What, w.Function, w.Why)
		}
	}
	calls := 0
	used := map[boundaryExempt]bool{}
	for _, b := range c.Boundary {
		if b.Kind == "call" {
			calls++
		}
		if b.OK {
			continue
		}
		exempt := false
		for _, e := range g.inv.Gate.BoundaryExempt {
			if e.File == b.File && e.Callee == b.What {
				exempt, used[e] = true, true
				fmt.Fprintf(g.out, "EXEMPT boundary %s:%d %s: %s\n", b.File, b.Line, b.What, e.Why)
			}
		}
		if !exempt {
			g.fail("stage 2 (boundary census) %s:%d %s: %s", b.File, b.Line, b.What, b.Why)
		}
	}
	for _, e := range g.inv.Gate.BoundaryExempt {
		if !used[e] {
			g.fail("stage 2 (boundary census) the exemption of %s in %s matches no call the census failed", e.Callee, e.File)
		}
	}
	if calls == 0 {
		g.fail("stage 2 (boundary census) UNKNOWN: it listed no call of a library")
	}
	if len(c.Fields) == 0 {
		g.fail("stage 2 (field census) UNKNOWN: it listed no field the boundary sets")
	}
	for _, f := range c.Fields {
		if !f.OK {
			g.fail("stage 2 (field census) %s:%d %s in %s: %s", f.File, f.Line, f.What, f.Function, f.Why)
		}
	}
}

// report prints every call the census listed, and each UNKNOWN, unjudged.
func (g *gate) report(c census) {
	for _, u := range c.Unknown {
		fmt.Fprintf(g.out, "UNKNOWN %s\n", u)
	}
	sort.Slice(c.Calls, func(i, j int) bool {
		return c.Calls[i].File+fmt.Sprint(c.Calls[i].Offset) < c.Calls[j].File+fmt.Sprint(c.Calls[j].Offset)
	})
	for _, call := range c.Calls {
		fmt.Fprintf(g.out, "CALL %s:%d %s in %s\n", call.File, call.Line, call.Callee, call.Function)
	}
	for _, b := range c.Box {
		fmt.Fprintf(g.out, "BOX %s:%d %s in %s: %s ok=%t %s\n", b.File, b.Line, b.What, b.Function, b.Kind, b.OK, b.Why)
	}
	for _, h := range c.Holders {
		fmt.Fprintf(g.out, "HOLDER %s (%s:%d)\n", h.What, h.File, h.Line)
	}
	for _, r := range c.Reflect {
		fmt.Fprintf(g.out, "REFLECT %s:%d %s in %s: ok=%t %s\n", r.File, r.Line, r.What, r.Function, r.OK, r.Why)
	}
	for _, w := range c.Whole {
		fmt.Fprintf(g.out, "WHOLE %s %s:%d %s (%s): ok=%t %s\n", w.Kind, w.File, w.Line, w.What, w.Function, w.OK, w.Why)
	}
	for _, b := range c.Boundary {
		if b.Kind == "api" {
			fmt.Fprintf(g.out, "BOUNDARY api %s (%s)\n", b.What, b.Why)
			continue
		}
		fmt.Fprintf(g.out, "BOUNDARY call %s:%d %s: ok=%t %s\n", b.File, b.Line, b.What, b.OK, b.Why)
	}
	for _, f := range c.Fields {
		fmt.Fprintf(g.out, "FIELD %s:%d %s in %s: ok=%t %s\n", f.File, f.Line, f.What, f.Function, f.OK, f.Why)
	}
}

// locate is the byte range in the snapshot of the row's preimage, which must occur exactly once
// in the row's function.
func (g *gate) locate(r *row, c census) ([2]int, error) {
	data, err := os.ReadFile(filepath.Join(g.snapshot, r.Mutation.File))
	if err != nil {
		return [2]int{}, fmt.Errorf("its file %s cannot be read: %v", r.Mutation.File, err)
	}
	var fn [2]int
	var ok bool
	if strings.HasSuffix(r.Mutation.File, ".go") {
		fn, ok = goFunction(data, r.Mutation.File, r.Function)
	} else {
		fn, ok = c.Functions[r.Mutation.File][r.Function]
	}
	if !ok {
		return [2]int{}, fmt.Errorf("its function %s is not in %s", r.Function, r.Mutation.File)
	}
	body := string(data[fn[0]:fn[1]])
	if n := strings.Count(body, r.Mutation.Preimage); n != 1 {
		return [2]int{}, fmt.Errorf("its preimage %q occurs %d times in %s, not once", r.Mutation.Preimage, n, r.Function)
	}
	at := fn[0] + strings.Index(body, r.Mutation.Preimage)
	return [2]int{at, at + len(r.Mutation.Preimage)}, nil
}

// goFunction is the byte range of the function named name (types.Func.FullName's form) in a Go
// file, found by syntax alone.
func goFunction(data []byte, file, name string) ([2]int, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, data, parser.SkipObjectResolution)
	if err != nil {
		return [2]int{}, false
	}
	path := goImportPath(file)
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && goFuncName(path, fd) == name {
			return [2]int{fset.Position(fd.Pos()).Offset, fset.Position(fd.End()).Offset}, true
		}
	}
	return [2]int{}, false
}

// goImportPath is the import path of the package a file of packages/envoy is in.
func goImportPath(file string) string {
	dir := filepath.Dir(strings.TrimPrefix(file, "packages/envoy/"))
	if dir == "." {
		return module
	}
	return module + "/" + filepath.ToSlash(dir)
}

// goFuncName is types.Func.FullName of fd, from syntax: "pkg.F", "(pkg.T).M" or "(*pkg.T).M".
func goFuncName(path string, fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return path + "." + fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	star := ""
	if s, ok := t.(*ast.StarExpr); ok {
		star, t = "*", s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if ix, ok := t.(*ast.IndexListExpr); ok {
		t = ix.X
	}
	id, _ := t.(*ast.Ident)
	if id == nil {
		return path + ".?." + fd.Name.Name
	}
	return "(" + star + path + "." + id.Name + ")." + fd.Name.Name
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}
