package rulehost

import (
	"slices"
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

// refusalRow is one host spelling the grammar refuses: what the refusal must say, and the text of
// the host as written it must not repeat ("" where there is no such text to look for).
type refusalRow struct {
	name   string
	host   string
	want   []string
	absent string
}

// absents is every text the row's refusal must not hold: the host's own text as written, and the
// grammar's advice (writeAHostName) where the row wants none, so a reason that carries its own
// advice, or names a form, is given no second one. A refusal gives one advice: writeAHostName, a
// form to write ("write it as …"), or the advice its own reason carries, which an IP address's
// and a scheme's do.
func (c refusalRow) absents() []string {
	absent := []string{c.absent}
	if !slices.Contains(c.want, writeAHostName) {
		absent = append(absent, writeAHostName)
	}
	return absent
}

// refusalRows is every host spelling outside the one grammar a rules host is written in, one row
// per way out of it. A row names the form to write where the host has one, and no form otherwise.
// cmd/broker drives a few of these spellings through airlock-check and the boot, the ones a reader
// of the rules file could change before Refusal reads them.
var refusalRows = func() []refusalRow {
	// An IP address is refused in every spelling, each with what it would do served: the
	// canonical spelling and upper-case IPv6 bind and are decrypted, a trailing dot makes a name
	// the resolver refuses, and every other spelling binds nothing and is tunnelled unread. Where
	// the file writes the address another way than netaddr.Canonical's, the refusal names that
	// form.
	ip := func(does []string, canonical string) []string {
		want := append([]string{"is an IP address", "carries no secret there", "name the service by its host name"}, does...)
		if canonical != "" {
			want = append(want, canonical+" in its canonical form")
		}
		return want
	}
	decrypted := []string{"no TLS server name", "proxy decrypts a CONNECT to it", "sni mismatch"}
	tunnelled := []string{"no CONNECT's host", "binds nothing", "tunnelled unread"}
	dotted := []string{"trailing dot", "the resolver refuses", "refused before anything is bound"}
	formed := func(why, form string) []string { return []string{why, "write it as " + form} }
	noForm := func(why ...string) []string {
		return append(why, writeAHostName)
	}
	const numbered = "clients read such a host differently"
	const notDNS = "is no DNS name"
	// decryptedDot is the reason of one trailing dot after a name the resolver reads: a CONNECT
	// that writes the dot binds the entry and is decrypted, and every request it carries is
	// refused, whatever else is wrong with the name.
	decryptedDot := []string{"ends in a dot", "proxy decrypts a CONNECT to it", "sni mismatch"}
	longLabel := strings.Repeat("a", 64) + ".example"
	longName := strings.Repeat(strings.Repeat("b", 63)+".", 4) + "example"
	return []refusalRow{
		{"a canonical IPv4 address", "93.184.216.34", ip(decrypted, ""), "216.34"},
		{"a canonical IPv6 address", "2001:db8:7::1", ip(decrypted, ""), "7::1"},
		{"a canonical IPv6 address with an embedded IPv4 part", "2001:db8::5db8:d822", ip(decrypted, ""), "5db8"},
		{"an IPv6 address in upper case", "2001:DB8:8::1", ip(decrypted, "2001:db8:8::1"), "DB8"},
		{"an IPv6 address not written in its canonical form", "2001:db8:7:0::1", ip(tunnelled, "2001:db8:7::1"), "7:0::1"},
		{"an IPv6 address with a leading zero in a group", "2001:0db8:7::1", ip(tunnelled, "2001:db8:7::1"), "0db8"},
		{"an IPv4-mapped IPv6 address", "::ffff:93.184.216.34", ip(tunnelled, "93.184.216.34"), "::ffff:"},
		{"an IPv6 address written with an IPv4 part", "2001:db8::93.184.216.34", ip(tunnelled, "2001:db8::5db8:d822"), "::93.184"},
		{"an IPv6 address with a zone", `2001:db8:7::1%eth0 "x`, ip(tunnelled, "2001:db8:7::1"), "eth0"},
		{"an IPv6 address in brackets", "[2001:db8:7::1]", ip(tunnelled, "2001:db8:7::1"), "[2001"},
		{"an IPv6 address with a zone in brackets", `[2001:db8:7::1%eth0 "x]`, ip(tunnelled, "2001:db8:7::1"), "eth0"},
		{"an IPv4-mapped IPv6 address in brackets", "[::ffff:93.184.216.34]", ip(tunnelled, "93.184.216.34"), "::ffff:"},
		{"an IPv6 address after an opening bracket alone", "[2001:db8:7::1", ip(tunnelled, "2001:db8:7::1"), "[2001"},
		{"an IPv4 address with a trailing dot", "93.184.216.34.", ip(dotted, "93.184.216.34"), "216.34."},
		{"an IPv6 address with a trailing dot", "2001:db8:7::1.", ip(dotted, "2001:db8:7::1"), "::1."},
		{"an IPv4 address with a port", "93.184.216.34:443", ip(tunnelled, "93.184.216.34"), ":443"},
		{"an IPv6 address in brackets with a port", "[2001:db8:7::1]:443", ip(tunnelled, "2001:db8:7::1"), "]:443"},
		// A host name written another way than the grammar's: the refusal names the grammar's
		// spelling of the same host, in lower case.
		{"a host name with a trailing dot", "api.github.com.", formed("sni mismatch", "api.github.com"), "github.com."},
		// In mixed case and with a trailing dot the host binds, since airlock.HostBound folds
		// case, and so it is decrypted and refused as the same name in lower case is.
		{"a host name in mixed case with a trailing dot", "API.GitHub.com.", append(formed("ends in a dot", "api.github.com"), "sni mismatch"), "GitHub"},
		{"a wildcard with a trailing dot", "*.googleapis.com.", append(formed("ends in a dot", "*.googleapis.com"), "sni mismatch"), "googleapis.com."},
		{"a wildcard in mixed case with a trailing dot", "*.GoogleAPIs.com.", append(formed("ends in a dot", "*.googleapis.com"), "sni mismatch"), "GoogleAPIs"},
		{"a name of 253 characters with a trailing dot", longestHost + ".", formed("ends in a dot", longestHost), longestHost + "."},
		{"a Unicode internationalized name", "bücher.example", formed("punycode", "xn--bcher-kva.example"), "bücher"},
		{"a Unicode internationalized name under a wildcard", "*.bücher.example", formed("punycode", "*.xn--bcher-kva.example"), "bücher"},
		{"a host name ending in an ideographic full stop", "api.github.com\u3002", formed("is not ASCII", "api.github.com"), "\u3002"},
		{"a host name ending in a full-width full stop", "api.github.com\uff0e", formed("is not ASCII", "api.github.com"), "\uff0e"},
		{"a trailing dot and a zero-width space", "api.github.com.\u200b", formed("is not ASCII", "api.github.com"), "\u200b"},
		// Text that is no host name in any spelling: the refusal names no form.
		{"an empty host", "", noForm("is empty", "binds nothing"), ""},
		{"a wildcard alone", "*", noForm("has a * other than one leading", "binds nothing"), ""},
		{"a wildcard over a wildcard", "*.*", noForm("has a * other than one leading", "binds nothing"), ""},
		{"a wildcard before a doubled dot", "*..com", noForm("has an empty label", "binds nothing"), "..com"},
		{"a leading dot", ".api.github.com", noForm("has an empty label", "binds nothing"), ".api"},
		{"a doubled dot", "a..b.com", noForm("has an empty label", "binds nothing"), "a..b"},
		{"a space", " ", noForm("white space", "binds nothing"), ""},
		{"a tab", "\t", noForm("white space", "binds nothing"), ""},
		{"a DEL character", "api\x7f.github.com", noForm("white space or a control character", "binds nothing"), "api\x7f"},
		{"a leading space", " api.github.com", noForm("white space", "binds nothing"), "github"},
		{"a trailing space", "api.github.com ", noForm("white space", "binds nothing"), "github"},
		{"a trailing dot and a space", "api.github.com. ", noForm("white space", "binds nothing"), "github"},
		{"a trailing dot and a newline", "api.github.com.\n", noForm("white space", "binds nothing"), "github"},
		// A port is digits after a colon; a colon before anything else is no DNS name's, and its
		// refusal says nothing of a port.
		{"a host name with a port", "api.github.com:443", noForm("holds a port", "without its port", "binds nothing"), ":443"},
		{"a scheme with one slash", "https:/slack.com", noForm(notDNS), "https:"},
		{"a user and a password", "user:pw@slack.com", noForm(notDNS), "pw@"},
		{"a mail address", "mailto:ops@slack.com", noForm(notDNS), "ops@"},
		{"a host name with a service for its port", "api.github.com:https", noForm(notDNS), ":https"},
		{"a host with a scheme", "https://slack.com", []string{"holds a scheme", "write the service's host name with no scheme"}, "https"},
		{"a host name in brackets", "[api.github.com]", noForm("holds a bracket", "binds nothing"), "[api"},
		{"a host name holding a %", "api%x.github.com", noForm("holds a %", "binds nothing"), "%x"},
		{"a host name holding a slash", "a/b.example", noForm("holds a character other than a letter, a digit, a hyphen and a dot", "binds nothing"), "a/b"},
		{"a colon in no IP address", "2001:db8::g", noForm(notDNS), "::g"},
		{"an underscore", "a_b.example.com", noForm("holds an underscore", "outside the LDH grammar"), "a_b"},
		{"a label starting with a hyphen", "-api.github.com", noForm("starts or ends with a hyphen"), "-api"},
		{"a label ending in a hyphen", "api-.github.com", noForm("starts or ends with a hyphen"), "api-"},
		{"a label with -- as its third and fourth characters", "ab--cd.example", noForm("as its third and fourth characters", "reserved form"), "ab--cd"},
		{"an xn-- label that is no punycode", "xn--zz.example", noForm("xn-- label the IDNA lookup profile refuses", "Fake A-label"), "xn--zz"},
		// The punycode of the ﬁ ligature: punycode decodes it, and the IDNA lookup profile, which
		// maps ﬁ to fi, refuses it as no A-label.
		{"an xn-- label that decodes but no lookup reads", "xn--le-1b1n.example", noForm("xn-- label the IDNA lookup profile refuses", "Fake A-label"), "xn--le-1b1n"},
		{"a label over 63 characters", longLabel, noForm("label longer than 63 characters", "binds nothing"), strings.Repeat("a", 64)},
		{"a name over 253 characters", longName, noForm("longer than 253 characters", "binds nothing"), strings.Repeat("b", 63)},
		{"a name of 254 characters", longestHost + "c", noForm("longer than 253 characters", "binds nothing"), strings.Repeat("c", 63)},
		{"a trailing dot after text that is no host name", "api github.com.", noForm("ends in a dot", "no host name either", "binds nothing"), "api github"},
		{"two trailing dots", "api.github.com..", noForm("ends in a dot", "no host name either", "binds nothing"), "github.com."},
		// One trailing dot after text Go's resolver does not read as a name either: digits
		// alone, a label with a hyphen at an end, a label over 63 characters, a name over 253.
		{"a trailing dot after numbers alone", "1.2.3.", noForm("ends in a dot", "no host name either", "binds nothing"), "1.2.3"},
		{"a trailing dot after a label starting with a hyphen", "-api.github.com.", noForm("ends in a dot", "no host name either", "binds nothing"), "-api"},
		{"a trailing dot after a label ending in a hyphen", "api-.github.com.", noForm("ends in a dot", "no host name either", "binds nothing"), "api-"},
		{"a trailing dot after a label over 63 characters", strings.Repeat("a", 64) + ".example.", noForm("ends in a dot", "no host name either", "binds nothing"), strings.Repeat("a", 64)},
		{"a trailing dot after a name of 254 characters", longestHost + "c.", noForm("ends in a dot", "no host name either", "binds nothing"), strings.Repeat("c", 63)},
		// One trailing dot after a name the resolver reads and the grammar refuses: a CONNECT
		// with the dot binds and is refused "sni mismatch", and the refusal names why the name
		// without its dot is refused as well.
		{"an underscore before a trailing dot", "a_b.example.com.", noForm(append(decryptedDot, "without the dot it holds an underscore")...), "a_b"},
		{"a -- label before a trailing dot", "ab--cd.example.", noForm(append(decryptedDot, "without the dot it has a label with", "as its third and fourth characters")...), "ab--cd"},
		{"a Fake A-label before a trailing dot", "xn--zz.example.", noForm(append(decryptedDot, "without the dot it has an xn-- label")...), "xn--zz"},
		{"a last label of digits before a trailing dot", "foo.123.", noForm(append(decryptedDot, "without the dot it ends in a number")...), "foo.123"},
		// A wildcard with a trailing dot is read as the names it matches, the shortest of which
		// is "a." and its suffix: a.216.34. and a.123. are names the resolver reads, so the
		// wildcard binds and is decrypted as they are, and a suffix of 252 characters matches
		// no name of 253 or fewer.
		{"a wildcard over the last labels of an address before a trailing dot", "*.216.34.", noForm(append(decryptedDot, "without the dot it is a wildcard whose last label is a number")...), "216.34"},
		{"a wildcard over a number before a trailing dot", "*.123.", noForm(append(decryptedDot, "without the dot it is a wildcard whose last label is a number")...), "123"},
		{"a wildcard over 252 characters before a trailing dot", "*." + strings.Repeat(strings.Repeat("e", 63)+".", 3) + strings.Repeat("e", 60) + ".", noForm("ends in a dot", "no host name either", "binds nothing"), strings.Repeat("e", 60)},
		// The boundary: "a." and a suffix of 251 characters is a name of exactly 253, which the
		// resolver reads, so this wildcard binds and is decrypted with its dot, and its refusal
		// names the wildcard without the dot as the form to write.
		{"a wildcard over 251 characters before a trailing dot", "*." + strings.Repeat(strings.Repeat("f", 63)+".", 3) + strings.Repeat("f", 59) + ".",
			formed("sni mismatch", "*."+strings.Repeat(strings.Repeat("f", 63)+".", 3)+strings.Repeat("f", 59)), ""},
		{"an IPv4 address with a leading zero", "093.184.216.34", noForm("ends in a number", "leading zero", numbered), "093.184"},
		{"an IPv4 address of fewer than four parts", "93.184.54818", noForm("ends in a number", "fewer than four parts", numbered), "184.54818"},
		{"an IPv4 address written as one number", "1572395042", noForm("ends in a number", numbered), "1572395042"},
		{"an IPv4 address written in hex", "0x5db8d822", noForm("ends in a number", "hex part", numbered), "0x5db8"},
		{"a name whose last label is a number", "foo.123", noForm("ends in a number", numbered), "foo.123"},
		{"a name whose last label is 0x alone", "foo.0x", noForm("ends in a number", numbered), "foo.0x"},
		// In upper case the hex is read as its lower case, as the proxy and HostBound read it.
		{"a last label of hex digits in upper case", "foo.0X1F", noForm("ends in a number", numbered), "0X1F"},
		{"one label before 0x alone", "a.0x", noForm("ends in a number", numbered), "a.0x"},
		{"a number before 0x alone", "10.0x", noForm("ends in a number", numbered), "10.0x"},
		{"0x alone", "0x", noForm("ends in a number", numbered), ""},
		{"a wildcard over the last labels of an address", "*.216.34", noForm("wildcard whose last label is a number", "addresses by their text"), "*.216"},
		{"a wildcard over 0x alone", "*.0x", noForm("wildcard whose last label is a number", "addresses by their text"), "*.0x"},
		// The hosts of the airlock package's own HostShapeOK rows that no row above already
		// spells, each refused ahead of airlock.Validate, whose refusal would quote it.
		{"a bare wildcard with a trailing dot", "*.", noForm("ends in a dot", "no host name either"), ""},
		{"a path", "slack.com/x", noForm("holds a character other than a letter, a digit, a hyphen and a dot"), "slack.com/"},
		{"a wildcard inside a name", "*.*.com", noForm("has a * other than one leading"), "*.*"},
		{"a Kelvin sign", "sl\u212aack.com", []string{"is not ASCII"}, "\u212a"},
	}
}()

// longestHost is a name of 253 characters, the longest the grammar reads, and longestLabel a name
// whose first label is 63 characters, the longest label it reads: a row one character past either
// is refused, and these two are read.
var (
	longestHost  = strings.Repeat(strings.Repeat("c", 63)+".", 3) + strings.Repeat("c", 61)
	longestLabel = strings.Repeat("d", 63) + ".example"
)

// canonicalHosts are hosts the grammar reads, one of each shape a deployment's rules file names -
// a name, a wildcard, a name with a hyphen and a digit in a label, an internationalized name as
// its punycode, a single-label name, and a name in upper or mixed case, which airlock.HostShapeOK
// accepts and airlock.HostBound folds on both sides, the punycode in upper case among them, which
// the grammar reads as its lower case - and the edges of the grammar it reads: the longest name,
// the longest label, and a number as a label other than the last.
var canonicalHosts = []string{"api.github.com", "*.googleapis.com", "s3.us-east-1.amazonaws.com", "xn--bcher-kva.example", "localhost",
	"API.GitHub.com", "*.GoogleAPIs.com", "XN--BCHER-KVA.example", longestHost, longestLabel, "123.foo"}

// TestRefusalRefusesEveryHostTheProxyNeverCompares: a rules host is served only in the one grammar
// a rules host is written in, the one spelling the proxy compares a request's host in, and
// Refusal refuses every other spelling saying why, naming the form to write where one exists, and
// giving one advice, with nothing else of the host as written. The control is a host of each
// shape a deployment names, in the grammar's spelling: Refusal serves each.
func TestRefusalRefusesEveryHostTheProxyNeverCompares(t *testing.T) {
	for _, c := range refusalRows {
		t.Run(c.name, func(t *testing.T) {
			why := Refusal(c.host)
			for _, want := range c.want {
				if !strings.Contains(why, want) {
					t.Fatalf("Refusal(%q) = %q, which does not say %q", c.host, why, want)
				}
			}
			for _, absent := range c.absents() {
				if absent != "" && strings.Contains(why, absent) {
					t.Fatalf("Refusal(%q) = %q, which holds %q, which it must not", c.host, why, absent)
				}
			}
		})
	}
	t.Run("the control: a host of each shape a deployment names, in the grammar's spelling", func(t *testing.T) {
		for _, host := range canonicalHosts {
			if why := Refusal(host); why != "" {
				t.Errorf("Refusal(%q) = %q; want a host in the grammar's spelling served", host, why)
			}
		}
	})
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
