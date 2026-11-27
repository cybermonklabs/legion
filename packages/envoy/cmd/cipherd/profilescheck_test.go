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

// twoProfiles is a profiles file the broker would serve: two kinds, one of them with headers of
// its own, and two names behind them, GITHUB_ADMIN_PAT by its source's proxy tag, which the store
// holds, and OPENAI, with a tier of its own, by its proxy string.
const twoProfiles = `{"kinds": [
  {"name": "github", "hosts": ["api.github.com", "github.com"], "ways": ["swap"]},
  {"name": "openai", "hosts": ["api.openai.com"], "headers": ["authorization", "x-api-key"], "ways": ["swap"]}
], "profiles": [
  {"name": "GITHUB_ADMIN_PAT"},
  {"name": "OPENAI", "tier": "agent", "proxy": "openai"}
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
// file's own kinds and profiles, read against an empty profiles file, so every kind and profile is
// an addition and every field of it is written out, the ones the file leaves out among them. The
// whole of stdout is asserted, because the report's order is its contract with the person reading
// it in a pull request: two runs on the same files say the same thing, and the lines of one kind
// or profile are together. With no --secrets-json, GITHUB_ADMIN_PAT's kind is its source's tag,
// which was not read: the report names it UNKNOWN, and the run exits 3 saying so on stderr.
func TestProfilesCheckReportsAWholeFileAsAdded(t *testing.T) {
	path := profilesFile(t, twoProfiles)
	code, stdout, stderr := runCheck(t, path)
	wantErr := "profiles check: " + path + ": UNKNOWN: not read (no --secrets-json): the kind of 1 profile, its source's proxy tag; nothing it read is refused\n"
	if code != exitUnknown || stderr != wantErr {
		t.Fatalf("a valid file whose one profile's kind is unread = exit %d with stderr %q, want exit %d and %q", code, stderr, exitUnknown, wantErr)
	}
	want := fmt.Sprintf(`profiles check: %s is valid: 2 profiles and 2 kinds
profiles check: UNKNOWN: profile "GITHUB_ADMIN_PAT": its kind is its source GITHUB_ADMIN_PAT's proxy tag, which was not read (no --secrets-json), so it was held to no kind
profiles check: compared against an empty profiles file, so every kind and profile reads as added
kind "github": added
kind "github": hosts api.github.com
kind "github": hosts github.com
kind "github": headers authorization
kind "github": variables none
kind "github": ways swap
kind "github": allow any method and path
kind "github": placeholder_prefix the default, agent-secrets-placeholder-
kind "github": max_grant none
kind "openai": added
kind "openai": hosts api.openai.com
kind "openai": headers authorization
kind "openai": headers x-api-key
kind "openai": variables none
kind "openai": ways swap
kind "openai": allow any method and path
kind "openai": placeholder_prefix the default, agent-secrets-placeholder-
kind "openai": max_grant none
profile "GITHUB_ADMIN_PAT": added
profile "GITHUB_ADMIN_PAT": source GITHUB_ADMIN_PAT
profile "GITHUB_ADMIN_PAT": tier the source's
profile "GITHUB_ADMIN_PAT": requesters the source's
profile "GITHUB_ADMIN_PAT": maxGrant the source's
profile "GITHUB_ADMIN_PAT": proxy none
profile "GITHUB_ADMIN_PAT": allow none
profile "GITHUB_ADMIN_PAT": github none
profile "GITHUB_ADMIN_PAT": google none
profile "GITHUB_ADMIN_PAT": proxy block none
profile "OPENAI": added
profile "OPENAI": source OPENAI
profile "OPENAI": tier agent
profile "OPENAI": requesters the source's
profile "OPENAI": maxGrant the source's
profile "OPENAI": proxy openai
profile "OPENAI": allow none
profile "OPENAI": github none
profile "OPENAI": google none
profile "OPENAI": proxy block none
`, path)
	if stdout != want {
		t.Fatalf("the report is\n%s\nwant\n%s", stdout, want)
	}
}

// everyField is a profiles file that sets every field a kind and a profile have: one kind that
// sets each of its fields, and three profiles, since a profile holds a github block or a google
// block and never both, and a dpop block alone: GITHUB_READ sets every field but google and the
// dpop block, GOOGLE_READ sets google, and RESULTS_TOKEN sets the dpop block.
const everyField = `{"kinds": [
  {"name": "github", "hosts": ["api.github.com"], "headers": ["authorization", "x-github-token"], "variables": ["GH_TOKEN"], "ways": ["swap", "mint"],
   "allow": [{"method": "GET", "pathPrefix": "/repos/"}], "placeholder_prefix": "tml-", "max_grant": "12h"}
], "profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon", "ada@example.com"], "maxGrant": "1h30m",
   "github": {"accounts": ["example-org"], "repositories": ["example-org/legion"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
   "proxy": "github", "allow": [{"method": "GET", "pathPrefix": "/repos/example-org/", "host": "api.github.com"}]},
  {"name": "GOOGLE_READ", "source": "GCS_READER", "google": {"scopes": ["devstorage.read_only"]}},
  {"name": "RESULTS_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"}}
]}`

// everyFieldLines is, for each field of a profile by its JSON path (a block's fields under the
// block's name), the lines the report of everyField as an added file must hold for it: the field
// and the value the file gives it.
var everyFieldLines = map[string][]string{
	"name":                {`profile "GITHUB_READ": added`, `profile "GOOGLE_READ": added`, `profile "RESULTS_TOKEN": added`},
	"source":              {`profile "GITHUB_READ": source LEGION_IMPLEMENT_APP`, `profile "GOOGLE_READ": source GCS_READER`},
	"tier":                {`profile "GITHUB_READ": tier agent`},
	"requesters":          {`profile "GITHUB_READ": requesters ada@example.com`, `profile "GITHUB_READ": requesters legion-daemon`},
	"maxGrant":            {`profile "GITHUB_READ": maxGrant 1h30m0s`},
	"proxy":               {`profile "GITHUB_READ": proxy github`},
	"allow":               {`profile "GITHUB_READ": allow GET /repos/example-org/ on api.github.com`},
	"github.accounts":     {`profile "GITHUB_READ": github.accounts example-org`},
	"github.repositories": {`profile "GITHUB_READ": github.repositories example-org/legion`},
	"github.permissions":  {`profile "GITHUB_READ": github.permissions contents read`, `profile "GITHUB_READ": github.permissions metadata read`},
	"github.value":        {`profile "GITHUB_READ": github.value token`},
	"google.scopes":       {`profile "GOOGLE_READ": google.scopes devstorage.read_only`},
	"proxy.hosts":         {`profile "RESULTS_TOKEN": proxy.hosts dispatch.test`},
	"proxy.headers":       {`profile "RESULTS_TOKEN": proxy.headers Authorization`},
	"proxy.allow":         {`profile "RESULTS_TOKEN": proxy.allow POST /api/v1/`},
	"proxy.kind":          {`profile "RESULTS_TOKEN": proxy.kind dpop`},
}

// everyKindLines is, for each field of a kind by its JSON name, the lines the report of
// everyField as an added file must hold for it.
var everyKindLines = map[string][]string{
	"name":               {`kind "github": added`},
	"hosts":              {`kind "github": hosts api.github.com`},
	"headers":            {`kind "github": headers authorization`, `kind "github": headers x-github-token`},
	"variables":          {`kind "github": variables GH_TOKEN`},
	"ways":               {`kind "github": ways swap`, `kind "github": ways mint`},
	"allow":              {`kind "github": allow GET /repos/`},
	"placeholder_prefix": {`kind "github": placeholder_prefix tml-`},
	"max_grant":          {`kind "github": max_grant 12h`},
}

// reportBlockNames is the name the report gives each block of a profile, by the block's JSON
// name: the proxy key's block is "proxy block", since "proxy" alone is the key's string form.
var reportBlockNames = map[string]string{"github": "github", "google": "google", "proxy": "proxy block"}

// profileFieldPaths is every JSON field of policy.Profile, a block's fields (GitHubBlock,
// GoogleBlock, ProxyBlock) under the block's own field name, read from the types themselves, so a
// field added to a profile is a field this report's test asks about. The two exported fields a
// Profile does not marshal (json "-") are named as the file writes them: ProxyKind is the proxy
// key's string form and Allow the profile's allow; another such field fails the test until it is
// named. An unexported field is no field the file can set (encoding/json reads none), so it is
// none of the report's.
func profileFieldPaths(t *testing.T) []string {
	t.Helper()
	unmarshalled := map[string]string{"ProxyKind": "proxy", "Allow": "allow"}
	blocks := []reflect.Type{reflect.TypeFor[policy.GitHubBlock](), reflect.TypeFor[policy.GoogleBlock](), reflect.TypeFor[policy.ProxyBlock]()}
	var walk func(typ reflect.Type, prefix string) []string
	walk = func(typ reflect.Type, prefix string) []string {
		var paths []string
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if tag == "-" && prefix == "" && unmarshalled[field.Name] != "" {
				tag = unmarshalled[field.Name]
			}
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

// jsonNames is the JSON name of each field of typ.
func jsonNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var names []string
	for i := range typ.NumField() {
		tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no JSON name, so the profiles file cannot set it and the report names nothing of it", typ.Name(), typ.Field(i).Name)
		}
		names = append(names, tag)
	}
	return names
}

// TestProfilesCheckReportsEveryField reads every JSON field of a kind, of a profile and its
// blocks, and of an allow row from the types: a file adding a kind and profiles that set every
// field prints, for each field, a line naming it and the value the file gives it; and --against a
// file that removes the kind and one profile, the report prints each field of the removed kind
// and profile, a field of a block it does not have as that block's "none". So no field of a kind
// or a profile, today's or one added later, changes in a pull request without a line a reviewer
// reads. An allow row is written as its method, its path prefix and its host, so a field added to
// a row fails here until the report writes it.
func TestProfilesCheckReportsEveryField(t *testing.T) {
	if got, want := jsonNames(t, reflect.TypeFor[proxyrules.Allow]()), []string{"method", "pathPrefix", "host"}; !slices.Equal(got, want) {
		t.Fatalf("an allow row's fields are %v, and the report writes %v of each row (rowText): write each field", got, want)
	}
	paths := profileFieldPaths(t)
	if got, want := slices.Sorted(slices.Values(paths)), slices.Sorted(maps.Keys(everyFieldLines)); !slices.Equal(got, want) {
		t.Fatalf("a profile's fields are %v, and this test's lines name %v: give each field its line", got, want)
	}
	kindPaths := jsonNames(t, reflect.TypeFor[proxyrules.Kind]())
	if got, want := slices.Sorted(slices.Values(kindPaths)), slices.Sorted(maps.Keys(everyKindLines)); !slices.Equal(got, want) {
		t.Fatalf("a kind's fields are %v, and this test's lines name %v: give each field its line", got, want)
	}
	added := profilesFile(t, everyField)
	code, stdout, stderr := runCheck(t, added)
	// GOOGLE_READ names no kind as a string, so with no --secrets-json its kind is unread: the
	// report is whole all the same, and the run says so and exits 3.
	if code != exitUnknown || !strings.Contains(stdout, `profiles check: UNKNOWN: profile "GOOGLE_READ": its kind is its source GCS_READER's proxy tag`) {
		t.Fatalf("the every-field file = exit %d with stderr %q, want exit %d and GOOGLE_READ's UNKNOWN line", code, stderr, exitUnknown)
	}
	lines := strings.Split(stdout, "\n")
	for _, set := range []struct {
		paths []string
		lines map[string][]string
	}{{paths, everyFieldLines}, {kindPaths, everyKindLines}} {
		for _, path := range set.paths {
			for _, want := range set.lines[path] {
				if !slices.Contains(lines, want) {
					t.Errorf("the report of an added file names no %s: no line %q in\n%s", path, want, stdout)
				}
			}
		}
	}

	// --against the every-field file, a file that holds the other two profiles alone removes
	// GITHUB_READ and the kind, and the report holds a line for every field each had.
	without := profilesFile(t, `{"profiles": [
  {"name": "GOOGLE_READ", "source": "GCS_READER", "google": {"scopes": ["devstorage.read_only"]}},
  {"name": "RESULTS_TOKEN", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"}}
]}`)
	code, stdout, stderr = runCheck(t, without, "--against", added)
	if code != 0 || stderr != "" {
		t.Fatalf("a file removing GITHUB_READ = exit %d with stderr %q, want exit 0", code, stderr)
	}
	lines = strings.Split(stdout, "\n")
	for _, want := range []string{`profile "GITHUB_READ": removed`, `kind "github": removed`} {
		if !slices.Contains(lines, want) {
			t.Fatalf("the report does not say %q:\n%s", want, stdout)
		}
	}
	for _, path := range paths {
		if path == "name" {
			continue
		}
		block, _, inBlock := strings.Cut(path, ".")
		reported := slices.ContainsFunc(lines, func(line string) bool {
			return strings.HasPrefix(line, `profile "GITHUB_READ": `+path+" ") ||
				(inBlock && line == `profile "GITHUB_READ": `+reportBlockNames[block]+" none")
		})
		if !reported {
			t.Errorf("the report of a removed profile names no %s of it:\n%s", path, stdout)
		}
	}
	for _, path := range kindPaths {
		if path != "name" && !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, `kind "github": `+path+" ") }) {
			t.Errorf("the report of a removed kind names no %s of it:\n%s", path, stdout)
		}
	}
	for _, want := range append(everyFieldLines["github.repositories"], everyKindLines["placeholder_prefix"]...) {
		if !slices.Contains(lines, want) {
			t.Errorf("the report of the removed profile and kind does not hold %q:\n%s", want, stdout)
		}
	}
}

// TestProfilesCheckRefusesAFileTheBrokerWouldNotServe covers the ways a file is refused: the
// broker's own reader decides (policy.ParseProfiles, which the boot runs), and the one line says
// which kind or profile and which field, or where in the file, so that the author of the pull
// request can find it; and a profile whose allow its proxy string's kind does not cover, which the
// policy load would refuse. Each refusal writes nothing on stdout: a report of a file the broker
// would not serve would be read as a file it had agreed to.
func TestProfilesCheckRefusesAFileTheBrokerWouldNotServe(t *testing.T) {
	const dpop = `"hosts": ["dispatch.test"], "headers": ["Authorization"], "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"`
	for _, c := range []struct {
		name string
		body string
		want []string
	}{
		{
			"a proxy block of no kind in a file of kinds, which the kinds replaced",
			`{"kinds": [{"name": "github", "hosts": ["api.github.com"], "ways": ["swap"]}], "profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"]}}]}`,
			[]string{`profiles[0] "GITHUB_ADMIN_PAT"`, "proxy block is no longer read", "declare the kind under kinds", "GITHUB_ADMIN_PAT, proxy=<kind>"},
		},
		{
			"a proxy block of kind azure in a file of kinds, which the kinds replaced",
			`{"kinds": [{"name": "azure", "hosts": ["login.microsoftonline.com"], "ways": ["exchange"]}], "profiles": [{"name": "AZURE_ADMIN", "proxy": {"hosts": ["login.microsoftonline.com"], "kind": "azure"}}]}`,
			[]string{`profiles[0] "AZURE_ADMIN"`, "proxy block is no longer read", "kinds"},
		},
		{
			"a proxy block of a kind no block took, in a file of no kinds",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "kind": "gcp"}}]}`,
			[]string{`profiles[0] "GITHUB_ADMIN_PAT"`, `proxy.kind is "gcp"`, `a proxy block's kind is "", aws, azure or dpop`},
		},
		{
			"a judge in a proxy block of a file of no kinds",
			`{"profiles": [{"name": "GITHUB_ADMIN_READ", "source": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 900}}]}`,
			[]string{`profiles[0] "GITHUB_ADMIN_READ"`, "proxy.judge", "its own judge tag", "the source, GITHUB_ADMIN_PAT"},
		},
		{
			"a judge in a dpop block",
			`{"profiles": [{"name": "RESULTS_TOKEN", "proxy": {` + dpop + `, "judge": 600}}]}`,
			[]string{`profiles[0] "RESULTS_TOKEN"`, "proxy.judge", "has no judge"},
		},
		{
			"guidance in a dpop block",
			`{"profiles": [{"name": "RESULTS_TOKEN", "proxy": {` + dpop + `, "guidance": "grant for the release job"}}]}`,
			[]string{`profiles[0] "RESULTS_TOKEN"`, "proxy.guidance", "has no judge"},
		},
		{
			"a dpop block with no host",
			`{"profiles": [{"name": "RESULTS_TOKEN", "proxy": {"headers": ["Authorization"], "kind": "dpop"}}]}`,
			[]string{`profiles[0] "RESULTS_TOKEN"`, "proxy.hosts", "required"},
		},
		{
			"a kind with no host",
			`{"kinds": [{"name": "slack", "hosts": [], "ways": ["swap"]}], "profiles": []}`,
			[]string{`kinds[0] "slack"`, "no host"},
		},
		{
			"a proxy string naming no kind of the file",
			`{"kinds": [], "profiles": [{"name": "OPENAI", "proxy": "openai"}]}`,
			[]string{`profiles[0] "OPENAI"`, `names the kind "openai"`, "do not declare"},
		},
		{
			"a profile's row outside its proxy string's kind",
			`{"kinds": [{"name": "anthropic", "hosts": ["api.anthropic.com"], "ways": ["mint"], "allow": [{"method": "GET", "pathPrefix": "/v1/organizations/"}]}],
			  "profiles": [{"name": "ANTHROPIC_ADMIN", "proxy": "anthropic", "allow": [{"method": "POST", "pathPrefix": "/v1/organizations/"}]}]}`,
			[]string{`profile "ANTHROPIC_ADMIN": profile-allow-outside-kind: allow rows POST /v1/organizations/ are outside the kind anthropic's allow`},
		},
		{
			"a repeated name",
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT"}, {"name": "GITHUB_ADMIN_PAT", "tier": "agent"}]}`,
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
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT", "requesters": []}]}`,
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
			`{"profiles": [{"name": "GITHUB_ADMIN_PAT"}, {"name": "legion_slack_bot_token"}]}`,
			[]string{`profiles[1] "legion_slack_bot_token"`, "not an agent secret name", "no caller can ask for it"},
		},
		{
			"a profile name written with hyphens",
			`{"profiles": [{"name": "LEGION-SLACK-BOT-TOKEN"}]}`,
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

// hostsKind is the one kind profilesWithHosts declares.
const hostsKind = "service"

// profilesWithHosts is a profiles file of one kind, hostsKind, binding hosts, and one profile,
// name, behind it by its proxy string, written by encoding/json so that a host holding a quotation
// mark, a control character or Unicode is the host the row means. A host the grammar refuses is
// refused as the kind's: kinds[0] "service": hosts[<index>].
func profilesWithHosts(t *testing.T, name string, hosts ...string) string {
	t.Helper()
	type kind struct {
		Name  string   `json:"name"`
		Hosts []string `json:"hosts"`
		Ways  []string `json:"ways"`
	}
	type profile struct {
		Name  string `json:"name"`
		Proxy string `json:"proxy"`
	}
	body, err := json.Marshal(struct {
		Kinds    []kind    `json:"kinds"`
		Profiles []profile `json:"profiles"`
	}{[]kind{{hostsKind, hosts, []string{proxyrules.WaySwap}}}, []profile{{name, hostsKind}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestProfilesCheckRefusesAHostTheProxyNeverCompares: a kind's host is served only in the one
// grammar a rules host is written in (rulehost.Refusal, whose own test holds every spelling out of
// it), and `profiles check` refuses a spelling outside it as the boot does, naming the kind and the
// host's position, saying why, and naming the form to write where one exists, with nothing else
// of the host as written. The control is a file of hosts the grammar serves in a spelling a reader
// that folded case would change: it is valid.
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
			for _, want := range append([]string{`kinds[0] "` + hostsKind + `"`, "hosts[1]"}, c.want...) {
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

// resultsProfileOf is a profile as a profiles file writes it, of name and a proxy block whose
// fields are given as JSON text: hosts the inside of its list, and headers, allow and kind each
// left out when given "".
func resultsProfileOf(name, hosts, headers, allow, kind string) string {
	fields := []string{`"hosts": [` + hosts + `]`}
	if headers != "" {
		fields = append(fields, `"headers": `+headers)
	}
	if allow != "" {
		fields = append(fields, `"allow": `+allow)
	}
	if kind != "" {
		fields = append(fields, `"kind": "`+kind+`"`)
	}
	return `{"name": "` + name + `", "proxy": {` + strings.Join(fields, ", ") + `}}`
}

// TestProfilesCheckRefusesWhatTheBootRefusesOfTheResultsProfile: `profiles check` holds the
// results profile, DISPATCH_RESULTS_TOKEN, to the boot's own check of it. Every shape the file
// alone decides (config.ResultsEntryRules, then config.MintedEntryRules, the rules of every dpop
// block) is refused, with --private-upstream and without it, exit 1 with one line on stderr naming
// the file, the profile and the fault, and no report: a "*." host, two hosts, no kind, kind aws, no
// allow list, an empty one, a header other than Authorization, another profile, not of kind dpop,
// whose "*." host binds the results host, and a lower-case dispatch_results_token of kind dpop.
// Given the destinations the broker is given (--private-upstream, as BROKER_PROXY_PRIVATE_UPSTREAM),
// it makes the boot's whole check (config.ProxyMintedRules): the profile of another host than the
// destination's is refused, and so are a destination beside a file with no results profile and a
// second dpop profile of a host no destination names, which is valid without the flag. A
// --private-upstream given empty is not given, as the setting given blank names no destination.
// Without the flag, a file holding the right profile is valid and the report says in one line that
// its tie to the destination was not checked; with it, that line is absent; and a file with no
// results profile prints no such line. A destination the setting's parser refuses is a usage
// error that quotes no credential of its URL.
func TestProfilesCheckRefusesWhatTheBootRefusesOfTheResultsProfile(t *testing.T) {
	const (
		name     = "DISPATCH_RESULTS_TOKEN"
		headers  = `["Authorization"]`
		allow    = `[{"method": "POST", "pathPrefix": "/api/v1/projects/"}]`
		upstream = resultsBoundHost + "=http://10.0.0.5:8766"
		notSeen  = "profiles check: DISPATCH_RESULTS_TOKEN binds " + resultsBoundHost + "; its tie to BROKER_PROXY_PRIVATE_UPSTREAM was not checked (no --private-upstream)"
		unnamed  = "which no entry of BROKER_PROXY_PRIVATE_UPSTREAM names"
	)
	bound := `"` + resultsBoundHost + `"`
	file := func(profiles ...string) string { return `{"profiles": [` + strings.Join(profiles, ", ") + `]}` }
	right := resultsProfileOf(name, bound, headers, allow, "dpop")

	// refused runs the subcommand on body with args after the file, and holds it to a refusal of
	// the file: exit 1, no report, one line on stderr that names the file, every want, and one of
	// faults at least, and never a setting this run was not given.
	refused := func(t *testing.T, body string, args []string, wants, faults []string) {
		t.Helper()
		path := profilesFile(t, body)
		code, stdout, stderr := runCheck(t, append([]string{path}, args...)...)
		if code != 1 {
			t.Fatalf("exit %d, want 1: the boot refuses this file", code)
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
		text := strings.ReplaceAll(stderr, path, "")
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatalf("the refusal does not say %q: %q", want, stderr)
			}
		}
		if !slices.ContainsFunc(faults, func(fault string) bool { return strings.Contains(text, fault) }) {
			t.Fatalf("the refusal names none of the faults %q: %q", faults, stderr)
		}
		for _, unset := range []string{"BROKER_EXCHANGE_RESULT_WORKFLOWS", "BROKER_AIRLOCK_RULES_FILE"} {
			if strings.Contains(text, unset) {
				t.Fatalf("the refusal names %s, which this run was not given: %q", unset, stderr)
			}
		}
	}

	for _, c := range []struct {
		name   string
		body   string
		wants  []string
		faults []string
	}{
		{"a *. host", file(resultsProfileOf(name, `"*.ci-results.test"`, headers, allow, "dpop")), []string{name, `"*.ci-results.test"`}, []string{"must bind one exact host"}},
		{"two hosts", file(resultsProfileOf(name, bound+`, "`+otherResultsHost+`"`, headers, allow, "dpop")), []string{name, otherResultsHost}, []string{"must bind one exact host"}},
		{"no kind", file(resultsProfileOf(name, bound, headers, allow, "")), []string{name}, []string{"no longer read", "kinds"}},
		{"kind aws", file(resultsProfileOf(name, bound, headers, allow, "aws")), []string{name}, []string{"no longer read", "kinds"}},
		{"no allow list", file(resultsProfileOf(name, bound, headers, "", "dpop")), []string{name}, []string{"non-empty allow list"}},
		{"an empty allow list", file(resultsProfileOf(name, bound, headers, "[]", "dpop")), []string{name}, []string{"non-empty allow list"}},
		{"the header X-Token", file(resultsProfileOf(name, bound, `["X-Token"]`, allow, "dpop")), []string{name, "X-Token"}, []string{"Authorization header alone"}},
		{"a kind's *. host over the results host", `{"kinds": [{"name": "wildcard", "hosts": ["*.ci-results.test"], "ways": ["swap"]}], "profiles": [` + right + `]}`, []string{name, "wildcard"}, []string{"binds " + resultsBoundHost + ", the host of the profile " + name}},
		// The policy reader refuses a lower-case name before the results check reads the file;
		// either refusal names the profile as written.
		{"dispatch_results_token of kind dpop", file(resultsProfileOf("dispatch_results_token", bound, headers, allow, "dpop")), []string{`dispatch_results_token`}, []string{"not an agent secret name", "dispatch_results_token of kind dpop"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Run("without --private-upstream", func(t *testing.T) { refused(t, c.body, nil, c.wants, c.faults) })
			t.Run("with --private-upstream", func(t *testing.T) {
				refused(t, c.body, []string{"--private-upstream", upstream}, c.wants, c.faults)
			})
		})
	}

	t.Run("--private-upstream: the profile of another host than the destination's", func(t *testing.T) {
		refused(t, file(resultsProfileOf(name, `"`+otherResultsHost+`"`, headers, allow, "dpop")), []string{"--private-upstream", upstream},
			[]string{name, "binds " + otherResultsHost}, []string{unnamed})
	})
	t.Run("--private-upstream: a second dpop profile of a host no destination names", func(t *testing.T) {
		refused(t, file(right, resultsProfileOf("OTHER_TOKEN", `"`+otherResultsHost+`"`, headers, allow, "dpop")), []string{"--private-upstream", upstream},
			[]string{"OTHER_TOKEN", "binds " + otherResultsHost}, []string{unnamed})
	})
	t.Run("--private-upstream: a file with no results profile", func(t *testing.T) {
		refused(t, twoProfiles, []string{"--private-upstream", upstream}, []string{"names the host " + resultsBoundHost}, []string{"no profile of BROKER_PROFILES_FILE of kind dpop binds it"})
	})

	// valid runs the subcommand and holds it to a file it would serve, answering the report.
	valid := func(t *testing.T, body string, args ...string) string {
		t.Helper()
		path := profilesFile(t, body)
		code, stdout, stderr := runCheck(t, append([]string{path}, args...)...)
		if code != 0 || stderr != "" {
			t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
		}
		if !strings.Contains(stdout, path+" is valid") {
			t.Fatalf("the report does not say the file is valid: %q", stdout)
		}
		return stdout
	}
	t.Run("--private-upstream: the right profile, with no not-checked line", func(t *testing.T) {
		if stdout := valid(t, file(right), "--private-upstream", upstream); strings.Contains(stdout, "was not checked") {
			t.Fatalf("a run given the destination says the tie was not checked: %q", stdout)
		}
	})
	t.Run("without --private-upstream: the right profile, with the not-checked line", func(t *testing.T) {
		if stdout := valid(t, file(right)); strings.Count(stdout, notSeen+"\n") != 1 {
			t.Fatalf("the report does not say once, on a line of its own, %q: %q", notSeen, stdout)
		}
	})
	t.Run("--private-upstream given empty: the right profile, with the not-checked line", func(t *testing.T) {
		if stdout := valid(t, file(right), "--private-upstream", ""); strings.Count(stdout, notSeen+"\n") != 1 {
			t.Fatalf("a --private-upstream given empty is not given, so the report says once, on a line of its own, %q: %q", notSeen, stdout)
		}
	})
	t.Run("without --private-upstream: a second dpop profile, with a not-checked line of each", func(t *testing.T) {
		stdout := valid(t, file(right, resultsProfileOf("OTHER_TOKEN", `"`+otherResultsHost+`"`, headers, allow, "dpop")))
		if strings.Count(stdout, notSeen+"\n") != 1 || strings.Count(stdout, "OTHER_TOKEN binds "+otherResultsHost+"; its tie to BROKER_PROXY_PRIVATE_UPSTREAM was not checked") != 1 {
			t.Fatalf("the report does not say once of each dpop block that its tie was not checked: %q", stdout)
		}
	})
	t.Run("without --private-upstream: a file with no results profile, with no not-checked line", func(t *testing.T) {
		if stdout := valid(t, `{"profiles": []}`); strings.Contains(stdout, "was not checked") {
			t.Fatalf("a file with no results profile says a tie was not checked: %q", stdout)
		}
	})
	t.Run("a destination the setting's parser refuses", func(t *testing.T) {
		const word = "secret-word"
		code, stdout, stderr := runCheck(t, profilesFile(t, file(right)), "--private-upstream", resultsBoundHost+"="+userinfoURL("http", "u", word, "10.0.0.5:8766"))
		if code != 2 || stdout != "" {
			t.Fatalf("exit %d with stdout %q, want exit 2 and no report", code, stdout)
		}
		for _, want := range []string{"--private-upstream", "BROKER_PROXY_PRIVATE_UPSTREAM", "carries a user", profilesCheckUsage} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("the usage error does not say %q: %q", want, stderr)
			}
		}
		if strings.Contains(stderr, word) {
			t.Fatalf("the usage error quotes a part of the URL a credential can sit in: %q", stderr)
		}
	})
}

// TestProfilesCheckHoldsTheDestinationsAddressToTheBootsRule: --private-upstream holds each
// destination to the address rule the boot holds it to (config.PrivateUpstreamAddressRules,
// netaddr.DestinationRefusal), and --cluster-ranges, the value BROKER_AIRLOCK_CLUSTER_RANGES gives
// the broker, makes it the boot's whole rule. A destination the boot refuses is no value
// BROKER_PROXY_PRIVATE_UPSTREAM takes, so its refusal is a usage error, as the setting's parser's
// is: exit 2, no report, and on stderr one line naming BROKER_PROXY_PRIVATE_UPSTREAM and the rule,
// then the usage line. Each destination below that the boot refuses whatever its cluster ranges -
// loopback, localhost with and without the root's dot, the metadata servers, the IPv4 broadcast
// and unspecified addresses, 0.0.0.0/8 and the NAT64 form of the metadata server - is refused so
// without --cluster-ranges and with every ranges given: none, the Service range, every address. A
// public address, and a ClusterIP of a global Service range, are a destination only inside a
// broker's BROKER_AIRLOCK_CLUSTER_RANGES: without --cluster-ranges each is valid with one line
// that says it was not judged against the cluster ranges; given ranges that do not hold it, it is
// refused so naming both settings, as the boot refuses it; given ranges that hold it, it is valid
// with no such line. A private address, one of 100.64.0.0/10, a unique-local one and a name are
// valid with no such line, with any ranges; a name is the dial's to check, at the boot too. A
// --cluster-ranges value the setting's parser refuses, and one given without --private-upstream,
// are usage errors.
func TestProfilesCheckHoldsTheDestinationsAddressToTheBootsRule(t *testing.T) {
	path := profilesFile(t, `{"profiles": [`+resultsProfileOf("DISPATCH_RESULTS_TOKEN", `"`+resultsBoundHost+`"`, `["Authorization"]`, `[{"method": "POST", "pathPrefix": "/api/v1/projects/"}]`, "dpop")+`]}`)
	const (
		notJudged = "it was not judged against the cluster ranges (no --cluster-ranges)"
		// serviceRange is GKE's default Service range, global unicast; everything holds every
		// address, IPv4 and IPv6, in the setting's form with a space after the comma.
		serviceRange = "34.118.224.0/20"
		everything   = "0.0.0.0/0, ::/0"
	)
	// addressRefused holds a run to the refusal of a destination's address: exit 2, no report, and
	// two lines on stderr, the first saying each of wants, the second the usage line.
	addressRefused := func(t *testing.T, code int, stdout, stderr string, wants ...string) {
		t.Helper()
		if code != 2 || stdout != "" {
			t.Fatalf("exit %d with stdout %q; want exit 2 and no report, as the boot refuses the destination at start", code, stdout)
		}
		lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
		if len(lines) != 2 || lines[1] != profilesCheckUsage {
			t.Fatalf("the refusal reads %q; want one line of the refusal and then the usage line", stderr)
		}
		for _, want := range wants {
			if !strings.Contains(lines[0], want) {
				t.Fatalf("the refusal reads %q; want it to say %q", lines[0], want)
			}
		}
	}
	// Each ranges a run can give: none (no flag), and the flag given empty, with the Service
	// range, and with every address.
	rangesRuns := []struct {
		name string
		args []string
	}{
		{"no --cluster-ranges", nil},
		{"--cluster-ranges empty", []string{"--cluster-ranges", ""}},
		{"--cluster-ranges " + serviceRange, []string{"--cluster-ranges", serviceRange}},
		{"--cluster-ranges " + everything, []string{"--cluster-ranges", everything}},
	}
	for _, row := range []struct{ url, rule string }{
		{"http://127.0.0.1:8766", "a loopback or link-local address"},
		{"http://[::1]:8766", "a loopback or link-local address"},
		{"http://localhost:8766", "which resolves to loopback"},
		{"http://localhost.:8766", "which resolves to loopback"},
		{"http://169.254.169.254", "a loopback or link-local address"},
		{"http://[fd20:ce::254]", "a cloud metadata server's address"},
		{"http://255.255.255.255:8766", "the IPv4 broadcast address"},
		{"http://0.0.0.0:8766", "an unspecified or multicast address"},
		{"http://0.0.0.1:8766", "an address of 0.0.0.0/8"},
		{"http://[64:ff9b::a9fe:a9fe]", "carries an IPv4 address"},
	} {
		for _, ranges := range rangesRuns {
			t.Run("refused "+row.url+", "+ranges.name, func(t *testing.T) {
				code, stdout, stderr := runCheck(t, append([]string{path, "--private-upstream", resultsBoundHost + "=" + row.url}, ranges.args...)...)
				addressRefused(t, code, stdout, stderr, "--private-upstream", "BROKER_PROXY_PRIVATE_UPSTREAM", row.rule)
			})
		}
	}
	for _, row := range []struct {
		url string
		// outside is an address outside the private ranges, which only the cluster ranges admit;
		// inServiceRange says whether serviceRange holds it.
		outside, inServiceRange bool
	}{
		{"http://8.8.8.8:8766", true, false},
		{"http://203.0.113.5:8766", true, false},
		{"http://[2001:4860:4860::8888]:8766", true, false},
		{"http://34.118.224.10:8766", true, true},
		{"http://10.0.0.5:8766", false, false},
		{"http://100.64.1.1:8766", false, false},
		{"http://[fd12:3456::5]:8766", false, false},
		{"http://dispatch.dispatch.svc.cluster.local:8766", false, false},
	} {
		for _, ranges := range rangesRuns {
			held := ranges.args == nil || !row.outside || ranges.args[1] == everything || ranges.args[1] == serviceRange && row.inServiceRange
			t.Run(row.url+", "+ranges.name, func(t *testing.T) {
				code, stdout, stderr := runCheck(t, append([]string{path, "--private-upstream", resultsBoundHost + "=" + row.url}, ranges.args...)...)
				if !held {
					addressRefused(t, code, stdout, stderr, "BROKER_PROXY_PRIVATE_UPSTREAM", "outside the private ranges and BROKER_AIRLOCK_CLUSTER_RANGES")
					return
				}
				if code != 0 {
					t.Fatalf("exit %d (%q); want 0", code, stderr)
				}
				switch said := strings.Count(stdout, notJudged+"\n"); {
				case (ranges.args != nil || !row.outside) && said != 0:
					t.Fatalf("the report says the address was not judged against the ranges: %q", stdout)
				case ranges.args == nil && row.outside && said != 1:
					t.Fatalf("the report does not say once, on a line of its own, that the address was not judged against the ranges: %q", stdout)
				}
			})
		}
	}
	for _, run := range []struct {
		name  string
		args  []string
		wants []string
	}{
		{"--cluster-ranges without --private-upstream", []string{"--cluster-ranges", serviceRange}, []string{"--cluster-ranges judges the address of each destination --private-upstream gives, and was given without one"}},
		{"--cluster-ranges not a prefix", []string{"--private-upstream", resultsBoundHost + "=http://10.0.0.5:8766", "--cluster-ranges", serviceRange + ", not-a-prefix"}, []string{"--cluster-ranges: BROKER_AIRLOCK_CLUSTER_RANGES: element 2 is not a CIDR prefix"}},
		{"--cluster-ranges an IPv4-mapped prefix", []string{"--private-upstream", resultsBoundHost + "=http://10.0.0.5:8766", "--cluster-ranges", "::ffff:34.118.224.0/116"}, []string{"element 1 is an IPv4-mapped IPv6 prefix"}},
	} {
		t.Run(run.name, func(t *testing.T) {
			code, stdout, stderr := runCheck(t, append([]string{path}, run.args...)...)
			if code != 2 || stdout != "" {
				t.Fatalf("exit %d with stdout %q; want exit 2 and no report", code, stdout)
			}
			for _, want := range append(run.wants, profilesCheckUsage) {
				if !strings.Contains(stderr, want) {
					t.Fatalf("the usage error does not say %q: %q", want, stderr)
				}
			}
			if strings.Contains(stderr, "not-a-prefix") {
				t.Fatalf("the usage error quotes the element: %q", stderr)
			}
		})
	}
}

// TestProfilesCheckReportsWhatChangedAgainstTheOldFile is the report a reviewer reads: for a kind
// and a profile both files hold, each value of a list added or removed and each other field as
// old → new, a field the file leaves out written in the words of what the broker does and a field
// it leaves to the source written as the source's; a profile removed, with every field it had,
// its proxy block in an old file's form among them; and, under widens, each row of a name's allow
// under the new file its old allow does not cover, with the hosts the change adds to it and the
// names whose kind no tag read gave. The whole of stdout is asserted. GITHUB_READ is behind its
// proxy string's kind and is held to everything the file decides of it; LEGACY, under the new
// file a stored secret behind a kind only by its own tag, is not compared; and the github kind,
// which both files declare, is compared on its own, since a secret behind it by its own tag is
// not named. So the run exits 3.
func TestProfilesCheckReportsWhatChangedAgainstTheOldFile(t *testing.T) {
	old := profilesFile(t, `{"kinds": [{"name": "github", "hosts": ["api.github.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/v1/"}]}],
 "profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}}, "proxy": "github"},
  {"name": "LEGACY", "proxy": {"hosts": ["old.example.com"]}}
]}`)
	now := profilesFile(t, `{"kinds": [{"name": "github", "hosts": ["api.github.com", "uploads.github.com"], "headers": ["authorization", "x-api-key"], "variables": ["GH_TOKEN"],
   "ways": ["swap", "mint"], "allow": [{"method": "POST", "pathPrefix": "/v1/"}], "placeholder_prefix": "tml-", "max_grant": "12h"}],
 "profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon"], "maxGrant": "1h",
   "github": {"accounts": ["example-org", "ada"], "repositories": ["ada/proofs", "example-org/legion"], "permissions": {"contents": "read", "metadata": "read", "pull_requests": "read"}, "value": "token"},
   "proxy": "github", "allow": [{"method": "POST", "pathPrefix": "/v1/repos/", "host": "uploads.github.com"}]}
]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	wantErr := "profiles check: " + now + ": UNKNOWN: not read (no --secrets-json): the allow or hosts of 1 name against " + old +
		", and the names behind 1 kind by their own proxy tag, compared as their kind alone; nothing it read is refused\n"
	if code != exitUnknown || stderr != wantErr {
		t.Fatalf("a valid file against an old one = exit %d with stderr %q, want exit %d and %q", code, stderr, exitUnknown, wantErr)
	}
	want := fmt.Sprintf(`profiles check: %[1]s is valid: 1 profile and 1 kind
profiles check: compared against %[2]s
kind "github": hosts uploads.github.com added
kind "github": headers x-api-key added
kind "github": variables GH_TOKEN added
kind "github": ways mint added
kind "github": allow GET /v1/ removed
kind "github": allow POST /v1/ added
kind "github": placeholder_prefix the default, agent-secrets-placeholder- → tml-
kind "github": max_grant none → 12h
profile "GITHUB_READ": tier the source's → agent
profile "GITHUB_READ": requesters the source's → legion-daemon
profile "GITHUB_READ": maxGrant the source's → 1h0m0s
profile "GITHUB_READ": allow none → POST /v1/repos/ on uploads.github.com
profile "GITHUB_READ": github.accounts ada added
profile "GITHUB_READ": github.repositories every repository the installation reaches → ada/proofs example-org/legion
profile "GITHUB_READ": github.permissions pull_requests read added
profile "GITHUB_READ": github.value installations → token
profile "LEGACY": removed
profile "LEGACY": source LEGACY
profile "LEGACY": tier the source's
profile "LEGACY": requesters the source's
profile "LEGACY": maxGrant the source's
profile "LEGACY": proxy none
profile "LEGACY": allow none
profile "LEGACY": github none
profile "LEGACY": google none
profile "LEGACY": proxy.hosts old.example.com
profile "LEGACY": proxy.headers authorization
profile "LEGACY": proxy.allow any method and path
profile "LEGACY": proxy.kind ""
widens "GITHUB_READ": POST /v1/repos/ on uploads.github.com
widens "LEGACY": not compared: under %[1]s no profile holds the name, and its stored secret is behind a kind only by its own proxy tag, which only --secrets-json reads
widens kind "github": POST /v1/
hosts "GITHUB_READ": uploads.github.com added
hosts kind "github": uploads.github.com added
profiles check: the kinds both files declare were compared on their own (1 kind): a stored secret behind one by its own proxy tag, which only --secrets-json reads, takes that kind's allow and hosts, and none was named
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
	now := profilesFile(t, `{"profiles": [{"name": "LEGION_IMPLEMENT_APP", "proxy": {"hosts": ["dispatch.test"], "headers": ["Authorization"], "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"}}]}`)
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
		`profile "LEGION_IMPLEMENT_APP": proxy block added`,
		`profile "LEGION_IMPLEMENT_APP": proxy.hosts dispatch.test`,
		`profile "LEGION_IMPLEMENT_APP": proxy.headers Authorization`,
		`profile "LEGION_IMPLEMENT_APP": proxy.allow POST /api/v1/`,
		`profile "LEGION_IMPLEMENT_APP": proxy.kind dpop`,
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
// report, and the default headers with it: a kind that names no header carries the credential in
// authorization, so a file that writes "headers": [] has taken that header away, and the report
// says so rather than treating the two as the same empty field. A host removed widens nothing, so
// the kind, compared on its own, lists nothing under widens; the run exits 3 all the same, since a
// secret behind it by its own tag is not named.
func TestProfilesCheckReportsAHostRemovedAndAHeaderRemoved(t *testing.T) {
	old := profilesFile(t, `{"kinds": [{"name": "github", "hosts": ["api.github.com", "github.com"], "ways": ["swap"]}], "profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": "github"}]}`)
	now := profilesFile(t, `{"kinds": [{"name": "github", "hosts": ["api.github.com"], "headers": [], "ways": ["swap"]}], "profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": "github"}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != exitUnknown || !strings.Contains(stderr, "the names behind 1 kind by their own proxy tag") {
		t.Fatalf("exit %d with stderr %q, want exit %d naming the one kind compared on its own", code, stderr, exitUnknown)
	}
	if got := slices.Concat(linesWith(stdout, "widens kind "), linesWith(stdout, "hosts kind ")); got != nil || !strings.Contains(stdout, "widens: none among what was compared\n") {
		t.Fatalf("a kind that only loses a host and a header lists %q under widens, want nothing:\n%s", got, stdout)
	}
	for _, want := range []string{
		`kind "github": hosts github.com removed`,
		`kind "github": headers authorization removed`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the report does not say %q:\n%s", want, stdout)
		}
	}
}

// TestProfilesCheckReportsTheDefaultHeaderTheSwapApplies: the header a kind with no "headers"
// field reads as, in this report, is the one proxyrules.Rewrite actually swaps in, read from the
// airlock package itself, so a change to that default cannot leave the report saying a header was
// added or removed that the swap says nothing about.
func TestProfilesCheckReportsTheDefaultHeaderTheSwapApplies(t *testing.T) {
	names := proxyrules.Kind{}.HeaderNames()
	if len(names) != 1 {
		t.Fatalf("a kind with no headers field names %v; this report reads it as one header", names)
	}
	added := profilesFile(t, `{"kinds": [{"name": "github", "hosts": ["api.github.com"], "ways": ["swap"]}], "profiles": []}`)
	code, stdout, stderr := runCheck(t, added)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if want := fmt.Sprintf("kind %q: headers %s\n", "github", names[0]); !strings.Contains(stdout, want) {
		t.Fatalf("the report does not say %q:\n%s", want, stdout)
	}
}

// TestProfilesCheckSaysWhenNothingChanged pins the one line a pull request that changed something
// else gets: a report of no lines at all would read as a report that had not run. A file that
// spells a profile another way, an account in upper case, changes nothing the broker serves, and
// widens nothing.
func TestProfilesCheckSaysWhenNothingChanged(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "GITHUB_READ", "github": {"accounts": ["Example-Org"], "permissions": {"contents": "read", "metadata": "read"}}}]}`)
	now := profilesFile(t, `{"profiles": [{"name": "GITHUB_READ", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "installations"}}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	if !strings.HasSuffix(stdout, "profiles check: nothing changed\nwidens: none\n") {
		t.Fatalf("an unchanged file's report does not end in the nothing-changed line and an empty widens list:\n%s", stdout)
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
// and not held to the file's rules (policy.DecodeProfiles): the profiles in force may be in the
// form of proxy blocks the kinds replaced, here one that still sets the judge and guidance a key's
// own tags now carry, and a check that refused them would refuse the pull request that moves the
// file to kinds. The block is read, and its judge and guidance dropped, so the report says the
// block went and the name is behind a kind now, with what that adds to it, and nothing of them.
func TestProfilesCheckComparesAgainstAnOldFileTheBrokerWouldRefuse(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 1800, "guidance": "grant it for the release job"}}]}`)
	now := profilesFile(t, `{"kinds": [{"name": "github", "hosts": ["api.github.com", "github.com"], "ways": ["swap"]}], "profiles": [{"name": "GITHUB_ADMIN_PAT", "proxy": "github"}]}`)
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	lines := strings.Split(stdout, "\n")
	for _, want := range []string{
		`profile "GITHUB_ADMIN_PAT": proxy none → github`,
		`profile "GITHUB_ADMIN_PAT": proxy block removed`,
		`profile "GITHUB_ADMIN_PAT": proxy.hosts api.github.com`,
		"widens: none",
		`hosts "GITHUB_ADMIN_PAT": github.com added`,
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("the report does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "judge") || strings.Contains(stdout, "guidance") {
		t.Fatalf("the report names the old block's judge or guidance:\n%s", stdout)
	}
}

// TestProfilesCheckComparesTheKindsAFileDeclares: an old file that declares no kinds serves each
// proxy block behind a kind of its own (policy.ParseProfiles), named for its profile, and the report
// compares the kinds each file declares, not those: a new file that declares a kind of the name an
// old block's own kind has, here token for the profile TOKEN, adds that kind. So the run writes
// `kind "token": added` with every field of it, compares no kind both files declare, writes no
// UNKNOWN, and exits 0. The control is the same new file with the kind named bearer, which no old
// block's kind is named.
func TestProfilesCheckComparesTheKindsAFileDeclares(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "TOKEN", "proxy": {"hosts": ["api.example.com"]}}]}`)
	for _, kind := range []string{"token", "bearer"} {
		t.Run(kind, func(t *testing.T) {
			now := profilesFile(t, `{"kinds": [{"name": "`+kind+`", "hosts": ["api.example.com"], "ways": ["swap", "mint"]}], "profiles": [{"name": "TOKEN", "proxy": "`+kind+`"}]}`)
			code, stdout, stderr := runCheck(t, now, "--against", old)
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr:\n%s", code, stderr, stdout)
			}
			lines := strings.Split(stdout, "\n")
			for _, want := range []string{
				fmt.Sprintf("kind %q: added", kind),
				fmt.Sprintf("kind %q: hosts api.example.com", kind),
				fmt.Sprintf(`profile "TOKEN": proxy none → %s`, kind),
				`profile "TOKEN": proxy block removed`,
				"widens: none",
			} {
				if !slices.Contains(lines, want) {
					t.Errorf("the report does not say %q:\n%s", want, stdout)
				}
			}
			if strings.Contains(stdout, "UNKNOWN") || strings.Contains(stdout, "widens kind") {
				t.Errorf("the report compares a kind the old file does not declare:\n%s", stdout)
			}
		})
	}
}

// TestProfilesCheckServesNoTagByAKindAFileOfNoKindsMadeForABlock: under an old file that declares
// no kinds, a source's proxy tag names no kind, not even one of the name an old block's own kind
// has (the policy load refuses such a source, proxy-kind-unknown), so a profile of that source with
// no block was behind nothing under the old file, and the report lists only the name behind a
// block as behind the proxy there. Here TOKEN's block is behind the kind token, and OTHER's source
// OTHER_SRC carries proxy=token, which the new file declares.
func TestProfilesCheckServesNoTagByAKindAFileOfNoKindsMadeForABlock(t *testing.T) {
	old := profilesFile(t, `{"profiles": [{"name": "TOKEN", "proxy": {"hosts": ["api.example.com"]}}, {"name": "OTHER", "source": "OTHER_SRC"}]}`)
	now := profilesFile(t, `{"kinds": [{"name": "token", "hosts": ["api.example.com"], "ways": ["swap", "mint"]}], "profiles": [{"name": "TOKEN", "proxy": "token"}, {"name": "OTHER", "source": "OTHER_SRC"}]}`)
	tags := writeTempFile(t, "secrets.json", []byte(`[
  {"name": "projects/1/secrets/agent-secrets-token", "annotations": {"tier": "agent"}},
  {"name": "projects/1/secrets/agent-secrets-other-src", "annotations": {"tier": "agent", "proxy": "token"}}
]`))
	code, stdout, stderr := runCheck(t, now, "--against", old, "--secrets-json", tags)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d with stderr %q, want exit 0 and nothing on stderr:\n%s", code, stderr, stdout)
	}
	if served := linesWith(stdout, "served "); len(served) != 1 || !strings.HasPrefix(served[0], `served "TOKEN": `) {
		t.Errorf("the names behind the proxy under the old file are %q; want TOKEN alone, behind its block", served)
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
		{"--secrets-json with no file", []string{path, "--secrets-json"}},
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
// configuration and no database, and its exit status reaches the process, here 3 for a file one of
// whose profiles' kinds only the store's tags give; `profiles` with no second word, or another, is
// a usage error; the removed `airlock-check` exits 2 naming the command that replaces it, and reads
// no file; and `end-sessions` still reaches its own rather than being swallowed by the new one.
func TestTheBinaryRoutesEachSubcommandWord(t *testing.T) {
	binPath := buildBroker(t)
	path := profilesFile(t, twoProfiles)

	out, err := exec.Command(binPath, profilesCommand, profilesCheckCommand, path).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitUnknown {
		t.Fatalf("broker profiles check %s = %v, want exit %d\n%s", path, err, exitUnknown, out)
	}
	t.Logf("broker profiles check %s printed:\n%s", path, out)
	for _, want := range []string{"is valid: 2 profiles and 2 kinds", `kind "openai": hosts api.openai.com`, `UNKNOWN: profile "GITHUB_ADMIN_PAT"`} {
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
	if out := exitsTwo(t, []string{"airlock-check is removed", "broker profiles check <file> [--against <old file>] [--secrets-json <file>]"}, "airlock-check", path); strings.Contains(out, "is valid") {
		t.Fatalf("the removed airlock-check read the file: %s", out)
	}
	exitsTwo(t, []string{"usage: broker " + endSessionsCommand + " <login>"}, endSessionsCommand)
}

// userinfoURL is scheme://user:password@rest, built here so no test line spells a URL that
// carries a credential in the form a secret scanner reads as one (as config_test.go's helper).
func userinfoURL(scheme, user, password, rest string) string {
	return scheme + "://" + user + ":" + password + "@" + rest
}
