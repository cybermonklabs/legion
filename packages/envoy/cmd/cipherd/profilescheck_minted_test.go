package main

import (
	"bytes"
	"strings"
	"testing"
)

// checkProfiles runs `profiles check` on the file profiles holds with args after it, and answers
// its exit status, its stdout and its stderr.
func checkProfiles(t *testing.T, profiles string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runProfilesCheck(append([]string{profilesFile(t, profiles)}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestProfilesCheckRefusesWhatTheBootRefusesOfADPoPBlock: `profiles check` refuses, exit 1, naming
// the profile and the fault, every dpop block the boot refuses for its shape alone - a "*." host,
// two hosts, no allow list, a header other than Authorization, a block of no kind over its host -
// whatever its arguments (config.MintedEntryRules). Given --private-upstream, the value the
// broker's BROKER_PROXY_PRIVATE_UPSTREAM holds, it also refuses a dpop block of another host than
// the destination's, and takes the destination's with no line saying anything was not checked.
// Without the flag it takes that block, exits 0 and writes one line saying the block's tie to the
// destination was not checked, so the command says what it could not see.
func TestProfilesCheckRefusesWhatTheBootRefusesOfADPoPBlock(t *testing.T) {
	plain := `{"name": "WILDCARD_TOKEN", "proxy": {"hosts": ["*.test"]}}`
	for _, c := range []struct{ name, profiles, says string }{
		{"a *. host", mintedProfiles(`"*.test"`, []string{"FIRST_TOKEN"}), "one exact host"},
		{"two hosts", mintedProfiles(`"dispatch.test", "other.test"`, []string{"FIRST_TOKEN"}), "one exact host"},
		{"no allow list", `{"profiles": [{"name": "FIRST_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "kind": "dpop"}}]}`, "allow list"},
		{"a header X-Token", `{"profiles": [{"name": "FIRST_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["X-Token"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}]}`, "Authorization header alone"},
		{"an allow entry of every path", `{"profiles": [{"name": "FIRST_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/"}]}}]}`, "path prefix"},
		{"a block of no kind over the host", mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN"}, plain), "WILDCARD_TOKEN"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := checkProfiles(t, c.profiles)
			if code != 1 || !strings.Contains(stderr, "FIRST_TOKEN") || !strings.Contains(stderr, c.says) {
				t.Fatalf("profiles check of %s = exit %d, stdout %q, stderr %q; want exit 1 naming FIRST_TOKEN and %q", c.name, code, stdout, stderr, c.says)
			}
		})
	}
	const destination = "dispatch.test=http://10.0.0.5:8766"
	right := mintedProfiles(`"dispatch.test"`, []string{"FIRST_TOKEN", "SECOND_TOKEN"})
	if code, stdout, stderr := checkProfiles(t, mintedProfiles(`"evil.test"`, []string{"FIRST_TOKEN"}), "--private-upstream", destination); code != 1 || !strings.Contains(stderr, "evil.test") || !strings.Contains(stderr, "BROKER_PROXY_PRIVATE_UPSTREAM") {
		t.Fatalf("profiles check of a dpop block of another host with --private-upstream = exit %d, stdout %q, stderr %q; want exit 1 naming evil.test and the setting", code, stdout, stderr)
	}
	if code, stdout, stderr := checkProfiles(t, right, "--private-upstream", destination); code != 0 || strings.Contains(stdout, "not checked") {
		t.Fatalf("profiles check of the destination's dpop blocks with --private-upstream = exit %d, stdout %q, stderr %q; want exit 0 and no line saying something was not checked", code, stdout, stderr)
	}
	code, stdout, stderr := checkProfiles(t, right)
	if code != 0 || !strings.Contains(stdout, "FIRST_TOKEN binds dispatch.test; its tie to BROKER_PROXY_PRIVATE_UPSTREAM was not checked (no --private-upstream)") {
		t.Fatalf("profiles check of dpop blocks without --private-upstream = exit %d, stdout %q, stderr %q; want exit 0 and the not-checked line for each block", code, stdout, stderr)
	}
	if code, _, stderr := checkProfiles(t, right, "--private-upstream", "dispatch.test=http://u:pw@10.0.0.5:8766"); code != 2 || !strings.Contains(stderr, "carries a user") || strings.Contains(stderr, "pw") {
		t.Fatalf("profiles check with a --private-upstream carrying a user = exit %d, stderr %q; want exit 2 naming the fault and quoting no credential", code, stderr)
	}
	// A destination the broker's start refuses because no dial reaches it is refused here too, as
	// a value BROKER_PROXY_PRIVATE_UPSTREAM does not take: profiles check knows no dial allow.
	for _, value := range []string{"dispatch.test=http://127.0.0.1:8766", "dispatch.test=http://localhost:8766"} {
		if code, _, stderr := checkProfiles(t, right, "--private-upstream", value); code != 2 || !strings.Contains(stderr, "BROKER_PROXY_PRIVATE_UPSTREAM") || !strings.Contains(stderr, "loopback") {
			t.Errorf("profiles check with --private-upstream %s = exit %d, stderr %q; want exit 2 naming the setting and loopback, as the broker's start refuses it", value, code, stderr)
		}
	}
	// Two destinations, each bound by a dpop block of its own host, pass; a destination no block
	// binds is refused, naming its host; and a host named twice is no value the setting takes.
	const both = "dispatch.test=http://10.0.0.5:8766 results.test=http://10.0.0.6:8766"
	eachOwn := `{"profiles": [` +
		`{"name": "FIRST_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}, ` +
		`{"name": "RESULTS_TOKEN", "proxy": {"hosts": ["results.test"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}]}`
	if code, stdout, stderr := checkProfiles(t, eachOwn, "--private-upstream", both); code != 0 {
		t.Errorf("profiles check of two destinations each bound by its own block = exit %d, stdout %q, stderr %q; want exit 0", code, stdout, stderr)
	}
	if code, _, stderr := checkProfiles(t, right, "--private-upstream", both); code != 1 || !strings.Contains(stderr, "results.test") {
		t.Errorf("profiles check with a destination no block binds = exit %d, stderr %q; want exit 1 naming results.test", code, stderr)
	}
	if code, _, stderr := checkProfiles(t, right, "--private-upstream", destination+" "+destination); code != 2 || !strings.Contains(stderr, "twice") {
		t.Errorf("profiles check with a host named twice = exit %d, stderr %q; want exit 2 naming the host twice", code, stderr)
	}
}
