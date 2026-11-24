package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// noticeFile is a production file of the probe module's package at pkg. With call set, Notice
// prints its error through the rule (redact.Cause); without it, through err.Error(). The rest
// keeps the two forms alike enough that git reads a move of one into the other as a rename.
func noticeFile(pkg string, call bool) string {
	words := "err.Error()"
	if call {
		words = "redact.Cause(err, url)"
	}
	var b strings.Builder
	b.WriteString("package " + pkg + "\n\n")
	if call {
		b.WriteString("import \"example.com/m/redact\"\n\n")
	}
	b.WriteString("// Notice is the line a failed request prints.\nfunc Notice(err error, url string) string {\n\treturn \"the request failed: \" + " + words + "\n}\n")
	for _, name := range []string{"First", "Second", "Third", "Fourth", "Fifth", "Sixth", "Seventh", "Eighth"} {
		b.WriteString("\n// " + name + " is one of the package's other words, which the move keeps as they are.\nfunc " + name + "() string { return \"" + strings.ToLower(name) + "\" }\n")
	}
	return b.String()
}

// TestTheCallCensusReadsARuleCallInAMovedFile: a change that moves a file (git mv) and adds a call
// of the rule in it gives git a rename, status R, which changed's --diff-filter=AM dropped while
// rename detection was on, so the census listed no call in the file and stage 2 passed with the
// call unbound. With --no-renames the moved file is added whole: the census lists the call, stage
// 2's call census fails it while no row's mutation names it, and passes it once a row's does.
// Control: git, with rename detection on as this test sets it, reads the move as a rename, so
// the plant is the case rename detection hid.
func TestTheCallCensusReadsARuleCallInAMovedFile(t *testing.T) {
	// The test's git reads no config of the box's but its own: rename detection on, as git's
	// default is, so a box whose config turns it off cannot hide the case.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.renames")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GOWORK", "")
	t.Setenv("GOFLAGS", "")
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=probe", "-c", "user.email=probe@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".github", "scripts", "go_build_tags.py"))
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		".github/scripts/go_build_tags.py": string(script),
		"go.work":                          "go 1.27\n\nuse ./m\n",
		"m/go.mod":                         "module example.com/m\n\ngo 1.27\n",
		"m/redact/redact.go":               "package redact\n\n// Cause is the probe's rule: the words of err that print.\nfunc Cause(err error, url string) string { return \"the request failed\" }\n",
		"m/old/notice.go":                  noticeFile("old", false),
	})
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("mv", "m/old", "m/moved")
	writeTree(t, root, map[string]string{"m/moved/notice.go": noticeFile("moved", true)})
	git("add", "-A")
	git("commit", "-q", "-m", "head: the move, and a call of the rule in the moved file")
	head := git("rev-parse", "HEAD")
	if status := git("diff", "--name-status", base, head); !strings.HasPrefix(status, "R") || !strings.Contains(status, "m/moved/notice.go") {
		t.Fatalf("control: git does not read the move as a rename, so the plant is not the case rename detection hid:\n%s", status)
	}

	const moved = "m/moved/notice.go"
	g := &gate{base: base, head: head, snapshot: root, inv: inventory{Gate: gateConfig{Rule: []string{"example.com/m/redact.Cause"}}}}
	c := g.goCensus(root)
	if len(c.Unknown) > 0 {
		t.Fatalf("the census is UNKNOWN: %q", c.Unknown)
	}
	var listed []string
	for _, call := range c.Calls {
		listed = append(listed, call.File+" "+call.Function)
	}
	if len(c.Calls) != 1 || c.Calls[0].File != moved || c.Calls[0].Function != "example.com/m/moved.Notice" {
		t.Fatalf("the census lists the calls %q, want the one call of the rule in the moved file (%s, example.com/m/moved.Notice)", listed, moved)
	}

	unbound := "stage 2 (call census) call " + moved + ":"
	judged := func(rows ...*row) string {
		t.Helper()
		var out bytes.Buffer
		g.out = &out
		ranges, bindErr := map[*row][2]int{}, map[*row]error{}
		for _, r := range rows {
			at, err := g.locate(r, c)
			if err != nil {
				t.Fatalf("row %s does not bind: %v", r.ID, err)
			}
			ranges[r] = at
		}
		g.judge(c, ranges, bindErr)
		return out.String()
	}
	if out := judged(); !strings.Contains(out, unbound) || !strings.Contains(out, "no row's mutation names it") {
		t.Errorf("stage 2 does not fail the moved file's call while no row's mutation names it; its lines:\n%s", out)
	}
	bound := &row{ID: "moved-notice", Function: "example.com/m/moved.Notice",
		Mutation: &mutation{File: moved, Preimage: "redact.Cause(err, url)", Replacement: "err.Error()"}}
	if out := judged(bound); strings.Contains(out, unbound) {
		t.Errorf("stage 2 still fails the moved file's call with row %s's mutation naming it; its lines:\n%s", bound.ID, out)
	}
}
