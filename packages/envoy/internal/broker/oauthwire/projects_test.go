package oauthwire_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// TestParseProjectsRefusesWhatIsNoList: a list is project keys separated by white space, at most
// MaxProjects of them, each once; anything else is refused before any of it is asked about, and
// the refusal names the rule. A blank list is no projects at all.
func TestParseProjectsRefusesWhatIsNoList(t *testing.T) {
	seventeen := make([]string, oauthwire.MaxProjects+1)
	for i := range seventeen {
		seventeen[i] = fmt.Sprintf("P%02d", i)
	}
	for _, bad := range []struct{ raw, says string }{
		{"priv", `"priv" is not a project key`},
		{"PUB Priv", `"Priv" is not a project key`},
		{strings.Join(seventeen, " "), "at most 16"},
		{"PRIV PUB PRIV", "PRIV is named twice"},
		{"PRIV,PUB", `"PRIV,PUB" is not a project key`},
		{"P", `"P" is not a project key`},
		{"ABCDEFGHIJK", `"ABCDEFGHIJK" is not a project key`},
	} {
		got, err := oauthwire.ParseProjects(bad.raw)
		if err == nil || !strings.Contains(err.Error(), bad.says) || got != nil {
			t.Errorf("ParseProjects(%q) = %v, %v; want no list and a refusal saying %q", bad.raw, got, err, bad.says)
		}
	}
	for raw, want := range map[string][]string{
		"PUB  PRIV":        {"PRIV", "PUB"},
		"\tPRIV\n":         {"PRIV"},
		"ZZ A1 MM":         {"A1", "MM", "ZZ"},
		"":                 nil,
		"   ":              nil,
		"ACME ACME2 ACME3": {"ACME", "ACME2", "ACME3"},
	} {
		got, err := oauthwire.ParseProjects(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseProjects(%q) = %v, %v; want %v sorted and no error", raw, got, err, want)
		}
	}
	sixteen, err := oauthwire.ParseProjects(strings.Join(seventeen[:oauthwire.MaxProjects], " "))
	if err != nil || len(sixteen) != oauthwire.MaxProjects {
		t.Fatalf("ParseProjects of %d keys = %v, %v; want all of them", oauthwire.MaxProjects, sixteen, err)
	}
}

// TestValidProjectsHoldsAListToItsOwnSpelling: ValidProjects takes exactly what ParseProjects
// answers — keys, sorted, each once, at most MaxProjects — and refuses anything else, which is
// how a reader of a list it did not parse itself (an introspection answer) holds it to the rule.
func TestValidProjectsHoldsAListToItsOwnSpelling(t *testing.T) {
	for _, good := range [][]string{nil, {}, {"PRIV"}, {"PRIV", "PUB"}} {
		if err := oauthwire.ValidProjects(good); err != nil {
			t.Errorf("ValidProjects(%v) = %v, want nil", good, err)
		}
	}
	for _, bad := range [][]string{{"PUB", "PRIV"}, {"PRIV", "PRIV"}, {"priv"}, {""}, make([]string, oauthwire.MaxProjects+1)} {
		if err := oauthwire.ValidProjects(bad); err == nil {
			t.Errorf("ValidProjects(%q) = nil, want a refusal", bad)
		}
	}
}

// TestJoinProjectsRoundTrips: the stored and the wire spelling of a list parses back to the very
// list, and no list is the empty string.
func TestJoinProjectsRoundTrips(t *testing.T) {
	for _, list := range [][]string{nil, {"PRIV"}, {"A1", "PRIV", "PUB"}} {
		joined := oauthwire.JoinProjects(list)
		back, err := oauthwire.ParseProjects(joined)
		if err != nil || !reflect.DeepEqual(back, list) {
			t.Errorf("ParseProjects(JoinProjects(%v)) = %v, %v; want the same list", list, back, err)
		}
	}
	if joined := oauthwire.JoinProjects(nil); joined != "" {
		t.Errorf("JoinProjects(nil) = %q, want the empty string", joined)
	}
	if joined := oauthwire.JoinProjects([]string{"PRIV", "PUB"}); joined != "PRIV PUB" {
		t.Errorf("JoinProjects(PRIV, PUB) = %q, want \"PRIV PUB\"", joined)
	}
}
