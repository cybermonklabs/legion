package main

// Tests that the check holds three things its verification names: the probe markers it prints as
// part of its bound, the device sign-in's echo rows in its inventory, and its statement of what
// its results do not establish.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTheGatesReportNamesEveryProbeMarkerItsEvidenceReads: the report, which the check prints as
// part of its bound, holds one MARKERS line, and it names exactly the probe markers a mutant's
// captured output is read for: each it names makes output that holds it alone evidence
// (result.marked), and it names every one of them.
func TestTheGatesReportNamesEveryProbeMarkerItsEvidenceReads(t *testing.T) {
	var out bytes.Buffer
	(&gate{out: &out}).report(census{})
	var lines []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "MARKERS ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("the report holds %d MARKERS lines, want one:\n%s", len(lines), out.String())
	}
	_, list, ok := strings.Cut(lines[0], ": ")
	if !ok {
		t.Fatalf("the MARKERS line names no list: %q", lines[0])
	}
	printed := strings.Split(list, ", ")
	// Control: output that holds no marker is not evidence, so a marker below that is is read for.
	if (result{captures: map[string][]string{"probe": {"a captured line that names no marker"}}}).marked("probe") {
		t.Fatal("control: output with no marker reads as evidence, so marked cannot tell a marker apart")
	}
	for _, m := range printed {
		if !(result{captures: map[string][]string{"probe": {"captured " + m + " here"}}}).marked("probe") {
			t.Errorf("the MARKERS line names %q, which a mutant's captured output is not read for", m)
		}
	}
	for _, m := range markers {
		if !slices.Contains(printed, m) {
			t.Errorf("the MARKERS line leaves out %q, which a mutant's captured output is read for: %q", m, lines[0])
		}
	}
}

// TestTheInventoryHoldsTheDeviceSignInEchoRows: the verification names, among the rows for an
// answer that echoes its own request, "a device sign-in's wait and refresh" beside "an
// external-account mint's token exchange and impersonation", and the inventory holds a row for
// each: internal/cipherclient's TestProxyEchoDeviceSignInPrintsTheClassAndNoCredential, its Wait
// and its Refresh.
func TestTheInventoryHoldsTheDeviceSignInEchoRows(t *testing.T) {
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

// TestTheChecksStatementNamesWhatItsResultsDoNotEstablish: the statement the check prints of what
// its results do not establish names, among others, github.com/coreos/go-oidc among the libraries
// whose errors the censuses hold; a call into one of those libraries made through reflection
// (reflect.Value.MethodByName(...).Call), which no census lists; the JSON-key form of the
// fixture's known residual (split-user-info-json-key) beside the bare one; a credential cut into
// pieces shorter than redact.TokenPiece, which the rule does not redact; and a public name's
// route match, which reads the request's whole escaped path.
func TestTheChecksStatementNamesWhatItsResultsDoNotEstablish(t *testing.T) {
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
		{"a credential cut into pieces shorter than the rule's shortest redacted run", "pieces shorter than redact.TokenPiece"},
		{"a public name's route match, which reads the request's whole escaped path", "whole escaped path"},
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
