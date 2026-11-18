package rulehost

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
)

// hostShapeRows are the hosts of the airlock package's own tests of airlock.HostShapeOK: every row
// of TestHostShapeOK (airlock_test.go) and every host of TestModelAgreesOnHostShapes
// (airlock_fuzz_test.go), which holds Go's Validate to the Lean model's. They are copied here
// because a test of another package cannot read them; the two lists overlap, and each host is
// read once.
var hostShapeRows = []string{
	"slack.com", "*.googleapis.com", "API.GitHub.com", "a-b.example.com", "localhost",
	"1e100.net", "*.0.example.com", "xn--p1ai", "93.184.216.34", "*.216.34", "*.34", "8080",
	"https://slack.com", "*", "slack.com/", "slack.com:443",
	".slack.com", "slack..com", "slack.com.", "*.", "", "-slack.com", "slack-.com",
	"slack_com", "*.*.com", "sl\u212aack.com",
}

// TestTheGrammarAgreesWithHostShapeOKOnTheAirlocksOwnHosts: on every host the airlock package
// tests airlock.HostShapeOK with, the grammar serves exactly the hosts HostShapeOK accepts. None
// of them is one the grammar adds a refusal for, so a host the grammar refused there would be a
// refusal of a host the Lean model serves, and one it served a host the model refuses.
func TestTheGrammarAgreesWithHostShapeOKOnTheAirlocksOwnHosts(t *testing.T) {
	accepted := 0
	for _, host := range hostShapeRows {
		served, shaped := Refusal(host) == "", airlock.HostShapeOK(host)
		if served != shaped {
			t.Errorf("Refusal(%q) = %q while airlock.HostShapeOK(%q) = %v; want the grammar to serve exactly what HostShapeOK accepts", host, Refusal(host), host, shaped)
		}
		if shaped {
			accepted++
		}
	}
	// The control: the rows hold hosts of both kinds, so the comparison above can fail either way.
	if accepted == 0 || accepted == len(hostShapeRows) {
		t.Fatalf("airlock.HostShapeOK accepts %d of the %d rows; want some of each", accepted, len(hostShapeRows))
	}
}

// FuzzRefusalIsNoLooserThanHostShapeOK holds the grammar to airlock.HostShapeOK, the Lean-proved
// shape airlock.Validate holds every host to: a host Refusal serves (an empty reason) is one
// HostShapeOK accepts, so cmd/broker's validateRulesFile, which reads every host here first, never
// passes a host for Validate to refuse in words that quote it. It holds by construction, since
// HostShapeOK is the grammar's first test; without that test the grammar's own refusals past it
// read no character and no empty label, and the seeds below fail. The seeds are the airlock
// package's own hosts and the shapes of this program's host rows; go test runs each seed, and
// go test -fuzz searches beyond them.
func FuzzRefusalIsNoLooserThanHostShapeOK(f *testing.F) {
	for _, host := range hostShapeRows {
		f.Add(host)
	}
	for _, host := range []string{
		"2001:db8:7::1", "[2001:db8:7::1]", "2001:db8:7::1%eth0", "::ffff:192.0.2.10", "0x1f", "*.0x1f",
		"a.0x7f000001", "093.184.216.34", "api.github.com.", "a_b.example.com.", "api github.com.",
		"ab--cd.example.", "xn--zz.example.", "foo.123.", "https:/slack.com", "user:pw@slack.com",
		"mailto:ops@slack.com", "0x", "a.0x", "*.0x", "10.0x", "API.GitHub.com.", "bücher.example",
	} {
		f.Add(host)
	}
	f.Fuzz(func(t *testing.T, host string) {
		if why := Refusal(host); why == "" && !airlock.HostShapeOK(host) {
			t.Fatalf("Refusal serves %q, which airlock.HostShapeOK refuses", host)
		}
	})
}

// TestARulesHostOfManyDotsIsReadInLinearTime: the grammar takes every trailing dot off at once and
// reads the rest once, so a host of 100,000 dots, or a name followed by them, is refused as no
// host name in well under a second, and its reason says it binds nothing. Reading one dot at a
// time, as the grammar once did, took 2.76 s for such a host.
func TestARulesHostOfManyDotsIsReadInLinearTime(t *testing.T) {
	for _, host := range []string{strings.Repeat(".", 100000), "api.github.com" + strings.Repeat(".", 100000)} {
		start := time.Now()
		why := Refusal(host)
		if took := time.Since(start); took > time.Second {
			t.Errorf("reading a host of %d characters took %v; want well under a second", len(host), took)
		}
		if !strings.Contains(why, "ends in a dot") || !strings.Contains(why, "binds nothing") {
			t.Errorf("a host of %d characters that end in dots is refused %q; want that it ends in a dot and binds nothing", len(host), why)
		}
	}
}
