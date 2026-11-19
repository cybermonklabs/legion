package main

import (
	"bytes"
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
			"guidance longer than the judge reads",
			`{"secrets": [{"name": "ANTHROPIC", "judge": 600, "guidance": "` + strings.Repeat("g", 2001) + `"}]}`,
			[]string{`secrets[0] "ANTHROPIC"`, "guidance", "2001", "2000"},
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
			for _, want := range c.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("the refusal does not say %q: %q", want, stderr)
				}
			}
		})
	}
}

// TestAirlockCheckRefusesAHostTheProxyNeverCompares is one row for each spelling of a host that
// binds no secret, so that the entry binds nothing; airlock-check refuses each as the boot does.
// An IP address binds none in any spelling: a client sends no TLS server name for one, so the
// canonical form is refused "sni mismatch", and the proxy, which takes a CONNECT host's port and
// brackets off and writes an address as netaddr.Host writes it, compares no other form at all;
// each is refused with the one reason, before airlock.Validate, whose refusal would quote the
// host. A host that is no address is refused naming no form. No refusal repeats the host as
// written, whose zone or label is any text. The control is a file of host names in upper or
// mixed case, which fold.ASCII alone changes and HostBound folds on both sides: it is valid.
func TestAirlockCheckRefusesAHostTheProxyNeverCompares(t *testing.T) {
	const ipAddress = "binds no secret in any spelling"
	// bindsNothing is what each row says but the wildcard's, which binds addresses by their text.
	bindsNothing := func(want ...string) []string { return append([]string{"binds nothing"}, want...) }
	noDNSName := []string{"is no DNS name", "binds it nowhere", "host name"}
	for _, c := range []struct {
		name   string
		host   string
		want   []string
		absent string
	}{
		// An address in its canonical form, and in a form netaddr.Host rewrites.
		{"an IPv4 address", "192.0.2.10", []string{ipAddress, "sni mismatch", "host name"}, "192.0.2.10"},
		{"an IPv6 address", "2001:db8:7::1", []string{ipAddress, "sni mismatch", "host name"}, "2001:db8:7::1"},
		{"an IPv6 address in upper case", "2001:DB8:8::1", []string{ipAddress, "host name"}, "DB8"},
		{"an IPv6 address not written in its canonical form", "2001:db8:7:0::1", []string{ipAddress, "host name"}, "2001:db8:7:0::1"},
		{"an IPv6 address with a leading zero in a group", "2001:0db8:7::1", []string{ipAddress, "host name"}, "0db8"},
		{"an IPv4-mapped IPv6 address", "::ffff:192.0.2.10", []string{ipAddress, "host name"}, "::ffff:"},
		{"an IPv6 address with an embedded IPv4 part", "2001:db8::192.0.2.10", []string{ipAddress, "host name"}, "::192.0"},
		{"an IPv6 address with a zone", `2001:db8:7::1%eth0 \"x`, []string{ipAddress, "host name"}, "eth0"},
		// An address in a spelling netaddr.Host does not read.
		{"an IPv6 address in brackets", "[2001:db8:7::1]", []string{ipAddress, "host name"}, "[2001"},
		{"an IPv6 address with a zone in brackets", `[2001:db8:7::1%eth0 \"x]`, []string{ipAddress, "host name"}, "eth0"},
		{"an IPv4-mapped IPv6 address in brackets", "[::ffff:192.0.2.10]", []string{ipAddress, "host name"}, "::ffff:"},
		{"an IPv6 address after an opening bracket alone", "[2001:db8:7::1", []string{ipAddress, "host name"}, "[2001"},
		{"an IPv4 address with a trailing dot", "192.0.2.10.", []string{ipAddress, "host name"}, "2.10."},
		{"an IPv6 address with a trailing dot", "2001:db8:7::1.", []string{ipAddress, "host name"}, "::1."},
		{"an IPv4 address with a port", "192.0.2.10:443", []string{ipAddress, "host name"}, ":443"},
		{"an IPv6 address in brackets with a port", "[2001:db8:7::1]:443", []string{ipAddress, "host name"}, "]:443"},
		// A host that holds no address.
		{"a host name with a port", "api.github.com:443", bindsNothing("holds a port", "write the host name alone"), ":443"},
		{"a host name in brackets", "[api.github.com]", bindsNothing("bracket or a %", "no IP address"), "[api"},
		{"a host name holding a %", "api%x.github.com", bindsNothing("bracket or a %", "no IP address"), "%x"},
		{"a colon in no IP address", "2001:db8::g", bindsNothing("holds a colon", "no IP address"), "::g"},
		{"an IPv4 address with a leading zero", "093.184.216.34", bindsNothing("ends in a number", "leading zero"), "093.184"},
		{"an IPv4 address of fewer than four parts", "93.184.54818", bindsNothing("ends in a number", "fewer than four parts"), "184.54818"},
		{"an IPv4 address written as one number", "1572395042", bindsNothing("ends in a number", "fewer than four parts"), "1572395042"},
		{"an IPv4 address written in hex", "0x5db8d822", bindsNothing("ends in a number", "hex part"), "0x5db8"},
		{"a wildcard over the last labels of an address", "*.216.34", []string{"wildcard", "last label is a number", "match addresses by their text"}, "*.216"},
		// A host name with a trailing dot matches only a request that writes the dot, which no
		// client does for a name it was given without one; the form named is the folded name.
		{"a host name with a trailing dot", "api.github.com.", bindsNothing("trailing dot", "write it as api.github.com"), "github.com."},
		{"a host name in mixed case with a trailing dot", "API.GitHub.com.", bindsNothing("trailing dot", "write it as api.github.com"), "GitHub"},
		{"a wildcard with a trailing dot", "*.googleapis.com.", bindsNothing("trailing dot", "write it as *.googleapis.com"), "googleapis.com."},
		{"a trailing dot after text that is no host name", "api github.com.", noDNSName, "api github"},
		{"an underscore before a trailing dot", "a_b.example.com.", noDNSName, "a_b"},
		{"an empty host", "", bindsNothing("is empty"), `""`},
		// A host airlock.HostShapeOK refuses for any other reason is refused as no DNS name,
		// naming nothing of it, ahead of airlock.Validate, whose refusal quotes the host; and a
		// scheme is read as a scheme, not as the "host" net.SplitHostPort would make of it.
		{"a host with a scheme", "https://slack.com", bindsNothing("holds a scheme"), "https"},
		{"a host with a path", "slack.com/x", noDNSName, "slack.com/"},
		{"a bare wildcard", "*", noDNSName, "*"},
		{"a wildcard alone with a trailing dot", "*.", noDNSName, "*."},
		{"a leading dot", ".slack.com", noDNSName, ".slack"},
		{"an empty label", "slack..com", noDNSName, "slack.."},
		{"a label opening with a hyphen", "-slack.com", noDNSName, "-slack"},
		{"a label closing with a hyphen", "slack-.com", noDNSName, "slack-."},
		{"an underscore", "slack_com", noDNSName, "slack_com"},
		{"a wildcard inside a name", "*.*.com", noDNSName, "*.*"},
		{"a character no label holds", "sl\u212aack.com", noDNSName, "\u212a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := airlockFile(t, "airlock.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com", "`+c.host+`"]}]}`)
			code, stdout, stderr := runAirlockCheck(t, path)
			if code != 1 || stdout != "" {
				t.Fatalf("%s = exit %d with stdout %q, want exit 1 and no report", c.name, code, stdout)
			}
			for _, want := range append([]string{`secrets[0] "GITHUB"`, "hosts[1]"}, c.want...) {
				if !strings.Contains(stderr, want) {
					t.Fatalf("the refusal does not say %q: %q", want, stderr)
				}
			}
			if strings.Contains(stderr, c.absent) {
				t.Fatalf("the refusal repeats %q as the file writes it: %q", c.absent, stderr)
			}
		})
	}
	t.Run("the control: host names in the form the proxy compares, in any case", func(t *testing.T) {
		path := airlockFile(t, "airlock.json", `{"secrets": [{"name": "GITHUB", "hosts": ["api.github.com", "API.GitHub.com", "*.googleapis.com", "s3.us-east-1.amazonaws.com", "xn--bcher-kva.example"]}]}`)
		if code, _, stderr := runAirlockCheck(t, path); code != 0 || stderr != "" {
			t.Fatalf("a file of host names in the form the proxy compares = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
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
   "allow": [{"method": "POST", "pathPrefix": "/v1/"}], "kind": "aws", "judge": 600, "guidance": "grant for the release job"}
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
secret "GITHUB": guidance "" → "grant for the release job"
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
