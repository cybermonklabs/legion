package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
)

// twoSecrets is a rules file the broker would serve: two entries, one of them with headers of
// its own.
const twoSecrets = `{"secrets": [
  {"name": "GITHUB", "hosts": ["api.github.com", "github.com"]},
  {"name": "OPENAI", "hosts": ["api.openai.com"], "headers": ["authorization", "x-api-key"]}
]}`

// airlockFile writes one rules file for a test and returns its path.
func airlockFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runAirlockCheck drives the subcommand's own function, which is what the binary runs: the
// arguments after the subcommand's name, and the two writers.
func runAirlockCheck(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := airlockCheck(args, &stdout, &stderr)
	t.Logf("broker %s %s = exit %d\nstdout:\n%sstderr:\n%s",
		airlockCheckCommand, strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// TestAirlockCheckReportsAWholeFileAsAdded pins the report a file with no --against gets: it is
// the file's own contents, read against an empty rules file, so every entry and every host of it
// is an addition. The whole of stdout is asserted, because the report's order is its contract
// with the person reading it in a pull request: two runs on the same files say the same thing,
// and the lines of one secret are together.
func TestAirlockCheckReportsAWholeFileAsAdded(t *testing.T) {
	path := airlockFile(t, "airlock.json", twoSecrets)
	code, stdout, stderr := runAirlockCheck(t, path)
	if code != 0 || stderr != "" {
		t.Fatalf("a valid file = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	want := fmt.Sprintf(`airlock-check: %s is valid: 2 secrets
airlock-check: compared against an empty rules file, so every entry reads as added
secret "GITHUB": added
secret "GITHUB": host api.github.com added
secret "GITHUB": host github.com added
secret "GITHUB": header authorization added
secret "OPENAI": added
secret "OPENAI": host api.openai.com added
secret "OPENAI": header authorization added
secret "OPENAI": header x-api-key added
`, path)
	if stdout != want {
		t.Fatalf("the report is\n%s\nwant\n%s", stdout, want)
	}
}

// TestAirlockCheckRefusesAFileTheBrokerWouldNotServe covers every way a file is refused: the
// broker's own airlock package decides, and the line says which entry, or where in the file, so
// that the author of the pull request can find it. Each refusal writes nothing on stdout: a
// report of a file the broker would not serve would be read as a file it had agreed to.
func TestAirlockCheckRefusesAFileTheBrokerWouldNotServe(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want []string
	}{
		{
			"a judge above the longest lifetime",
			`{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"]}, {"name": "ANTHROPIC", "hosts": ["api.anthropic.com"], "judge": 1800}]}`,
			[]string{`secrets[1] "ANTHROPIC"`, "1800", "900"},
		},
		{
			"a judge and no host",
			`{"secrets": [{"name": "ANTHROPIC", "hosts": [], "judge": 600}]}`,
			[]string{`secrets[0] "ANTHROPIC"`, "no host"},
		},
		{
			"a repeated name",
			`{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"]}, {"name": "GITHUB", "hosts": ["github.com"]}]}`,
			[]string{`secrets[1] "GITHUB"`, "repeated"},
		},
		{
			"a file that is not JSON",
			"{\"secrets\": [\n  {\"name\": \"GITHUB\", \"hosts\": [\"api.github.com\"]},\n]}",
			[]string{"not JSON", "byte offset", "line 3", "column 1"},
		},
		{
			"a field the broker does not know",
			`{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"], "allowed": ["GET"]}]}`,
			[]string{"unknown field", "allowed"},
		},
		// A name that is not a name a session can ask for: Rules.Lookup matches a released
		// name exactly, so such an entry holds nothing back, and the file would be reviewed
		// as covering a secret it does not cover.
		{
			"an entry name in lower case",
			`{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"]}, {"name": "legion_slack_bot_token", "hosts": ["slack.com"]}]}`,
			[]string{`secrets[1] "legion_slack_bot_token"`, "not an agent secret name", "holds nothing back"},
		},
		{
			"an entry name written with hyphens",
			`{"secrets": [{"name": "LEGION-SLACK-BOT-TOKEN", "hosts": ["slack.com"]}]}`,
			[]string{`secrets[0] "LEGION-SLACK-BOT-TOKEN"`, "not an agent secret name", "holds nothing back"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := airlockFile(t, "airlock.json", c.body)
			code, stdout, stderr := runAirlockCheck(t, path)
			if code != 1 {
				t.Fatalf("%s = exit %d, want 1", c.name, code)
			}
			if stdout != "" {
				t.Fatalf("a refused file still printed a report: %q", stdout)
			}
			if lines := strings.Count(strings.TrimRight(stderr, "\n"), "\n") + 1; lines != 1 {
				t.Fatalf("the refusal is %d lines, want one: %q", lines, stderr)
			}
			if !strings.Contains(stderr, path) {
				t.Fatalf("the refusal does not name the file %s: %q", path, stderr)
			}
			// The file's path is the row's temporary directory, which holds the subtest's name,
			// so every want is looked for in the refusal with the path taken out.
			text := strings.ReplaceAll(stderr, path, "")
			for _, want := range c.want {
				if !strings.Contains(text, want) {
					t.Fatalf("the refusal does not say %q: %q", want, stderr)
				}
			}
		})
	}
}

// ruleHostRow is one host spelling the broker refuses: what the refusal must say, and the text of
// the host as written it must not repeat ("" where there is no such text to look for).
type ruleHostRow struct {
	name   string
	host   string
	want   []string
	absent string
}

// ruleHostRows is every host spelling outside the one grammar a rules host is written in
// (rulehost.Refusal), one row per way out of it, each refused by airlock-check
// (TestAirlockCheckRefusesAHostTheProxyNeverCompares) and by the boot
// (TestMainRefusesAnAirlockRulesFileItCannotServe) alike. A row names the form to write where the
// host has one, and no form otherwise.
var ruleHostRows = func() []ruleHostRow {
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
		return append(why, "write a host name of letters, digits and hyphens")
	}
	const numbered = "clients read such a host differently"
	const noDNSName = "is no DNS name"
	// decryptedDot is the reason of one trailing dot after a name the resolver reads: a CONNECT
	// that writes the dot binds the entry and is decrypted, and every request it carries is
	// refused, whatever else is wrong with the name.
	decryptedDot := []string{"ends in a dot", "proxy decrypts a CONNECT to it", "sni mismatch"}
	longLabel := strings.Repeat("a", 64) + ".example"
	longName := strings.Repeat(strings.Repeat("b", 63)+".", 4) + "example"
	return []ruleHostRow{
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
		{"a name of 253 characters with a trailing dot", longestRuleHost + ".", formed("ends in a dot", longestRuleHost), longestRuleHost + "."},
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
		{"a scheme with one slash", "https:/slack.com", noForm(noDNSName), "https:"},
		{"a user and a password", "user:pw@slack.com", noForm(noDNSName), "pw@"},
		{"a mail address", "mailto:ops@slack.com", noForm(noDNSName), "ops@"},
		{"a host name with a service for its port", "api.github.com:https", noForm(noDNSName), ":https"},
		{"a host with a scheme", "https://slack.com", []string{"holds a scheme", "write the service's host name with no scheme"}, "https"},
		{"a host name in brackets", "[api.github.com]", noForm("holds a bracket", "binds nothing"), "[api"},
		{"a host name holding a %", "api%x.github.com", noForm("holds a %", "binds nothing"), "%x"},
		{"a host name holding a slash", "a/b.example", noForm("holds a character other than a letter, a digit, a hyphen and a dot", "binds nothing"), "a/b"},
		{"a colon in no IP address", "2001:db8::g", noForm(noDNSName), "::g"},
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
		{"a name of 254 characters", longestRuleHost + "c", noForm("longer than 253 characters", "binds nothing"), strings.Repeat("c", 63)},
		{"a trailing dot after text that is no host name", "api github.com.", noForm("ends in a dot", "no host name either", "binds nothing"), "api github"},
		{"two trailing dots", "api.github.com..", noForm("ends in a dot", "no host name either", "binds nothing"), "github.com."},
		// One trailing dot after a name the resolver reads and the grammar refuses: a CONNECT
		// with the dot binds and is refused "sni mismatch", and the refusal names why the name
		// without its dot is refused as well.
		{"an underscore before a trailing dot", "a_b.example.com.", noForm(append(decryptedDot, "without the dot it holds an underscore")...), "a_b"},
		{"a -- label before a trailing dot", "ab--cd.example.", noForm(append(decryptedDot, "without the dot it has a label with", "as its third and fourth characters")...), "ab--cd"},
		{"a Fake A-label before a trailing dot", "xn--zz.example.", noForm(append(decryptedDot, "without the dot it has an xn-- label")...), "xn--zz"},
		{"a last label of digits before a trailing dot", "foo.123.", noForm(append(decryptedDot, "without the dot it ends in a number")...), "foo.123"},
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

// longestRuleHost is a name of 253 characters, the longest the grammar reads, and
// longestRuleLabel a name whose first label is 63 characters, the longest label it reads: a row
// one character past either is refused, and these two are read.
var (
	longestRuleHost  = strings.Repeat(strings.Repeat("c", 63)+".", 3) + strings.Repeat("c", 61)
	longestRuleLabel = strings.Repeat("d", 63) + ".example"
)

// canonicalRuleHosts are hosts the grammar reads, one of each shape a deployment's rules file
// names - a name, a wildcard, a name with a hyphen and a digit in a label, an internationalized
// name as its punycode, a single-label name, and a name in upper or mixed case, which
// airlock.HostShapeOK accepts and airlock.HostBound folds on both sides, the punycode in upper
// case among them, which the grammar reads as its lower case - and the edges of the grammar it
// reads: the longest name, the longest label, and a number as a label other than the last.
var canonicalRuleHosts = []string{"api.github.com", "*.googleapis.com", "s3.us-east-1.amazonaws.com", "xn--bcher-kva.example", "localhost",
	"API.GitHub.com", "*.GoogleAPIs.com", "XN--BCHER-KVA.example", longestRuleHost, longestRuleLabel, "123.foo"}

// rulesWithHosts is a rules file of one entry, name, that binds hosts, written by encoding/json
// so that a host holding a quotation mark, a control character or Unicode is the host the row
// means.
func rulesWithHosts(t *testing.T, name string, hosts ...string) string {
	t.Helper()
	type entry struct {
		Name  string   `json:"name"`
		Hosts []string `json:"hosts"`
	}
	body, err := json.Marshal(struct {
		Secrets []entry `json:"secrets"`
	}{[]entry{{name, hosts}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestAirlockCheckRefusesAHostTheProxyNeverCompares: a rules host is served only in the one
// grammar a rules host is written in, the one spelling the proxy compares a request's host in,
// and airlock-check refuses every other spelling as the boot does, naming the entry and the
// host's position, saying why, and naming the form to write where one exists, with nothing else
// of the host as written. The control is a file of hosts of each shape a deployment names, in the
// grammar's spelling: it is valid.
func TestAirlockCheckRefusesAHostTheProxyNeverCompares(t *testing.T) {
	for _, c := range ruleHostRows {
		t.Run(c.name, func(t *testing.T) {
			path := airlockFile(t, "airlock.json", rulesWithHosts(t, "GITHUB", "api.github.com", c.host))
			code, stdout, stderr := runAirlockCheck(t, path)
			if code != 1 || stdout != "" {
				t.Fatalf("%s = exit %d with stdout %q, want exit 1 and no report", c.name, code, stdout)
			}
			// The file's path is the row's temporary directory, which holds the subtest's name,
			// and a subtest's name holds words a want looks for, such as "underscore": every want,
			// and the text that must be absent, is looked for with the path taken out.
			text := strings.ReplaceAll(stderr, path, "")
			for _, want := range append([]string{`secrets[0] "GITHUB"`, "hosts[1]"}, c.want...) {
				if !strings.Contains(text, want) {
					t.Fatalf("the refusal does not say %q: %q", want, stderr)
				}
			}
			if c.absent != "" && strings.Contains(text, c.absent) {
				t.Fatalf("the refusal repeats %q as the file writes it: %q", c.absent, stderr)
			}
		})
	}
	t.Run("the control: a host of each shape a deployment names, in the grammar's spelling", func(t *testing.T) {
		path := airlockFile(t, "airlock.json", rulesWithHosts(t, "GITHUB", canonicalRuleHosts...))
		if code, _, stderr := runAirlockCheck(t, path); code != 0 || stderr != "" {
			t.Fatalf("a file of hosts in the grammar's spelling = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
		}
	})
}

// TestAirlockCheckReportsWhatChangedAgainstTheOldFile is the report a reviewer reads: a host
// added, a header added, an entry removed with the host it held, and the three fields that
// decide the rest of where a credential may go, each as old → new.
func TestAirlockCheckReportsWhatChangedAgainstTheOldFile(t *testing.T) {
	old := airlockFile(t, "old.json", `{"secrets": [
  {"name": "GITHUB", "hosts": ["api.github.com"], "allow": [{"method": "GET", "pathPrefix": "/v1/"}], "judge": 300},
  {"name": "LEGACY", "hosts": ["old.example.com"]}
]}`)
	now := airlockFile(t, "airlock.json", `{"secrets": [
  {"name": "GITHUB", "hosts": ["api.github.com", "uploads.github.com"], "headers": ["authorization", "x-api-key"],
   "allow": [{"method": "POST", "pathPrefix": "/v1/"}], "kind": "aws", "judge": 600}
]}`)
	code, stdout, stderr := runAirlockCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("a valid file against an old one = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	want := fmt.Sprintf(`airlock-check: %s is valid: 1 secret
airlock-check: compared against %s
secret "GITHUB": host uploads.github.com added
secret "GITHUB": header x-api-key added
secret "GITHUB": allow GET /v1/ → POST /v1/
secret "GITHUB": kind "" → aws
secret "GITHUB": judge 300 seconds → 600 seconds
secret "LEGACY": removed
secret "LEGACY": host old.example.com removed
secret "LEGACY": header authorization removed
`, now, old)
	if stdout != want {
		t.Fatalf("the report is\n%s\nwant\n%s", stdout, want)
	}
}

// TestAirlockCheckReportsAHostRemovedAndAHeaderRemoved is the other direction of the same
// report, and the default headers with it: an entry that names no header carries the secret in
// authorization, so a file that writes "headers": [] has taken that header away, and the report
// says so rather than treating the two as the same empty field.
func TestAirlockCheckReportsAHostRemovedAndAHeaderRemoved(t *testing.T) {
	old := airlockFile(t, "old.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com", "github.com"]}]}`)
	now := airlockFile(t, "airlock.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"], "headers": []}]}`)
	code, stdout, stderr := runAirlockCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	for _, want := range []string{
		`secret "GITHUB": host github.com removed`,
		`secret "GITHUB": header authorization removed`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the report does not say %q:\n%s", want, stdout)
		}
	}
}

// TestAirlockCheckReportsTheDefaultHeaderTheSwapApplies: the header an entry with no "headers"
// field reads as, in this report, is the one airlock.Rewrite actually swaps in, read from the
// airlock package itself. The report held a second copy of that default, so a change to it on
// one side would have made the report say a header was added or removed that the swap says
// nothing about, and no test would have failed.
func TestAirlockCheckReportsTheDefaultHeaderTheSwapApplies(t *testing.T) {
	names := airlock.SecretRule{}.HeaderNames()
	if len(names) != 1 {
		t.Fatalf("an entry with no headers field names %v; this report reads it as one header", names)
	}
	added := airlockFile(t, "airlock.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"]}]}`)
	code, stdout, stderr := runAirlockCheck(t, added)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if want := fmt.Sprintf("secret %q: header %s added\n", "GITHUB", names[0]); !strings.Contains(stdout, want) {
		t.Fatalf("the report does not say %q:\n%s", want, stdout)
	}
}

// TestAirlockCheckSaysWhenNothingChanged pins the one line a pull request that changed something
// else gets: a report of no lines at all would read as a report that had not run.
func TestAirlockCheckSaysWhenNothingChanged(t *testing.T) {
	old := airlockFile(t, "old.json", twoSecrets)
	now := airlockFile(t, "airlock.json", twoSecrets)
	code, stdout, stderr := runAirlockCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if !strings.HasSuffix(stdout, "airlock-check: nothing changed\n") {
		t.Fatalf("an unchanged file's report does not end in the nothing-changed line:\n%s", stdout)
	}
	if strings.Contains(stdout, " added") || strings.Contains(stdout, " removed") {
		t.Fatalf("an unchanged file's report claims a change:\n%s", stdout)
	}
}

// TestAirlockCheckRefusesAnOldFileItCannotRead refuses with 1 and names the --against file: the
// report is the point of the run, so an old file that is not there is a failure and not a run
// against nothing.
func TestAirlockCheckRefusesAnOldFileItCannotRead(t *testing.T) {
	now := airlockFile(t, "airlock.json", twoSecrets)
	missing := filepath.Join(t.TempDir(), "gone.json")
	code, stdout, stderr := runAirlockCheck(t, now, "--against", missing)
	if code != 1 {
		t.Fatalf("--against a file that is not there = exit %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("it printed a report anyway: %q", stdout)
	}
	if !strings.Contains(stderr, missing) {
		t.Fatalf("the refusal does not name %s: %q", missing, stderr)
	}
}

// TestAirlockCheckComparesAgainstAnOldFileTheBrokerWouldRefuse is why the old file is parsed and
// not validated: the rules in force may predate a refusal this broker makes, and saying what
// changed is the whole point of the run.
func TestAirlockCheckComparesAgainstAnOldFileTheBrokerWouldRefuse(t *testing.T) {
	old := airlockFile(t, "old.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"], "judge": 1800}]}`)
	now := airlockFile(t, "airlock.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com"], "judge": 900}]}`)
	code, stdout, stderr := runAirlockCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if want := `secret "GITHUB": judge 1800 seconds → 900 seconds`; !strings.Contains(stdout, want) {
		t.Fatalf("the report does not say %q:\n%s", want, stdout)
	}
}

// TestAirlockCheckRefusesAUsageError keeps the usage errors apart from the file errors: exit 2
// and the usage line, as the end-sessions subcommand answers its own.
func TestAirlockCheckRefusesAUsageError(t *testing.T) {
	path := airlockFile(t, "airlock.json", twoSecrets)
	for _, c := range []struct {
		name string
		args []string
	}{
		{"no file", nil},
		{"a blank file name", []string{"  "}},
		{"two files", []string{path, path}},
		{"a second positional after the flag", []string{path, "--against", path, path}},
		{"an unknown flag", []string{"--compare-to", path}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runAirlockCheck(t, c.args...)
			if code != 2 {
				t.Fatalf("%s = exit %d, want 2", c.name, code)
			}
			if stdout != "" {
				t.Fatalf("a usage error printed a report: %q", stdout)
			}
			if !strings.Contains(stderr, airlockCheckUsage) {
				t.Fatalf("a usage error printed no usage line: %q", stderr)
			}
		})
	}
}

// TestTheBinaryRoutesBothSubcommandWords drives the built binary, the one thing the function
// tests above cannot see: `airlock-check` reaches this command, on a machine with no broker
// configuration and no database, and `end-sessions` still reaches its own rather than being
// swallowed by the new one.
func TestTheBinaryRoutesBothSubcommandWords(t *testing.T) {
	binPath := buildBroker(t)
	path := airlockFile(t, "airlock.json", twoSecrets)

	out, err := exec.Command(binPath, airlockCheckCommand, path).CombinedOutput()
	if err != nil {
		t.Fatalf("broker %s %s = %v\n%s", airlockCheckCommand, path, err, out)
	}
	t.Logf("broker %s %s printed:\n%s", airlockCheckCommand, path, out)
	for _, want := range []string{"is valid: 2 secrets", `secret "OPENAI": host api.openai.com added`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("the binary's report does not say %q: %s", want, out)
		}
	}

	out, err = exec.Command(binPath, endSessionsCommand).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("broker %s with no login = %v, want exit 2\n%s", endSessionsCommand, err, out)
	}
	if want := "usage: broker " + endSessionsCommand + " <login>"; !strings.Contains(string(out), want) {
		t.Fatalf("broker %s with no login does not print its own usage line %q: %s", endSessionsCommand, want, out)
	}
}
