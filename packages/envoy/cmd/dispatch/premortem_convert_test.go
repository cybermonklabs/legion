package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
)

// The map file `envoy-dispatch premortem-convert --prevented-by` reads: one line for each risk, three
// fields separated by one tab each, blank lines and lines opening with # skipped. A file of that
// shape reads as its entries with the line each came from; every line of another shape is refused
// naming its line, through the command table the binary dispatches on and before any database is
// opened, with exit 1 and nothing written.
func TestThePremortemMapFileIsRefusedNamingTheLine(t *testing.T) {
	check := "dispatch://CORE-1/artifact/verification-md#b-c-visibility-kinds"
	other := "dispatch://CORE-2/artifact/verification-md#b-k2"
	file := strings.Join([]string{
		"# risk\tchecks\treason",
		"",
		"0\tnone\tNo check covers a slice that ships nothing visible.",
		"8\t" + check + " " + other + "\tThe kind census fails a kind with no predicate.",
		"   ",
		"3\tnone\tA reason\twith a tab in it.\r",
	}, "\n")
	entries, lines, err := parsePreventedBy(file)
	if err != nil {
		t.Fatalf("parse a well-formed map: %v", err)
	}
	want := []api.PremortemEntry{
		{Risk: 0, Checks: []string{}, Reason: "No check covers a slice that ships nothing visible."},
		{Risk: 8, Checks: []string{check, other}, Reason: "The kind census fails a kind with no predicate."},
		{Risk: 3, Checks: []string{}, Reason: "A reason\twith a tab in it."},
	}
	if len(entries) != len(want) || !slices.Equal(lines, []int{3, 4, 6}) {
		t.Fatalf("the map reads %+v from lines %v, want %+v from lines 3, 4 and 6", entries, lines, want)
	}
	for index := range want {
		if entries[index].Risk != want[index].Risk || !slices.Equal(entries[index].Checks, want[index].Checks) ||
			entries[index].Reason != want[index].Reason {
			t.Errorf("line %d reads %+v, want %+v", lines[index], entries[index], want[index])
		}
	}

	for _, refused := range []struct{ name, line, says string }{
		{"two fields", "2\tnone", "the line holds 2 fields"},
		{"one field", "2 none no reason", "the line holds 1 fields"},
		{"a word for a number", "two\tnone\tA reason.", `the first field "two" is no risk number`},
		{"a negative number", "-1\tnone\tA reason.", `the first field "-1" is no risk number`},
		{"a number with a leading zero", "08\tnone\tA reason.", `the first field "08" is no risk number`},
		{"an empty number", "\tnone\tA reason.", `the first field "" is no risk number`},
		{"an empty second field", "2\t\tA reason.", `the second field "" is neither none nor check addresses`},
		{"two spaces between checks", "2\t" + check + "  " + other + "\tA reason.", "separated by one space each"},
		{"none beside a check", "2\tnone " + check + "\tA reason.", "is neither none nor check addresses"},
		{"a blank reason", "2\tnone\t  ", "the reason is blank"},
		{"text that is not UTF-8", "2\tnone\tA reason \xff.", "the line is not UTF-8"},
	} {
		text := "# the map\n0\tnone\tA reason.\n" + refused.line + "\n"
		if _, _, err := parsePreventedBy(text); err == nil || !strings.HasPrefix(err.Error(), "3: ") ||
			!strings.Contains(err.Error(), refused.says) {
			t.Errorf("a map with %s on line 3 is refused %v, want line 3 named, saying %q", refused.name, err, refused.says)
		}

		path := filepath.Join(t.TempDir(), "map.tsv")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatalf("write the map file: %v", err)
		}
		var stdout, stderr bytes.Buffer
		code := runSubcommand(context.Background(), []string{"premortem-convert", "--issue", "CORE-1",
			"--artifact", "graph-premortem-md", "--prevented-by", path}, envGetter(map[string]string{}), &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), path+":3: ") ||
			!strings.Contains(stderr.String(), refused.says) {
			t.Errorf("premortem-convert with %s on line 3 exited %d with stdout %q and stderr %q, want 1 naming %s:3",
				refused.name, code, stdout.String(), stderr.String(), path)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := runSubcommand(context.Background(), []string{"premortem-convert", "--issue", "CORE-1"},
		envGetter(map[string]string{}), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--artifact is required") {
		t.Errorf("premortem-convert without --artifact exited %d with stderr %q, want 2 naming the flag", code, stderr.String())
	}
	stderr.Reset()
	if code := runSubcommand(context.Background(), []string{"premortem-convert", "-h"},
		envGetter(map[string]string{}), &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "three fields separated by\none tab each") {
		t.Errorf("premortem-convert -h exited %d with stderr %q, want 0 and the usage giving the map file's format", code, stderr.String())
	}
}
