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

// TestAStorePackageCallsNoExit: checkExitCodes parses every non-test package under
// internal/cipher/secretstore and refuses an os.Exit there, naming its file, since a store's code
// runs inside agent-secrets and cipherd alike and an exit code from it is none the error page
// lists. A store package that calls none passes (the control).
func TestAStorePackageCallsNoExit(t *testing.T) {
	const cli = "package main\nfunc run() int { return 0 }\n"
	for _, tc := range []struct{ name, store, want string }{
		{"store exit", "package fixture\nimport \"os\"\nfunc Quit() { os.Exit(3) }\n", "internal/cipher/secretstore/fixture/store.go"},
		{"store without exit", "package fixture\nfunc Quit() {}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for dir, text := range map[string]string{cliDir: cli, storesDir + "/fixture": tc.store} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
				name := "main.go"
				if dir != cliDir {
					name = "store.go"
				}
				if err := os.WriteFile(filepath.Join(root, dir, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkExitCodes(root, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "os.Exit") {
				t.Fatalf("got %v, want the os.Exit in %s refused", err, tc.want)
			}
		})
	}
}
