package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFormCallbacksKeepExitCodesChecked(t *testing.T) {
	const source = `package main
const exitUsageError = 2
type command struct{}
type secretForm struct { help command; run func() int }
var secretForms []secretForm
func init() { secretForms = []secretForm{{command{}, good}} }
func good() int { return exitUsageError }
func dispatch() int {
	for _, form := range secretForms { return form.run() }
	return 0
}
`
	for _, tc := range []struct{ name, before, after, want string }{
		{"registered checked function", "", "", ""},
		{"undocumented handler exit", "return exitUsageError", "return 17", "exit code 17"},
		{"foreign handler", "command{}, good", "command{}, foreign.Run", "checked local exit-code function"},
		{"literal callback", "command{}, good", "command{}, func() int { return 17 }", "checked local exit-code function"},
		{"changed row", "return form.run()", "form.run = foreign.Run; return form.run()", "cannot be reassigned"},
		{"shadowed row", "return form.run()", "if true { form := foreign.Form(); return form.run() }", "exit code form.run()"},
		{"unknown callback", "range secretForms", "range otherForms", "exit code form.run()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, cliDir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			text := source
			if tc.before != "" {
				text = strings.Replace(text, tc.before, tc.after, 1)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			err := checkExitCodes(root, []docConst{{name: "exitUsageError"}})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// TestTheGcloudSupervisorsStatusIsNoExitCode: runGcloudSupervisor's returns are the gcloud
// supervisor's status, read only by its parent, so checkExitCodes neither checks them nor takes the
// function as an exit-code one: a return of 127 passes, os.Exit may take its call, and a command
// that returns its call is refused, as is the same body under any other name.
func TestTheGcloudSupervisorsStatusIsNoExitCode(t *testing.T) {
	const source = `package main
import "os"
func init() { os.Exit(runGcloudSupervisor(nil)) }
func runGcloudSupervisor(args []string) int { if len(args) == 0 { return 2 }; return 127 }
func dispatch() int { return 0 }
`
	for _, tc := range []struct{ name, before, after, want string }{
		{"the supervisor's own statuses", "", "", ""},
		{"a command that returns the supervisor's status", "return 0 }\n", "return runGcloudSupervisor(nil) }\n", "exit code runGcloudSupervisor(nil)"},
		{"the same body under another name", "runGcloudSupervisor(args []string)", "superviseAgain(args []string)", "exit code 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, cliDir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			text := source
			if tc.before != "" {
				text = strings.Replace(text, tc.before, tc.after, 1)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			err := checkExitCodes(root, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
