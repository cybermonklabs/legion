package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretFormCallbacksKeepExitCodesChecked: checkExitCodes follows an exit code from each root
// (the argument main passes to os.Exit, and each registered secret-form handler) down through the
// functions it returns, and refuses an undocumented code wherever on that path it is planted: in
// a handler, in a command the root returns, in a helper that command returns, in a helper that a
// helper declared before its command returns, which a closure of one pass over the files does not
// reach, and at the end of a chain of three helpers each declared before the function that
// returns it, which a closure of one or two passes does not reach. It checks the argument of
// os.Exit itself as an exit code too. An int helper no exit path reaches is no exit code.
func TestSecretFormCallbacksKeepExitCodesChecked(t *testing.T) {
	const source = `package main
const exitUsageError = 2
type command struct{}
type secretForm struct { help command; run func() int }
var secretForms []secretForm
func init() { secretForms = []secretForm{{command{}, good}} }
func main() { os.Exit(dispatch()) }
func good() int { return exitUsageError }
func waitBound(wait int) int { return wait }
func dispatch() int {
	for _, form := range secretForms { return form.run() }
	return cmdList()
}
func cmdList() int { if false { return 1 }; return listMachines() }
func listMachines() int { return exitUsageError }
`
	for _, tc := range []struct{ name, before, after, want string }{
		{"unreached int helper", "", "", ""},
		{"real undocumented handler exit", "return exitUsageError", "return 17", "exit code 17"},
		{"undocumented exit in a command the root returns", "{ return 1 }", "{ return 17 }", "exit code 17"},
		{"undocumented exit in a helper that command returns", "listMachines() int { return exitUsageError }", "listMachines() int { return 17 }", "exit code 17"},
		{"undocumented exit below a helper declared before its command",
			"func cmdList() int { if false { return 1 }; return listMachines() }\nfunc listMachines() int { return exitUsageError }",
			"func listMachines() int { return machineExit() }\nfunc cmdList() int { if false { return 1 }; return listMachines() }\nfunc machineExit() int { return 17 }",
			"exit code 17"},
		{"undocumented exit at the end of three helpers each declared before the one that returns it",
			"func cmdList() int { if false { return 1 }; return listMachines() }\nfunc listMachines() int { return exitUsageError }",
			"func machineCode() int { return 17 }\nfunc machineExit() int { return machineCode() }\nfunc listMachines() int { return machineExit() }\nfunc cmdList() int { if false { return 1 }; return listMachines() }",
			"exit code 17"},
		{"undocumented os.Exit argument", "os.Exit(dispatch())", "os.Exit(17)", "exit code 17"},
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
