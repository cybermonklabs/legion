package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// twoProfiles is a profiles file the broker would serve: two stored values behind the proxy, one
// of them with a tier and headers of its own.
const twoProfiles = `{"profiles": [
  {"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com", "github.com"]}},
  {"name": "OPENAI", "tier": "agent", "proxy": {"hosts": ["api.openai.com"], "headers": ["authorization", "x-api-key"]}}
]}`

// runCheck drives the subcommand's own function, which is what the binary runs: the arguments
// after `profiles check`, and the two writers.
func runCheck(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runProfilesCheck(args, &stdout, &stderr)
	t.Logf("broker profiles check %s = exit %d\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// TestProfilesCheckReportsAWholeFileAsAdded pins the report a file with no --against gets: the
// file's own profiles, read against an empty profiles file, so every profile is an addition and
// every field of it is written out, the ones the file leaves to the source among them. The whole
// of stdout is asserted, because the report's order is its contract with the person reading it in
// a pull request: two runs on the same files say the same thing, and the lines of one profile are
// together.
func TestProfilesCheckReportsAWholeFileAsAdded(t *testing.T) {
	path := profilesFile(t, twoProfiles)
	code, stdout, stderr := runCheck(t, path)
	if code != 0 || stderr != "" {
		t.Fatalf("a valid file = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	want := fmt.Sprintf(`profiles check: %s is valid: 2 profiles
profiles check: compared against an empty profiles file, so every profile reads as added
profile "GITHUB_ADMIN_PAT": added
profile "GITHUB_ADMIN_PAT": source GITHUB_ADMIN_PAT
profile "GITHUB_ADMIN_PAT": tier the source's
profile "GITHUB_ADMIN_PAT": requesters the source's
profile "GITHUB_ADMIN_PAT": maxGrant the source's
profile "GITHUB_ADMIN_PAT": github none
profile "GITHUB_ADMIN_PAT": google none
profile "GITHUB_ADMIN_PAT": proxy.hosts api.github.com
profile "GITHUB_ADMIN_PAT": proxy.hosts github.com
profile "GITHUB_ADMIN_PAT": proxy.headers authorization
profile "GITHUB_ADMIN_PAT": proxy.allow any method and path
profile "GITHUB_ADMIN_PAT": proxy.kind ""
profile "GITHUB_ADMIN_PAT": proxy.judge none
profile "GITHUB_ADMIN_PAT": proxy.guidance ""
profile "OPENAI": added
profile "OPENAI": source OPENAI
profile "OPENAI": tier agent
profile "OPENAI": requesters the source's
profile "OPENAI": maxGrant the source's
profile "OPENAI": github none
profile "OPENAI": google none
profile "OPENAI": proxy.hosts api.openai.com
profile "OPENAI": proxy.headers authorization
profile "OPENAI": proxy.headers x-api-key
profile "OPENAI": proxy.allow any method and path
profile "OPENAI": proxy.kind ""
profile "OPENAI": proxy.judge none
profile "OPENAI": proxy.guidance ""
`, path)
	if stdout != want {
		t.Fatalf("the report is\n%s\nwant\n%s", stdout, want)
	}
}

// everyField is a profiles file that sets every field a profile has, over three profiles, since a
// profile holds a github block or a google block and never both: GITHUB_READ sets every field but
// google and the proxy's allow, kind, judge and guidance, GOOGLE_READ sets google, and AWS_KEY sets
// the rest of the proxy block.
const everyField = `{"profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon", "ada@example.com"], "maxGrant": "1h30m",
   "github": {"accounts": ["example-org"], "repositories": ["example-org/legion"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
   "proxy": {"hosts": ["api.github.com"], "headers": ["authorization", "x-github-token"]}},
  {"name": "GOOGLE_READ", "source": "GCS_READER", "google": {"scopes": ["devstorage.read_only"]}},
  {"name": "AWS_KEY", "proxy": {"hosts": ["*.amazonaws.com"], "allow": [{"method": "GET", "pathPrefix": "/"}], "kind": "aws", "judge": 300, "guidance": "grant it for the backup job"}}
]}`

// everyFieldLines is, for each field of a profile by its JSON path (a block's fields under the
// block's name), the lines the report of everyField as an added file must hold for it: the field
// and the value the file gives it.
var everyFieldLines = map[string][]string{
	"name":                {`profile "GITHUB_READ": added`, `profile "GOOGLE_READ": added`, `profile "AWS_KEY": added`},
	"source":              {`profile "GITHUB_READ": source LEGION_IMPLEMENT_APP`, `profile "GOOGLE_READ": source GCS_READER`},
	"tier":                {`profile "GITHUB_READ": tier agent`},
	"requesters":          {`profile "GITHUB_READ": requesters ada@example.com`, `profile "GITHUB_READ": requesters legion-daemon`},
	"maxGrant":            {`profile "GITHUB_READ": maxGrant 1h30m0s`},
	"github.accounts":     {`profile "GITHUB_READ": github.accounts example-org`},
	"github.repositories": {`profile "GITHUB_READ": github.repositories example-org/legion`},
	"github.permissions":  {`profile "GITHUB_READ": github.permissions contents read`, `profile "GITHUB_READ": github.permissions metadata read`},
	"github.value":        {`profile "GITHUB_READ": github.value token`},
	"google.scopes":       {`profile "GOOGLE_READ": google.scopes devstorage.read_only`},
	"proxy.hosts":         {`profile "GITHUB_READ": proxy.hosts api.github.com`, `profile "AWS_KEY": proxy.hosts *.amazonaws.com`},
	"proxy.headers":       {`profile "GITHUB_READ": proxy.headers x-github-token`},
	"proxy.allow":         {`profile "AWS_KEY": proxy.allow GET /`},
	"proxy.kind":          {`profile "AWS_KEY": proxy.kind aws`},
	"proxy.judge":         {`profile "AWS_KEY": proxy.judge 300 seconds`},
	"proxy.guidance":      {`profile "AWS_KEY": proxy.guidance "grant it for the backup job"`},
}

// profileFieldPaths is every JSON field of policy.Profile, a block's fields (GitHubBlock,
// GoogleBlock, ProxyBlock) under the block's own field name, read from the types themselves, so a
// field added to a profile is a field this report's test asks about.
func profileFieldPaths(t *testing.T) []string {
	t.Helper()
	blocks := []reflect.Type{reflect.TypeFor[policy.GitHubBlock](), reflect.TypeFor[policy.GoogleBlock](), reflect.TypeFor[policy.ProxyBlock]()}
	var walk func(typ reflect.Type, prefix string) []string
	walk = func(typ reflect.Type, prefix string) []string {
		var paths []string
		for i := range typ.NumField() {
			field := typ.Field(i)
			tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				t.Fatalf("%s.%s has no JSON name, so the profiles file cannot set it and the report names nothing of it", typ.Name(), field.Name)
			}
			if field.Type.Kind() == reflect.Pointer && slices.Contains(blocks, field.Type.Elem()) {
				paths = append(paths, walk(field.Type.Elem(), prefix+tag+".")...)
				continue
			}
			paths = append(paths, prefix+tag)
		}
		return paths
	}
	return walk(reflect.TypeFor[policy.Profile](), "")
}

// TestProfilesCheckReportsEveryField reads every JSON field of a profile and its blocks from the
// types: a file adding profiles that set every field prints, for each field, a line naming it and
// the value the file gives it; and --against a file that removes one, the report prints each field
// of the removed profile, a field of a block it does not have as that block's "none". So no field
// of a profile, today's or one added later, changes in a pull request without a line a reviewer
// reads.
func TestProfilesCheckReportsEveryField(t *testing.T) {
	paths := profileFieldPaths(t)
	if got, want := slices.Sorted(slices.Values(paths)), slices.Sorted(maps.Keys(everyFieldLines)); !slices.Equal(got, want) {
		t.Fatalf("a profile's fields are %v, and this test's lines name %v: give each field its line", got, want)
	}
	added := profilesFile(t, everyField)
	code, stdout, stderr := runCheck(t, added)
	if code != 0 || stderr != "" {
		t.Fatalf("the every-field file = exit %d with stderr %q, want exit 0", code, stderr)
	}
	lines := strings.Split(stdout, "\n")
	for _, path := range paths {
		for _, want := range everyFieldLines[path] {
			if !slices.Contains(lines, want) {
				t.Errorf("the report of an added profile names no %s: no line %q in\n%s", path, want, stdout)
			}
		}
	}

	// --against the every-field file, a file that holds the other two profiles alone removes
	// GITHUB_READ, and the report holds a line for every field it had.
	without := profilesFile(t, `{"profiles": [
  {"name": "GOOGLE_READ", "source": "GCS_READER", "google": {"scopes": ["devstorage.read_only"]}},
  {"name": "AWS_KEY", "proxy": {"hosts": ["*.amazonaws.com"], "allow": [{"method": "GET", "pathPrefix": "/"}], "kind": "aws", "judge": 300, "guidance": "grant it for the backup job"}}
]}`)
	code, stdout, stderr = runCheck(t, without, "--against", added)
	if code != 0 || stderr != "" {
		t.Fatalf("a file removing GITHUB_READ = exit %d with stderr %q, want exit 0", code, stderr)
	}
	lines = strings.Split(stdout, "\n")
	if !slices.Contains(lines, `profile "GITHUB_READ": removed`) {
		t.Fatalf("the report does not say GITHUB_READ was removed:\n%s", stdout)
	}
	for _, path := range paths {
		if path == "name" {
			continue
		}
		block, _, inBlock := strings.Cut(path, ".")
		reported := slices.ContainsFunc(lines, func(line string) bool {
			return strings.HasPrefix(line, `profile "GITHUB_READ": `+path+" ") ||
				(inBlock && line == `profile "GITHUB_READ": `+block+" none")
		})
		if !reported {
			t.Errorf("the report of a removed profile names no %s of it:\n%s", path, stdout)
		}
	}
	for _, want := range everyFieldLines["github.repositories"] {
		if !slices.Contains(lines, want) {
			t.Errorf("the report of the removed profile does not hold %q:\n%s", want, stdout)
		}
	}
}

// TestProfilesCheckRefusesAFileTheBrokerWouldNotServe covers the ways a file is refused: the
// broker's own reader decides (policy.ParseProfiles, which the boot runs), and the one line says
// which profile and which field, or where in the file, so that the author of the pull request can
// find it. Each refusal writes nothing on stdout: a report of a file the broker would not serve
// would be read as a file it had agreed to.
func TestProfilesCheckRefusesAFileTheBrokerWouldNotServe(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want []string
	}{
		{
			"a judge above the longest lifetime",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"]}}, {"name": "ANTHROPIC", "proxy": {"hosts": ["api.anthropic.com"], "judge": 1800}}]}`,
			[]string{`profiles[1] "ANTHROPIC"`, "1800", "900"},
		},
		{
			"guidance longer than the judge reads",
			`{"profiles": [{"name": "ANTHROPIC", "proxy": {"hosts": ["api.anthropic.com"], "judge": 600, "guidance": "` + strings.Repeat("g", 2001) + `"}}]}`,
			[]string{`profiles[0] "ANTHROPIC"`, "proxy.guidance", "2001", "2000"},
		},
		{
			"a proxy block with no host",
			`{"profiles": [{"name": "ANTHROPIC", "proxy": {"judge": 600}}]}`,
			[]string{`profiles[0] "ANTHROPIC"`, "proxy.hosts", "required"},
		},
		{
			"a repeated name",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"]}}, {"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["github.com"]}}]}`,
			[]string{`profiles[1] "GITHUB_ADMIN_PAT"`, "repeats"},
		},
		{
			"workflows at read",
			`{"profiles": [{"name": "GITHUB_WRITE", "github": {"accounts": ["example-org"], "permissions": {"workflows": "read"}}}]}`,
			[]string{`profiles[0] "GITHUB_WRITE"`, "github.permissions.workflows", "write alone"},
		},
		{
			"a github block that names no metadata",
			`{"profiles": [{"name": "GITHUB_READ", "github": {"accounts": ["example-org"], "permissions": {"contents": "read"}}}]}`,
			[]string{`profiles[0] "GITHUB_READ"`, "github.permissions", "names no metadata", "every installation token"},
		},
		{
			"requesters present and empty",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "requesters": [], "proxy": {"hosts": ["api.github.com"]}}]}`,
			[]string{`profiles[0] "GITHUB_ADMIN_PAT"`, "requesters", "nobody"},
		},
		{
			"a file that is not JSON",
			"{\"profiles\": [\n  {\"name\": \"GITHUB_ADMIN_PAT\", \"proxy\": {\"hosts\": [\"api.github.com\"]}},\n]}",
			[]string{"not JSON", "invalid character ','", "byte offset 86", "line 2", "column 71"},
		},
		{
			"a field the broker does not know",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "allowed": ["GET"]}}]}`,
			[]string{"unknown field", "allowed", "byte offset 83, line 1, column 83"},
		},
		{
			"today's rules file",
			`{"secrets": [{"name": "GITHUB_ADMIN_PAT", "hosts": ["api.github.com"]}]}`,
			[]string{`unknown field "secrets"`},
		},
		// A name that is not a name a session can ask for: no release ever matches it, so the
		// file would be reviewed as covering a secret it does not cover.
		{
			"a profile name in lower case",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"]}}, {"name": "legion_slack_bot_token", "proxy": {"hosts": ["slack.com"]}}]}`,
			[]string{`profiles[1] "legion_slack_bot_token"`, "not an agent secret name", "no caller can ask for it"},
		},
		{
			"a profile name written with hyphens",
			`{"profiles": [{"name": "LEGION-SLACK-BOT-TOKEN", "proxy": {"hosts": ["slack.com"]}}]}`,
			[]string{`profiles[0] "LEGION-SLACK-BOT-TOKEN"`, "not an agent secret name", "no caller can ask for it"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := profilesFile(t, c.body)
			code, stdout, stderr := runCheck(t, path)
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

// ruleHostRows are the host spellings `profiles check`
// (TestProfilesCheckRefusesAHostTheProxyNeverCompares) and the boot
// (TestMainRefusesAProfilesFileItCannotServe) both refuse. rulehost's own test holds a row for
// every way out of the grammar and reads Refusal's text, its one advice among it; these rows read
// the wiring: that the host Refusal reads is the host the file writes, which a reader of the file
// would change if it folded case, took a trailing dot or white space off, wrote an address in its
// canonical form, read the host as a URL or converted it to punycode, and that a refusal's reason
// and the form it names reach its reader.
var ruleHostRows = []ruleHostRow{
	{"a host name in mixed case with a trailing dot", "API.GitHub.com.", []string{"ends in a dot", "write it as api.github.com", "sni mismatch"}, "GitHub"},
	{"an IPv6 address in upper case", "2001:DB8:8::1",
		[]string{"is an IP address", "2001:db8:8::1 in its canonical form", "no TLS server name", "sni mismatch", "name the service by its host name"}, "DB8"},
	{"a scheme with one slash", "https:/slack.com", []string{"is no DNS name"}, "https:"},
	{"a host with a scheme", "https://slack.com", []string{"holds a scheme", "write the service's host name with no scheme"}, "https"},
	{"a Unicode internationalized name", "bücher.example", []string{"punycode", "write it as xn--bcher-kva.example"}, "bücher"},
	{"a trailing dot and a newline", "api.github.com.\n", []string{"white space", "binds nothing"}, "github"},
}

// foldedRuleHosts are hosts the grammar serves in a spelling a reader of the file would change
// if it folded case before Refusal read them; rulehost's own test holds every shape the grammar
// serves and its edges.
var foldedRuleHosts = []string{"API.GitHub.com", "*.GoogleAPIs.com", "XN--BCHER-KVA.example"}

// profilesWithHosts is a profiles file of one profile, name, whose proxy block binds hosts,
// written by encoding/json so that a host holding a quotation mark, a control character or
// Unicode is the host the row means.
func profilesWithHosts(t *testing.T, name string, hosts ...string) string {
	t.Helper()
	type proxy struct {
		Hosts []string `json:"hosts"`
	}
	type profile struct {
		Name  string `json:"name"`
		Proxy proxy  `json:"proxy"`
	}
	body, err := json.Marshal(struct {
		Profiles []profile `json:"profiles"`
	}{[]profile{{name, proxy{hosts}}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestProfilesCheckRefusesAHostTheProxyNeverCompares: a proxy block's host is served only in the
// one grammar a rules host is written in (rulehost.Refusal, whose own test holds every spelling
// out of it), and `profiles check` refuses a spelling outside it as the boot does, naming the
// profile and the host's position, saying why, and naming the form to write where one exists,
// with nothing else of the host as written. The control is a file of hosts the grammar serves in
// a spelling a reader that folded case would change: it is valid.
func TestProfilesCheckRefusesAHostTheProxyNeverCompares(t *testing.T) {
	for _, c := range ruleHostRows {
		t.Run(c.name, func(t *testing.T) {
			path := profilesFile(t, profilesWithHosts(t, "GITHUB_ADMIN_PAT", "api.github.com", c.host))
			code, stdout, stderr := runCheck(t, path)
			if code != 1 || stdout != "" {
				t.Fatalf("%s = exit %d with stdout %q, want exit 1 and no report", c.name, code, stdout)
			}
			// The refusal names the file, whose path is the row's temporary directory: it holds
			// the subtest's name and whatever the temporary directory's own path holds, so every
			// want, and the text that must be absent, is looked for with the path taken out.
			text := strings.ReplaceAll(stderr, path, "")
			for _, want := range append([]string{`profiles[0] "GITHUB_ADMIN_PAT"`, "proxy.hosts[1]"}, c.want...) {
				if !strings.Contains(text, want) {
					t.Fatalf("the refusal does not say %q: %q", want, stderr)
				}
			}
			if c.absent != "" && strings.Contains(text, c.absent) {
				t.Fatalf("the refusal holds %q, which it must not: %q", c.absent, stderr)
			}
		})
	}
	t.Run("the control: hosts the grammar serves, in a case a reader could fold", func(t *testing.T) {
		path := profilesFile(t, profilesWithHosts(t, "GITHUB_ADMIN_PAT", foldedRuleHosts...))
		if code, _, stderr := runCheck(t, path); code != 0 || stderr != "" {
			t.Fatalf("a file of hosts in the grammar's spelling = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
		}
	})
}

// TestProfilesCheckReportsWhatChangedAgainstTheOldFile is the report a reviewer reads: for a
// profile both files hold, each value of a list added or removed and each other field as old →
// new, a field the file leaves to the source written as the source's; and a profile removed, with
// every field it had.
func TestProfilesCheckReportsWhatChangedAgainstTheOldFile(t *testing.T) {
	old := profilesFile(t, `{"profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}},
   "proxy": {"hosts": ["api.github.com"], "allow": [{"method": "GET", "pathPrefix": "/v1/"}], "judge": 300}},
  {"name": "LEGACY", "proxy": {"hosts": ["old.example.com"]}}
]}`)
	now := profilesFile(t, `{"profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon"], "maxGrant": "1h",
   "github": {"accounts": ["example-org", "ada"], "repositories": ["ada/proofs", "example-org/legion"], "permissions": {"contents": "read", "metadata": "read", "pull_requests": "read"}, "value": "token"},
   "proxy": {"hosts": ["api.github.com", "uploads.github.com"], "headers": ["authorization", "x-api-key"],
    "allow": [{"method": "POST", "pathPrefix": "/v1/"}], "kind": "aws", "judge": 600, "guidance": "grant for the release job"}}
]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("a valid file against an old one = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	want := fmt.Sprintf(`profiles check: %s is valid: 1 profile
profiles check: compared against %s
profile "GITHUB_READ": tier the source's → agent
profile "GITHUB_READ": requesters the source's → legion-daemon
profile "GITHUB_READ": maxGrant the source's → 1h0m0s
profile "GITHUB_READ": github.accounts ada added
profile "GITHUB_READ": github.repositories every repository the installation reaches → ada/proofs example-org/legion
profile "GITHUB_READ": github.permissions pull_requests read added
profile "GITHUB_READ": github.value installations → token
profile "GITHUB_READ": proxy.hosts uploads.github.com added
profile "GITHUB_READ": proxy.headers x-api-key added
profile "GITHUB_READ": proxy.allow GET /v1/ → POST /v1/
profile "GITHUB_READ": proxy.kind "" → aws
profile "GITHUB_READ": proxy.judge 300 seconds → 600 seconds
profile "GITHUB_READ": proxy.guidance "" → "grant for the release job"
profile "LEGACY": removed
profile "LEGACY": source LEGACY
profile "LEGACY": tier the source's
profile "LEGACY": requesters the source's
profile "LEGACY": maxGrant the source's
profile "LEGACY": github none
profile "LEGACY": google none
profile "LEGACY": proxy.hosts old.example.com
profile "LEGACY": proxy.headers authorization
profile "LEGACY": proxy.allow any method and path
profile "LEGACY": proxy.kind ""
profile "LEGACY": proxy.judge none
profile "LEGACY": proxy.guidance ""
`, now, old)
	if stdout != want {
		t.Fatalf("the report is\n%s\nwant\n%s", stdout, want)
	}
}

// TestProfilesCheckReportsABlockAddedAndABlockRemoved: a profile that gains a block reads as that
// block added, with each of its fields, and one that loses a block as that block removed, with
// each field it had, rather than as a run of fields changed from nothing.
func TestProfilesCheckReportsABlockAddedAndABlockRemoved(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}}}]}`)
	now := profilesFile(t, `{"profiles": [{"name": "LEGION_IMPLEMENT_APP", "proxy": {"hosts": ["api.github.com"]}}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	for _, want := range []string{
		`profile "LEGION_IMPLEMENT_APP": github removed`,
		`profile "LEGION_IMPLEMENT_APP": github.accounts example-org`,
		`profile "LEGION_IMPLEMENT_APP": github.repositories every repository the installation reaches`,
		`profile "LEGION_IMPLEMENT_APP": github.permissions contents read`,
		`profile "LEGION_IMPLEMENT_APP": github.permissions metadata read`,
		`profile "LEGION_IMPLEMENT_APP": github.value installations`,
		`profile "LEGION_IMPLEMENT_APP": proxy added`,
		`profile "LEGION_IMPLEMENT_APP": proxy.hosts api.github.com`,
		`profile "LEGION_IMPLEMENT_APP": proxy.headers authorization`,
	} {
		if !slices.Contains(strings.Split(stdout, "\n"), want) {
			t.Fatalf("the report does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "google") {
		t.Fatalf("the report names a google block neither file has:\n%s", stdout)
	}
}

// TestProfilesCheckReportsAHostRemovedAndAHeaderRemoved is the other direction of the same
// report, and the default headers with it: a block that names no header carries the credential in
// authorization, so a file that writes "headers": [] has taken that header away, and the report
// says so rather than treating the two as the same empty field.
func TestProfilesCheckReportsAHostRemovedAndAHeaderRemoved(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com", "github.com"]}}]}`)
	now := profilesFile(t, `{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "headers": []}}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	for _, want := range []string{
		`profile "GITHUB_ADMIN_PAT": proxy.hosts github.com removed`,
		`profile "GITHUB_ADMIN_PAT": proxy.headers authorization removed`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the report does not say %q:\n%s", want, stdout)
		}
	}
}

// TestProfilesCheckReportsTheDefaultHeaderTheSwapApplies: the header a block with no "headers"
// field reads as, in this report, is the one proxyrules.Rewrite actually swaps in, read from the
// airlock package itself, so a change to that default cannot leave the report saying a header was
// added or removed that the swap says nothing about.
func TestProfilesCheckReportsTheDefaultHeaderTheSwapApplies(t *testing.T) {
	names := proxyrules.SecretRule{}.HeaderNames()
	if len(names) != 1 {
		t.Fatalf("an entry with no headers field names %v; this report reads it as one header", names)
	}
	added := profilesFile(t, `{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"]}}]}`)
	code, stdout, stderr := runCheck(t, added)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if want := fmt.Sprintf("profile %q: proxy.headers %s\n", "GITHUB_ADMIN_PAT", names[0]); !strings.Contains(stdout, want) {
		t.Fatalf("the report does not say %q:\n%s", want, stdout)
	}
}

// TestProfilesCheckSaysWhenNothingChanged pins the one line a pull request that changed something
// else gets: a report of no lines at all would read as a report that had not run. A file that
// spells a profile another way, an account in upper case, changes nothing the broker serves.
func TestProfilesCheckSaysWhenNothingChanged(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "GITHUB_READ", "github": {"accounts": ["Example-Org"], "permissions": {"contents": "read", "metadata": "read"}}}]}`)
	now := profilesFile(t, `{"profiles": [{"name": "GITHUB_READ", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "installations"}}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if !strings.HasSuffix(stdout, "profiles check: nothing changed\n") {
		t.Fatalf("an unchanged file's report does not end in the nothing-changed line:\n%s", stdout)
	}
	if strings.Contains(stdout, "profile \"") {
		t.Fatalf("an unchanged file's report names a profile:\n%s", stdout)
	}
}

// TestProfilesCheckRefusesAnOldFileItCannotRead refuses with 1 and names the --against file: the
// report is the point of the run, so an old file that is not there is a failure and not a run
// against nothing.
func TestProfilesCheckRefusesAnOldFileItCannotRead(t *testing.T) {
	now := profilesFile(t, twoProfiles)
	missing := filepath.Join(t.TempDir(), "gone.json")
	code, stdout, stderr := runCheck(t, now, "--against", missing)
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

// TestProfilesCheckComparesAgainstAnOldFileTheBrokerWouldRefuse is why the old file is decoded
// and not held to the file's rules (policy.DecodeProfiles): the profiles in force may predate a
// refusal this broker makes, and a check that refused them would refuse the pull request that
// fixes them too.
func TestProfilesCheckComparesAgainstAnOldFileTheBrokerWouldRefuse(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 1800}}]}`)
	now := profilesFile(t, `{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 900}}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if want := `profile "GITHUB_ADMIN_PAT": proxy.judge 1800 seconds → 900 seconds`; !strings.Contains(stdout, want) {
		t.Fatalf("the report does not say %q:\n%s", want, stdout)
	}
}

// TestProfilesCheckRefusesAUsageError keeps the usage errors apart from the file errors: exit 2
// and the usage line, as the end-sessions subcommand answers its own.
func TestProfilesCheckRefusesAUsageError(t *testing.T) {
	path := profilesFile(t, twoProfiles)
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
			code, stdout, stderr := runCheck(t, c.args...)
			if code != 2 {
				t.Fatalf("%s = exit %d, want 2", c.name, code)
			}
			if stdout != "" {
				t.Fatalf("a usage error printed a report: %q", stdout)
			}
			if !strings.Contains(stderr, profilesCheckUsage) {
				t.Fatalf("a usage error printed no usage line: %q", stderr)
			}
		})
	}
}

// TestTheBinaryRoutesEachSubcommandWord drives the built binary, the one thing the function tests
// above cannot see: `profiles check` reaches this command, on a machine with no broker
// configuration and no database; `profiles` with no second word, or another, is a usage error;
// the removed `airlock-check` exits 2 naming the command that replaces it, and reads no file; and
// `end-sessions` still reaches its own rather than being swallowed by the new one.
func TestTheBinaryRoutesEachSubcommandWord(t *testing.T) {
	binPath := buildBroker(t)
	path := profilesFile(t, twoProfiles)

	out, err := exec.Command(binPath, profilesCommand, profilesCheckCommand, path).CombinedOutput()
	if err != nil {
		t.Fatalf("broker profiles check %s = %v\n%s", path, err, out)
	}
	t.Logf("broker profiles check %s printed:\n%s", path, out)
	for _, want := range []string{"is valid: 2 profiles", `profile "OPENAI": proxy.hosts api.openai.com`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("the binary's report does not say %q: %s", want, out)
		}
	}

	exitsTwo := func(t *testing.T, want []string, args ...string) string {
		t.Helper()
		out, err := exec.Command(binPath, args...).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("broker %s = %v, want exit 2\n%s", strings.Join(args, " "), err, out)
		}
		for _, w := range want {
			if !strings.Contains(string(out), w) {
				t.Fatalf("broker %s does not say %q: %s", strings.Join(args, " "), w, out)
			}
		}
		return string(out)
	}
	exitsTwo(t, []string{profilesCheckUsage}, profilesCommand)
	exitsTwo(t, []string{profilesCheckUsage}, profilesCommand, "validate", path)
	if out := exitsTwo(t, []string{"airlock-check is removed", "broker profiles check <file> [--against <old file>]"}, "airlock-check", path); strings.Contains(out, "is valid") {
		t.Fatalf("the removed airlock-check read the file: %s", out)
	}
	exitsTwo(t, []string{"usage: broker " + endSessionsCommand + " <login>"}, endSessionsCommand)
}
