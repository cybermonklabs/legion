package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// skipCheck is the row's test's check in Go: it fails at REDACT-ASSERT probe.echo when Echo
// answers more than its word, with that answer captured under the same name.
const skipCheck = `package echo

import (
	"encoding/base64"
	"testing"
)

func check(t *testing.T) {
	got := Echo("word")
	t.Logf("REDACT-CAPTURE probe.echo %s", base64.StdEncoding.EncodeToString([]byte(got)))
	if got != "word" {
		t.Errorf("REDACT-ASSERT probe.echo: Echo answers %q", got)
	}
}
`

// skipCheckBun is the same check as the body of a Bun test.
const skipCheckBun = `
	const got = echo("word");
	console.log("REDACT-CAPTURE probe.echo " + Buffer.from(got).toString("base64"));
	if (got !== "word") throw new Error("REDACT-ASSERT probe.echo: echo answers " + got);
`

// skipFixture is a snapshot's files, by path, and the row the gate runs in it.
type skipFixture struct {
	files map[string]string
	r     *row
}

// skipGoRow is a Go module whose row's test is test, in echo_test.go, and whose mutation makes
// Echo answer a marker.
func skipGoRow(test string) skipFixture {
	return skipFixture{
		files: map[string]string{
			"packages/envoy/go.mod":             "module example.com/probe\n\ngo 1.27\n",
			"packages/envoy/echo/echo.go":       "package echo\n\n// Echo answers its word.\nfunc Echo(word string) string { return word }\n",
			"packages/envoy/echo/check_test.go": skipCheck,
			"packages/envoy/echo/echo_test.go":  "package echo\n\nimport \"testing\"\n\n" + test,
		},
		r: &row{ID: "probe", Package: "./echo", Test: "TestEcho", Capture: "probe.echo", Function: "example.com/probe/echo.Echo",
			Mutation: &mutation{File: "packages/envoy/echo/echo.go", Preimage: "return word }", Replacement: `return word + " not-a-secret-token" }`}},
	}
}

// skipBunRow is a Bun package whose row's test is test, in echo.test.ts, and whose mutation makes
// echo answer a marker.
func skipBunRow(test string) skipFixture {
	return skipFixture{
		files: map[string]string{
			"packages/probe/echo.ts":      "export const echo = (word: string): string => word;\n",
			"packages/probe/echo.test.ts": "import { test } from \"bun:test\";\nimport { echo } from \"./echo\";\n\n" + test,
		},
		r: &row{ID: "probe", File: "packages/probe/echo.test.ts", Test: "echo", Capture: "probe.echo", Function: "echo", bun: true,
			Mutation: &mutation{File: "packages/probe/echo.ts", Preimage: "=> word", Replacement: `=> word + " not-a-secret-token"`}},
	}
}

// TestARowWhoseTestSkipsIsUnknownInStagesFourAndFive: a run that skipped the row's test, or a
// subtest of it (go test's skip action, a Bun testcase's <skipped>), checked nothing, so stage 4
// stops at it UNKNOWN, naming the row, the skipped test and its skip's line, and stage 5 leaves
// its mutant unproven, neither caught nor survived. Before, stage 4 counted the skip a pass and
// stage 5 a surviving mutant: a row whose test needs a test database did so on a run without the
// database's URL. The controls run the same row with nothing skipped: stage 4 passes and stage 5
// catches the mutant, so the skip alone makes each verdict UNKNOWN.
func TestARowWhoseTestSkipsIsUnknownInStagesFourAndFive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture skipFixture
		skipped string // the skipped test the UNKNOWN lines name, its skip's line included; "" for a control
	}{
		{name: "the row's test skips", skipped: "TestEcho (echo_test.go:6: PROBE_DATABASE_URL must be set)",
			fixture: skipGoRow("func TestEcho(t *testing.T) {\n\tt.Skip(\"PROBE_DATABASE_URL must be set\")\n\tcheck(t)\n}\n")},
		{name: "a subtest of the row's test skips", skipped: "TestEcho/stored (echo_test.go:8: PROBE_DATABASE_URL must be set)",
			fixture: skipGoRow("func TestEcho(t *testing.T) {\n\tt.Run(\"plain\", func(t *testing.T) {})\n\tt.Run(\"stored\", func(t *testing.T) {\n\t\tt.Skip(\"PROBE_DATABASE_URL must be set\")\n\t\tcheck(t)\n\t})\n}\n")},
		{name: "control: nothing skips",
			fixture: skipGoRow("func TestEcho(t *testing.T) {\n\tt.Run(\"plain\", func(t *testing.T) {})\n\tt.Run(\"stored\", func(t *testing.T) { check(t) })\n}\n")},
		{name: "the row's Bun test skips", skipped: "echo",
			fixture: skipBunRow("test.skip(\"echo\", () => {" + skipCheckBun + "});\n")},
		{name: "control: the row's Bun test runs",
			fixture: skipBunRow("test(\"echo\", () => {" + skipCheckBun + "});\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := t.TempDir()
			for path, text := range tc.fixture.files {
				full := filepath.Join(snapshot, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := tc.fixture.r
			start := strings.Index(tc.fixture.files[r.Mutation.File], r.Mutation.Preimage)
			at := [2]int{start, start + len(r.Mutation.Preimage)}
			var out bytes.Buffer
			g := &gate{snapshot: snapshot, work: t.TempDir(), rowTimeout: time.Minute, out: &out}

			g.baseline(r, map[string]error{})
			stage4 := out.String()
			if tc.skipped == "" {
				if g.failures != 0 || g.unknowns != 0 {
					t.Fatalf("stage 4 of the row with nothing skipped: %d failures, %d UNKNOWN, want neither:\n%s", g.failures, g.unknowns, stage4)
				}
			} else if g.failures != 0 || g.unknowns != 1 || !strings.Contains(stage4, "UNKNOWN stage 4 (baseline) row probe: ") ||
				!strings.Contains(stage4, "skipped "+tc.skipped+", so it neither passed nor failed") {
				t.Errorf("stage 4 of the row whose test skips: %d failures, %d UNKNOWN, want one UNKNOWN naming the row and %q:\n%s", g.failures, g.unknowns, tc.skipped, stage4)
			}

			out.Reset()
			g.failures, g.unknowns = 0, 0
			g.gateRow(r, at)
			stage5 := out.String()
			if tc.skipped == "" {
				if g.failures != 0 || g.unknowns != 0 || !strings.Contains(stage5, "PASS stage 5 (mutation) row probe: ") {
					t.Fatalf("stage 5 of the row with nothing skipped: %d failures, %d UNKNOWN, want the mutant caught:\n%s", g.failures, g.unknowns, stage5)
				}
				return
			}
			if g.failures != 0 || g.unknowns != 1 || !strings.Contains(stage5, "UNKNOWN stage 5 (mutation) row probe ") ||
				!strings.Contains(stage5, "the mutant is unproven, neither caught nor survived: its test skipped "+tc.skipped) {
				t.Fatalf("stage 5 of the row whose test skips: %d failures, %d UNKNOWN, want one UNKNOWN leaving the mutant unproven and naming %q:\n%s", g.failures, g.unknowns, tc.skipped, stage5)
			}
		})
	}
}
