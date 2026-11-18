package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// control is one negative control of the check: a fault planted in a scratch worktree of the
// head, committed there, and the stage the gate must reject it at, naming expect, or, with each,
// naming every one of several sets. A control with own runs its own evidence instead of the
// gate's stages (controls 16 and 19, which run a row's test on an earlier tree).
type control struct {
	id, what string
	plant    func(c *controlRun) error
	rows     string // the rows stages 4 and 5 run, "" for every row
	stage    int    // the stage the gate must stop at
	expect   []string
	each     [][]string
	fixture  bool // control (1): the fixture's own tests are its evidence, not the gate's stages
	own      func(repo string, inv inventory) (verdict, output string)
}

type controlRun struct {
	wt, inventory string
	inv           map[string]any
}

// edit replaces the one occurrence of old in file (relative to the worktree) with new.
func (c *controlRun) edit(file, old, new string) error {
	path := filepath.Join(c.wt, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if n := strings.Count(string(data), old); n != 1 {
		return fmt.Errorf("the control's anchor %q occurs %d times in %s, not once", old, n, file)
	}
	return os.WriteFile(path, []byte(strings.Replace(string(data), old, new, 1)), 0o644)
}

// replaceAll replaces every occurrence of old in file, which must hold at least one.
func (c *controlRun) replaceAll(file, old, new string) error {
	path := filepath.Join(c.wt, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), old) {
		return fmt.Errorf("the control's anchor %q is not in %s", old, file)
	}
	return os.WriteFile(path, []byte(strings.ReplaceAll(string(data), old, new)), 0o644)
}

// write makes file (relative to the worktree) with text, a planted file of the control's own.
func (c *controlRun) write(file, text string) error {
	return os.WriteFile(filepath.Join(c.wt, file), []byte(text), 0o644)
}

func (c *controlRun) appendTo(file, text string) error {
	f, err := os.OpenFile(filepath.Join(c.wt, file), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// row is the inventory row id, as the control edits it.
func (c *controlRun) row(id string) map[string]any {
	for _, kind := range []string{"go", "bun"} {
		rows, _ := c.inv[kind].([]any)
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok && m["id"] == id {
				return m
			}
		}
	}
	return nil
}

func (c *controlRun) writeInventory() error {
	data, err := json.MarshalIndent(c.inv, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.wt, c.inventory), data, 0o644)
}

const (
	inventoryFile = "packages/envoy/internal/redact/testdata/redact-check.json"
	// controlRow is the mutation row the evidence controls use: its test's designated assertion
	// is redacttest.RequireNone, which they fault.
	controlRow     = "box-send-back"
	controlRowTest = "packages/envoy/internal/dispatch/envoy/client_test.go"
	boxFile        = "packages/envoy/internal/redact/remotetext.go"
	boxTSFile      = "packages/contracts/src/remote-text.ts"
	printTable     = "packages/envoy/internal/redact/redacttest/printtable.go"
	// githubRow is the GitHub check run's detail row, and githubBase the revision control (16)
	// applies its test to: main before this change, a commit on origin, whose detail quoted GitHub's
	// message raw.
	githubRow  = "results-ci"
	githubBase = "87112a0b71"
)

const (
	brokerclientPkg = "github.com/sjawhar/envoy/internal/brokerclient"
	plantDir        = "packages/envoy/internal/brokerclient/"
	plantTSDir      = "packages/broker-client/src/"
)

// redacttestFault replaces redacttest.RequireNone's failure with body, in the worktree.
func redacttestFault(c *controlRun, capture, failure string) error {
	return c.edit("packages/envoy/internal/redact/redacttest/redacttest.go",
		`	Capture(t, capture, text)
	for _, secret := range Held(text) {
		t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, secret, text)
	}`, capture+"\n"+failure)
}

// setMutation sets row id's recorded preimage and replacement.
func (c *controlRun) setMutation(id, preimage, replacement string) {
	m := c.row(id)["mutation"].(map[string]any)
	m["preimage"], m["replacement"] = preimage, replacement
}

var controls = []control{
	{id: "1", what: "the grammar returns every field unchanged, in Go and in TypeScript", fixture: true, plant: func(c *controlRun) error {
		if err := c.edit("packages/envoy/internal/redact/remote.go", "func Remote(field string) (string, bool) {\n", "func Remote(field string) (string, bool) {\n\tif field != \"\\x00control\" {\n\t\treturn field, true\n\t}\n"); err != nil {
			return err
		}
		return c.edit(boxTSFile, "export function readRemoteText(field: string): string | undefined {\n", "export function readRemoteText(field: string): string | undefined {\n  if (field !== \"\\u0000control\") return field;\n")
	}},
	{id: "2", what: "a new call of the rule, written as a receiver call through a type alias and a method value, at a site no row names", stage: 2,
		expect: []string{"call census", "cmd/agent-secrets-devrelay/main.go", "(github.com/sjawhar/envoy/brokeraddr.Address).Words", "controlAlias"},
		plant: func(c *controlRun) error {
			return c.appendTo("packages/envoy/cmd/agent-secrets-devrelay/main.go", `
// controlAlias is the control's planted call of the rule.
func controlAlias(words string) string {
	type address = brokeraddr.Address
	var a address
	w := a.Words
	return w(words)
}
`)
		}},
	{id: "3a", what: "a new method of the box that returns its text, in Go and in TypeScript", stage: 2,
		each: [][]string{
			{"box census", "(github.com/sjawhar/envoy/internal/redact.RemoteText).Raw"},
			{"box census", "RemoteText.raw"},
		},
		plant: func(c *controlRun) error {
			if err := c.appendTo(boxFile, "\n// Raw is control (3a)'s planted method.\nfunc (t RemoteText) Raw() string {\n\tif t.text == nil {\n\t\treturn \"\"\n\t}\n\treturn *t.text\n}\n"); err != nil {
				return err
			}
			return c.edit(boxTSFile, "  date(): Date | undefined {\n", "  raw(): string {\n    return this.#text;\n  }\n\n  date(): Date | undefined {\n")
		}},
	{id: "3b", what: "a comparison method of the box that logs its storage before returning its bool", stage: 2,
		expect: []string{"box census", "(github.com/sjawhar/envoy/internal/redact.RemoteText).Logged", "which is no rule"},
		plant: func(c *controlRun) error {
			return c.appendTo(boxFile, "\n// Logged is control (3b)'s planted comparison.\nfunc (t RemoteText) Logged(s string) bool {\n\tif t.text == nil {\n\t\treturn s == \"\"\n\t}\n\tprintln(*t.text)\n\treturn *t.text == s\n}\n")
		}},
	{id: "3c", what: "a new function in package redact that reads the box's storage through reflection", stage: 2,
		expect: []string{"reflection census", "packages/envoy/internal/redact/control_reflect.go"},
		plant: func(c *controlRun) error {
			return c.write("packages/envoy/internal/redact/control_reflect.go", "package redact\n\nimport \"reflect\"\n\n// controlReflect is control (3c)'s planted read of the box's storage.\nfunc controlReflect(t RemoteText) string { return reflect.ValueOf(t).Field(0).Elem().String() }\n")
		}},
	{id: "4", what: "the box holds its text by value, with its own matching mutation records", rows: "box-string", stage: 4,
		expect: []string{"baseline", "row box-string", "REDACT-ASSERT print/redact.RemoteText/", "/%v/unexported"},
		plant: func(c *controlRun) error {
			for _, r := range [][2]string{
				{"type RemoteText struct{ text *string }", "type RemoteText struct{ text string }"},
				{"RemoteText{text: &text}", "RemoteText{text: text}"},
				// A nil-or-empty check is one empty check of a value (go test's vet refuses the
				// redundant or the plain replacements would leave).
				{"t.text == nil || *t.text == \"\"", "t.text == \"\""},
				{"t.text == nil", "t.text == \"\""},
				{"*t.text", "t.text"},
			} {
				if err := c.replaceAll(boxFile, r[0], r[1]); err != nil {
					return err
				}
			}
			c.setMutation("box-string", "return Field(t.text)", "return t.text")
			c.setMutation("box-send-back", "shown := Field(t.text)", "shown := t.text")
			c.setMutation("box-reading", "return ruled(t.text)", "return t.text")
			return c.writeInventory()
		}},
	// MarshalJSON, since Format, not GoString, is what fmt calls for every verb.
	{id: "5", what: "one printer of the box, MarshalJSON, writes the literal probe into its output, its storage reads unchanged", rows: "box-string", stage: 4,
		expect: []string{"baseline", "row box-string", "REDACT-ASSERT print/redact.RemoteText/", "/json.Marshal/value"},
		plant: func(c *controlRun) error {
			return c.edit(boxFile, "func (t RemoteText) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }",
				"func (t RemoteText) MarshalJSON() ([]byte, error) { return json.Marshal(t.String() + \"not-a-secret-\" + \"token\") }")
		}},
	{id: "6", what: "new holders reached only through a pointer, a slice, a map, an embedded struct and a TypeScript object type, with no print-test row", stage: 2,
		each: [][]string{
			{"holder census", brokerclientPkg + ".controlViaPointer"},
			{"holder census", brokerclientPkg + ".controlViaSlice"},
			{"holder census", brokerclientPkg + ".controlViaMap"},
			{"holder census", brokerclientPkg + ".controlEmbedded"},
			{"holder census", plantTSDir + "control-holder.ts#ControlHolder"},
		},
		plant: func(c *controlRun) error {
			if err := c.write(plantDir+"control_holders.go", "package brokerclient\n\n// Control (6)'s planted holders, each reaching the box only through another type.\ntype controlViaPointer struct{ r *RefusedError }\n\ntype controlViaSlice struct{ r []tokenAnswer }\n\ntype controlViaMap struct{ r map[string]*RefusedError }\n\ntype controlEmbedded struct{ RefusedError }\n"); err != nil {
				return err
			}
			return c.write(plantTSDir+"control-holder.ts", "import type { BrokerRefusedError } from \"./token-client\";\n\n/** Control (6)'s planted holder, reaching the box through an array of refusals. */\nexport interface ControlHolder {\n  readonly refusals: readonly BrokerRefusedError[];\n}\n")
		}},
	{id: "7", what: "one same-placement row's twin made unreachable at its recorded placement, with no relocation", rows: "box-string", stage: 4,
		expect: []string{"baseline", "row box-string", "blind"},
		plant: func(c *controlRun) error {
			return c.replaceAll(printTable, `"text|json.Marshal|unexported":`, `"text|json.Marshal|unexported": "same", "control-7-was":`)
		}},
	{id: "8", what: "a planted fmt.Sprint and json.Marshal of an *oauth2.RetrieveError, and a template string and JSON.stringify of an openid-client error", stage: 2,
		each: [][]string{
			{"whole-value census", plantDir + "control_whole.go:11"},
			{"whole-value census", plantDir + "control_whole.go:13"},
			{"whole-value census", plantTSDir + "control-whole.ts:5"},
			{"whole-value census", plantTSDir + "control-whole.ts:9"},
		},
		plant: func(c *controlRun) error {
			if err := c.write(plantDir+"control_whole.go", "package brokerclient\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\n\t\"golang.org/x/oauth2\"\n)\n\n// controlSprint and controlMarshal are control (8)'s planted whole uses.\nfunc controlSprint(re *oauth2.RetrieveError) string { return fmt.Sprint(re) }\n\nfunc controlMarshal(re *oauth2.RetrieveError) ([]byte, error) { return json.Marshal(re) }\n"); err != nil {
				return err
			}
			return c.write(plantTSDir+"control-whole.ts", "import type { ResponseBodyError } from \"openid-client\";\n\n/** Control (8)'s planted whole uses of an openid-client error. */\nexport function controlTemplate(error: ResponseBodyError): string {\n  return `${error}`;\n}\n\nexport function controlStringify(error: ResponseBodyError): string {\n  return JSON.stringify(error);\n}\n")
		}},
	{id: "9a", what: "a new production call of reflect's struct-field access outside package redact that the inventory does not name", stage: 2,
		expect: []string{"reflection census", plantDir + "control_reflect.go"},
		plant: func(c *controlRun) error {
			return c.write(plantDir+"control_reflect.go", "package brokerclient\n\nimport \"reflect\"\n\n// controlField is control (9a)'s planted reflection.\nfunc controlField(v any) any { return reflect.ValueOf(v).Field(0).Interface() }\n")
		}},
	{id: "9b", what: "a named reflection call whose operand's possible types include an external error type", stage: 2,
		expect: []string{"reflection census", "controlNamed", "external error type"},
		plant: func(c *controlRun) error {
			if err := c.write(plantDir+"control_reflect_named.go", "package brokerclient\n\nimport (\n\t\"reflect\"\n\n\t\"golang.org/x/oauth2\"\n)\n\n// controlNamed is control (9b)'s planted reflection, which the inventory names.\nfunc controlNamed(re *oauth2.RetrieveError) any { return reflect.ValueOf(re).Elem().Field(0).Interface() }\n"); err != nil {
				return err
			}
			gate := c.inv["gate"].(map[string]any)
			exempt, _ := gate["reflection"].([]any)
			gate["reflection"] = append(exempt, map[string]any{
				"file": plantDir + "control_reflect_named.go", "function": brokerclientPkg + ".controlNamed",
				"operands": []string{"*golang.org/x/oauth2.RetrieveError"}, "why": "control (9b)",
			})
			return c.writeInventory()
		}},
	{id: "10", what: "one file of the change is made unparseable", stage: 2,
		expect: []string{"UNKNOWN", "packages/envoy/internal/dispatch/auth/callback.go", "cannot be parsed"},
		plant: func(c *controlRun) error {
			return c.appendTo("packages/envoy/internal/dispatch/auth/callback.go", "\nfunc {\n")
		}},
	{id: "11", what: "one row's mutation is applied at a site an outer redaction also covers", rows: "boundary-answer-code", stage: 5,
		expect: []string{"row boundary-answer-code", "a surviving mutant"},
		plant: func(c *controlRun) error {
			if err := c.edit("packages/envoy/internal/redact/oauth2.go", "ErrorCode:        ruled(re.ErrorCode),", "ErrorCode:        ruled(ruled(re.ErrorCode)),"); err != nil {
				return err
			}
			gate := c.inv["gate"].(map[string]any)
			exempt, _ := gate["exempt"].([]any)
			gate["exempt"] = append(exempt, map[string]any{"file": "packages/envoy/internal/redact/oauth2.go", "function": "github.com/sjawhar/envoy/internal/redact.newOAuthAnswer", "why": "control (11): the outer redaction it plants"})
			return c.writeInventory()
		}},
	{id: "12a", what: "the row's test fails at an unrelated assertion while its captured output holds a marker", rows: controlRow, stage: 5,
		expect: []string{"row " + controlRow, "the wrong assertion"},
		plant: func(c *controlRun) error {
			return redacttestFault(c, "\tCapture(t, capture, text)", `	if len(Held(text)) > 0 {
		t.Errorf("an unrelated check failed")
	}`)
		}},
	{id: "12b", what: "the row's test fails at its designated assertion with a marker in the message while its captured output holds none", rows: controlRow, stage: 5,
		expect: []string{"row " + controlRow, "no output evidence"},
		plant: func(c *controlRun) error {
			return redacttestFault(c, "\tCapture(t, capture, \"the output, not captured\")", `	for _, secret := range Held(text) {
		t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, secret, text)
	}`)
		}},
	{id: "13a", what: "one row's recorded preimage occurs more than once in its function", stage: 3,
		expect: []string{"binding", "row boundary-answer-code", "occurs 3 times"},
		plant: func(c *controlRun) error {
			c.row("boundary-answer-code")["mutation"].(map[string]any)["preimage"] = "ruled(re.Error"
			return c.writeInventory()
		}},
	{id: "13b", what: "one row's recorded preimage does not occur in its function", stage: 3,
		expect: []string{"binding", "row " + controlRow, "occurs 0 times"},
		plant: func(c *controlRun) error {
			c.row(controlRow)["mutation"].(map[string]any)["preimage"] = "redact.Field(*e.nothing)"
			return c.writeInventory()
		}},
	{id: "14", what: "one inventory test is renamed in the source and not in the inventory", rows: controlRow, stage: 4,
		expect: []string{"baseline", "row " + controlRow, "is missing"},
		plant: func(c *controlRun) error {
			return c.edit(controlRowTest, "func TestARoleRefusalsHolderIsSentBackOnlyAsTheRulePrintsIt(", "func TestARoleRefusalsHolderIsSentBackOnlyAsTheRulePrintsItRenamedByControl(")
		}},
	{id: "15a", what: "an empty inventory", stage: 1, expect: []string{"inventory", "cannot be parsed"},
		plant: func(c *controlRun) error { return os.WriteFile(filepath.Join(c.wt, c.inventory), nil, 0o644) }},
	{id: "15b", what: "an inventory with no print-site row", stage: 1, expect: []string{"inventory", "no print-site row"},
		plant: func(c *controlRun) error {
			for _, kind := range []string{"go", "bun"} {
				rows, _ := c.inv[kind].([]any)
				for _, r := range rows {
					delete(r.(map[string]any), "mutation")
				}
			}
			return c.writeInventory()
		}},
	{id: "15c", what: "an inventory with a recall entry whose guard is missing at the head", stage: 1, expect: []string{"inventory", "recall entry", "control-15c-no-such-row"},
		plant: func(c *controlRun) error {
			recall := c.inv["recall"].([]any)
			entry := recall[0].(map[string]any)
			entry["guard"], entry["holder"] = "control-15c-no-such-row", ""
			return c.writeInventory()
		}},
	{id: "16", what: "the GitHub message row's test, applied to " + githubBase + "'s tree, compiles and fails on a marker in the check run's captured detail",
		own: func(repo string, inv inventory) (string, string) {
			// The head's redacttest goes in with the test: the tree's own prints no REDACT-CAPTURE or
			// REDACT-ASSERT line, which the evidence reads.
			return earlierTree(repo, inv, githubRow, githubBase, []string{"packages/envoy/internal/dispatch/api/remote_words_test.go", "packages/envoy/internal/redact/redacttest/redacttest.go"}, "api.ci-detail")
		}},
	{id: "18", what: "a planted direct call, outside the boundary function, to an exported function of golang.org/x/oauth2 and to one of openid-client", stage: 2,
		each: [][]string{
			{"boundary census", plantDir + "control_boundary.go:6"},
			{"boundary census", plantTSDir + "control-boundary.ts:4"},
		},
		plant: func(c *controlRun) error {
			if err := c.write(plantDir+"control_boundary.go", "package brokerclient\n\nimport \"golang.org/x/oauth2/google\"\n\n// controlConfig is control (18)'s planted call of x/oauth2 outside the boundary.\nfunc controlConfig() { _, _ = google.ConfigFromJSON(nil) }\n"); err != nil {
				return err
			}
			return c.write(plantTSDir+"control-boundary.ts", "import { randomState } from \"openid-client\";\n\n/** Control (18)'s planted call of openid-client outside callLibrary. */\nexport const controlState = randomState();\n")
		}},
	{id: "19", what: "each leak row, applied to the tree where its leak is, compiles and fails on a marker in its captured output",
		own: func(repo string, inv inventory) (string, string) {
			var failed, out []string
			for _, l := range inv.Gate.Leaks {
				verdict, output := earlierTree(repo, inv, l.Row, l.Base, l.Files, l.Capture)
				out = append(out, fmt.Sprintf("leak %s at %s: %s", l.Row, l.Base, orAccepted(verdict)), output)
				if verdict != "" {
					failed = append(failed, l.Row+": "+verdict)
				}
			}
			if len(inv.Gate.Leaks) == 0 {
				return "the inventory names no leak row", ""
			}
			return strings.Join(failed, "; "), strings.Join(out, "\n")
		}},
}

func orAccepted(verdict string) string {
	if verdict == "" {
		return "fails on a marker, as a leak row must"
	}
	return verdict
}

// earlierTree applies row id's test to rev: a worktree of rev with files copied in from the head,
// the row's test run there alone. It answers "" when the test compiled and failed at REDACT-ASSERT
// capture with a marker in the output captured there, and the reason otherwise; a build or setup
// failure does not count.
func earlierTree(repo string, inv inventory, id, rev string, files []string, capture string) (string, string) {
	g := &gate{repo: repo, inv: inv, rowTimeout: 10 * time.Minute}
	var r *row
	for _, candidate := range g.allRows() {
		if candidate.ID == id {
			r = candidate
		}
	}
	if r == nil {
		return "the inventory holds no row " + id, ""
	}
	work, err := os.MkdirTemp(os.Getenv("TMPDIR"), "redact-earlier.")
	if err != nil {
		return err.Error(), ""
	}
	defer os.RemoveAll(work)
	g.work = work
	wt := filepath.Join(work, "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, rev).CombinedOutput(); err != nil {
		return fmt.Sprintf("the worktree at %s: %v: %s", rev, err, out), ""
	}
	defer exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
	for _, f := range files {
		data, err := exec.Command("git", "-C", repo, "show", "HEAD:"+f).Output()
		if err != nil {
			return fmt.Sprintf("the head's %s: %v", f, err), ""
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, f)), 0o755); err != nil {
			return err.Error(), ""
		}
		if err := os.WriteFile(filepath.Join(wt, f), data, 0o644); err != nil {
			return err.Error(), ""
		}
	}
	if r.bun {
		if err := g.bunInstall(wt); err != nil {
			return "the install at " + rev + ": " + err.Error(), ""
		}
	}
	probe := *r
	probe.Capture = capture
	res := g.runTest(wt, &probe)
	if verdict := res.evidence(&probe); verdict != "" {
		return verdict, res.note
	}
	return "", fmt.Sprintf("the test failed at REDACT-ASSERT %s with a marker in its captured output", capture)
}

// runControls runs each control (or the ones only names) from a passing baseline in a worktree of
// its own, and accepts it only when the gate rejects it at its stage for its row or call.
func runControls(repo, inventoryPath, only string) int {
	want := set(strings.Split(only, ","))
	failed := 0
	for _, ctl := range controls {
		if only != "" && !want[ctl.id] {
			continue
		}
		start := time.Now()
		verdict, output := runControl(repo, ctl)
		status := "ACCEPTED"
		if verdict != "" {
			status, failed = "FAILED", failed+1
		}
		fmt.Printf("control %s %s (%s): %s %s\n", ctl.id, status, time.Since(start).Round(time.Second), ctl.what, verdict)
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "FAIL ") || strings.HasPrefix(line, "gate: stopped") || strings.HasPrefix(line, "fixture ") || strings.HasPrefix(line, "leak ") {
				fmt.Printf("    %s\n", line)
			}
		}
	}
	if failed > 0 {
		fmt.Printf("controls: %d failed\n", failed)
		return 1
	}
	fmt.Println("controls: every control accepted")
	return 0
}

// runControl answers "" when the gate rejected the control as it must, and the reason otherwise,
// with the gate's output.
func runControl(repo string, ctl control) (string, string) {
	if ctl.own != nil {
		var inv inventory
		data, err := os.ReadFile(filepath.Join(repo, inventoryFile))
		if err != nil || json.Unmarshal(data, &inv) != nil {
			return "the head's inventory does not parse", ""
		}
		for i := range inv.Bun {
			inv.Bun[i].bun = true
		}
		return ctl.own(repo, inv)
	}
	work, err := os.MkdirTemp(os.Getenv("TMPDIR"), "redact-control.")
	if err != nil {
		return err.Error(), ""
	}
	defer os.RemoveAll(work)
	wt := filepath.Join(work, "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		return fmt.Sprintf("the worktree: %v: %s", err, out), ""
	}
	defer exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
	c := &controlRun{wt: wt, inventory: inventoryFile}
	data, err := os.ReadFile(filepath.Join(wt, inventoryFile))
	if err != nil || json.Unmarshal(data, &c.inv) != nil {
		return "the head's inventory does not parse", ""
	}
	if err := ctl.plant(c); err != nil {
		return "the plant: " + err.Error(), ""
	}
	// The plant's new files are committed too: the gate reads the pinned head alone.
	if out, err := exec.Command("git", "-C", wt, "add", "-A").CombinedOutput(); err != nil {
		return fmt.Sprintf("the plant's add: %v: %s", err, out), ""
	}
	if out, err := exec.Command("git", "-C", wt, "commit", "-qm", "control "+ctl.id).CombinedOutput(); err != nil {
		return fmt.Sprintf("the plant's commit: %v: %s", err, out), ""
	}
	var out bytes.Buffer
	stage := "mutate"
	if ctl.fixture {
		stage = "plan"
	}
	code := run(wt, filepath.Join(wt, inventoryFile), options{rows: ctl.rows, stage: stage}, &out)
	if ctl.fixture {
		if code != 0 {
			return "stages 1-3 did not pass before the fixture ran", out.String()
		}
		return fixtureControl(wt, &out), out.String()
	}
	return judgeControl(ctl, out.String(), code), out.String()
}

// judgeControl accepts the control when the first failing stage is its stage and that stage's
// failure names everything it expects.
func judgeControl(ctl control, output string, code int) string {
	if code == 0 {
		return "the gate passed it"
	}
	first := 0
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "FAIL stage ") {
			continue
		}
		var n int
		fmt.Sscanf(strings.TrimPrefix(line, "FAIL stage "), "%d", &n)
		if first == 0 {
			first = n
		}
		if n == first {
			lines = append(lines, line)
		}
	}
	if first != ctl.stage {
		return fmt.Sprintf("the gate stopped at stage %d, not %d", first, ctl.stage)
	}
	sets := ctl.each
	if len(sets) == 0 {
		sets = [][]string{ctl.expect}
	}
	var missing []string
	for _, set := range sets {
		found := false
		for _, line := range lines {
			named := true
			for _, e := range set {
				named = named && strings.Contains(line, e)
			}
			found = found || named
		}
		if !found {
			missing = append(missing, strings.Join(set, ", "))
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("no stage %d failure names %q", ctl.stage, strings.Join(missing, "; "))
	}
	return ""
}

// fixtureControl is control (1)'s evidence: with the grammar returning its input, every fixture
// row that holds a secret fails at REDACT-ASSERT fixture/<id>, with one of its secrets in the
// output the test captured there, in Go and in TypeScript.
func fixtureControl(wt string, out *bytes.Buffer) string {
	data, err := os.ReadFile(filepath.Join(wt, "packages/envoy/internal/redact/testdata/remote.json"))
	if err != nil {
		return err.Error()
	}
	var fixture struct {
		Rows []struct {
			ID      string   `json:"id"`
			Secrets []string `json:"secrets"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		return err.Error()
	}
	goTest := exec.Command("go", "test", "-count=1", "-v", "-run", "^TestRemotePrintsEveryCorpusRowAsItsPrinted$", "./internal/redact")
	goTest.Dir = filepath.Join(wt, "packages/envoy")
	goOut, _ := goTest.CombinedOutput()
	g := &gate{}
	if err := g.bunInstall(wt); err != nil {
		return err.Error()
	}
	bunTest := exec.Command("bun", "test", "src/remote-text.test.ts")
	bunTest.Dir = filepath.Join(wt, "packages/contracts")
	bunOut, _ := bunTest.CombinedOutput()
	missed := 0
	for lang, text := range map[string]string{"Go": string(goOut), "TypeScript": string(bunOut)} {
		res := result{captures: map[string][]string{}}
		res.scan(text)
		asserted := set(res.asserted)
		withSecrets, caught := 0, 0
		for _, row := range fixture.Rows {
			if len(row.Secrets) == 0 {
				continue
			}
			withSecrets++
			key := "fixture/" + row.ID
			held := false
			for _, captured := range res.captures[key] {
				for _, s := range row.Secrets {
					held = held || strings.Contains(captured, s)
				}
			}
			if asserted[key] && held {
				caught++
			} else {
				missed++
				fmt.Fprintf(out, "fixture %s row %s: not caught at REDACT-ASSERT %s with a secret in its captured output\n", lang, row.ID, key)
			}
		}
		fmt.Fprintf(out, "fixture %s: %d of the %d rows that hold a secret failed at their designated assertion with a secret in the rule's captured output\n", lang, caught, withSecrets)
	}
	if missed > 0 {
		return fmt.Sprintf("%d fixture rows were not caught", missed)
	}
	return ""
}
