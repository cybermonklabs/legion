package main

// Acceptance tests for the check c-redact-remote-text at c82c3a0d9f, round 5. Each fails at that
// head for the defect it names and passes once the defect is fixed. Copy this file into
// packages/envoy/cmd/redact-gate and run from packages/envoy:
//
//	go test -count=1 -run 'TestAcceptance' ./cmd/redact-gate

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcceptanceTheGatePrintsTheProbeMarkersItTested: "What the check prints as its bound" has the
// check report "the probe markers it tested". The gate holds them (markers, main.go) and reads
// each mutant's captured output for them (rows.go), but no print call of the gate names them: a
// stage 5 line says only "its captured output naming a marker", and the script prints none.
func TestAcceptanceTheGatePrintsTheProbeMarkersItTested(t *testing.T) {
	set := token.NewFileSet()
	packages, err := parser.ParseDir(set, ".", func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	reads, prints := 0, 0
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := selector.Sel.Name
				printer := strings.HasPrefix(name, "Fprint") || strings.HasPrefix(name, "Print")
				for _, arg := range call.Args {
					ast.Inspect(arg, func(m ast.Node) bool {
						if id, ok := m.(*ast.Ident); ok && id.Name == "markers" && printer {
							prints++
						}
						return true
					})
				}
				return true
			})
			ast.Inspect(file, func(n ast.Node) bool {
				if r, ok := n.(*ast.RangeStmt); ok {
					if id, ok := r.X.(*ast.Ident); ok && id.Name == "markers" {
						reads++
					}
				}
				return true
			})
		}
	}
	// Control: the gate does read the markers (its evidence loop ranges over them), so a print of
	// them would be found the same way.
	if reads == 0 {
		t.Fatal("control: no range over markers in the gate's source, so this test reads the wrong package")
	}
	if prints == 0 {
		t.Errorf("no print call of the gate names the probe markers it tested (markers is read in %d loops and printed by none)", reads)
	}
}

// TestAcceptanceTheInventoryHoldsTheDeviceSignInEchoRows: the verification's inventory list names,
// among the rows for an answer that echoes its own request, "a device sign-in's wait and refresh"
// beside "an external-account mint's token exchange and impersonation". The test that pins the
// device flow's Wait and Refresh against an echoing page exists
// (internal/cipherclient TestProxyEchoDeviceSignInPrintsTheClassAndNoCredential), but no inventory
// row names it, so the check runs neither.
func TestAcceptanceTheInventoryHoldsTheDeviceSignInEchoRows(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "redact", "testdata", "redact-check.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Go []struct {
			ID      string `json:"id"`
			Package string `json:"package"`
			Test    string `json:"test"`
		} `json:"go"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	holds := func(pkg, test string) bool {
		for _, row := range inventory.Go {
			if row.Package == pkg && row.Test == test {
				return true
			}
		}
		return false
	}
	// Control: the same lookup finds the external-account mint's echo rows the same sentence names,
	// so a miss below is the inventory's and not the lookup's.
	for _, sub := range []string{"impersonation", "security_token_service"} {
		if !holds("./internal/cipherclient", "TestProxyEchoExternalAccountMintNamesThePageWithoutItsCredentials/"+sub) {
			t.Fatalf("control: the lookup finds no external-account mint row %q, so it cannot see a device row either", sub)
		}
	}
	for _, sub := range []string{"Wait", "Refresh"} {
		test := "TestProxyEchoDeviceSignInPrintsTheClassAndNoCredential/" + sub
		if !holds("./internal/cipherclient", test) {
			t.Errorf("no inventory row names ./internal/cipherclient %s: the device sign-in's %s answered by a page that echoes the request", test, strings.ToLower(sub))
		}
	}
}

// TestAcceptanceTheCheckStatesTheBoundTheVerificationNames: "What the check prints as its bound"
// has the check state that its results do not establish, among others, errors of libraries other
// than golang.org/x/oauth2, github.com/coreos/go-oidc and openid-client; a call into one of those
// libraries made through reflection (reflect.Value.MethodByName(...).Call); and the forms the
// fixture's residual rows pin as printing. The script's statement names neither go-oidc nor the
// reflection call, and of the two known-residual rows at the head (split-user-info-bare and
// split-user-info-json-key) it names only the bare one.
func TestAcceptanceTheCheckStatesTheBoundTheVerificationNames(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "check-redact-remote-text.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "What these results do not establish")
	if start < 0 {
		t.Fatal("the script prints no statement of what its results do not establish")
	}
	end := strings.Index(script[start:], "\nEOF")
	if end < 0 {
		t.Fatal("the statement's heredoc has no end")
	}
	statement := strings.Join(strings.Fields(script[start:start+end]), " ")
	// Control: the statement this test reads is the one that names the other libraries' errors.
	if !strings.Contains(statement, "golang.org/x/oauth2") || !strings.Contains(statement, "openid-client") {
		t.Fatalf("control: the statement read is not the libraries' one:\n%s", statement)
	}
	for _, want := range []struct{ what, text string }{
		{"github.com/coreos/go-oidc among the libraries whose errors the censuses hold", "go-oidc"},
		{"a library call made through reflection, which no census lists", "MethodByName"},
		{"the known-residual row split-user-info-json-key, the JSON-key form of a bare label: name@host", "JSON"},
	} {
		if !strings.Contains(statement, want.text) {
			t.Errorf("the statement of what the results do not establish leaves out %s (%q):\n%s", want.what, want.text, statement)
		}
	}
	// Control: the fixture still pins the JSON-key form as printing, so the statement owes it.
	raw, err = os.ReadFile(filepath.Join("..", "..", "internal", "redact", "testdata", "remote.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Rows []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
			Readable bool   `json:"readable"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	pinned := false
	for _, row := range fixture.Rows {
		if row.ID == "split-user-info-json-key" && row.Category == "known-residual" && row.Readable {
			pinned = true
		}
	}
	if !pinned {
		t.Fatal("control: the fixture pins no known-residual row split-user-info-json-key as printing")
	}
}
