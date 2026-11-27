package policy_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// githubRead is a profile the broker serves: a GitHub App's reads on one account, as a bare token
// behind the proxy, under the github kind its proxy string names. It is the first profile of each
// malformed row's file, so a row's refusal must name the second profile, profiles[1], and not the
// file's first.
const githubRead = `{"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent",
  "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
  "proxy": "github"}`

// profilesFile is a profiles file holding the profiles given, in order, and the test kinds
// (policytest.Kinds) after them, on the line they end on, so a row's line and column are its own.
func profilesFile(profiles ...string) string {
	return `{"profiles": [` + strings.Join(profiles, ",\n") + `], "kinds": ` + policytest.Kinds + `}`
}

// githubWrite is the second profile of a malformed row: a GitHub profile whose github block is
// the row's.
func githubWrite(block string) string {
	return `{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "github": ` + block + `}`
}

// adminPAT is the second profile of a malformed row about a proxy key: a stored value behind the
// proxy, whose proxy key is the row's.
func adminPAT(proxy string) string {
	return `{"name": "GITHUB_ADMIN_PAT", "proxy": ` + proxy + `}`
}

// minted is the second profile of a malformed row about a dpop block, the one proxy block a kinds
// file serves: a capability the broker mints and holds, whose block is the row's.
func minted(block string) string {
	return `{"name": "TEST_MINTED_TOKEN", "proxy": ` + block + `}`
}

// repositories is n repositories of example-org, as github.repositories writes them.
func repositories(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf(`"example-org/repo-%03d"`, i)
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// TestParseProfilesRefusesEachMalformedField holds one row per rule of the profiles file's shape:
// each refuses the file, and its refusal names the profile by its index and its name and the field
// it refuses, so the author of the pull request can find it. The control is the rows' own first
// profile, with a profile of each other kind beside it: a file the broker serves.
func TestParseProfilesRefusesEachMalformedField(t *testing.T) {
	for _, c := range []struct {
		name    string
		profile string
		field   string
		want    []string
	}{
		{"a profile with no name", `{"source": "LEGION_IMPLEMENT_APP", "proxy": "github"}`, "name", []string{"required"}},
		{"a name in lower case", `{"name": "github_write", "proxy": "github"}`, "name", []string{"not an agent secret name"}},
		{"a name written with hyphens", `{"name": "GITHUB-WRITE", "proxy": "github"}`, "name", []string{"not an agent secret name"}},
		{"a name another profile holds", githubRead, "name", []string{"profiles[0]"}},
		{"a source in lower case", `{"name": "GITHUB_WRITE", "source": "legion-implement-app", "proxy": "github"}`, "source", []string{"not an agent secret name"}},
		{"a tier that is neither agent nor human", `{"name": "GITHUB_WRITE", "tier": "admin", "proxy": "github"}`, "tier", []string{`"admin"`}},
		{"requesters present and empty", `{"name": "GITHUB_WRITE", "requesters": [], "proxy": "github"}`, "requesters", []string{"nobody"}},
		{"a requester in upper case", `{"name": "GITHUB_WRITE", "requesters": ["Ada@Example.com"], "proxy": "github"}`, "requesters[0]", []string{"Ada@Example.com"}},
		{"two requesters in one entry", `{"name": "GITHUB_WRITE", "requesters": ["ada@example.com", "bob@example.com legion-daemon"], "proxy": "github"}`, "requesters[1]", []string{"one entry"}},
		{"a requester list with a comma", `{"name": "GITHUB_WRITE", "requesters": ["ada@example.com,bob@example.com"], "proxy": "github"}`, "requesters[0]", []string{"comma"}},
		{"a maxGrant that is no duration", `{"name": "GITHUB_WRITE", "maxGrant": "a day", "proxy": "github"}`, "maxGrant", []string{`"a day"`}},
		{"a maxGrant under a second", `{"name": "GITHUB_WRITE", "maxGrant": "500ms", "proxy": "github"}`, "maxGrant", []string{"1s"}},
		{"a github block with no accounts", githubWrite(`{"permissions": {"contents": "read"}}`), "github.accounts", []string{"required"}},
		{"an empty accounts list", githubWrite(`{"accounts": [], "permissions": {"contents": "read"}}`), "github.accounts", []string{"required"}},
		{"an account that is no GitHub login", githubWrite(`{"accounts": ["example-org", "-ada"], "permissions": {"contents": "read"}}`), "github.accounts[1]", []string{`"-ada"`}},
		{"an account twice, in two cases", githubWrite(`{"accounts": ["Example-Org", "example-org"], "permissions": {"contents": "read"}}`), "github.accounts[1]", []string{"twice"}},
		{"an empty repositories list", githubWrite(`{"accounts": ["example-org"], "repositories": [], "permissions": {"contents": "read"}}`), "github.repositories", []string{"empty"}},
		{"a repository with no account", githubWrite(`{"accounts": ["example-org"], "repositories": ["legion"], "permissions": {"contents": "read"}}`), "github.repositories[0]", []string{"account/name"}},
		{"a repository of an account the profile does not name", githubWrite(`{"accounts": ["example-org"], "repositories": ["example-org/legion", "other-org/legion"], "permissions": {"contents": "read"}}`), "github.repositories[1]", []string{"other-org", "github.accounts"}},
		{"an account no repository names", githubWrite(`{"accounts": ["example-org", "Ada"], "repositories": ["example-org/legion"], "permissions": {"contents": "read", "metadata": "read"}}`), "github.accounts[1]", []string{"names ada", "github.repositories names no repository of it"}},
		{"a repository name GitHub cannot hold", githubWrite(`{"accounts": ["example-org"], "repositories": ["example-org/a b"], "permissions": {"contents": "read"}}`), "github.repositories[0]", []string{"letters, digits"}},
		{"the repository name ..", githubWrite(`{"accounts": ["example-org"], "repositories": ["example-org/.."], "permissions": {"contents": "read"}}`), "github.repositories[0]", []string{"letters, digits"}},
		{"more repositories than a token takes", githubWrite(`{"accounts": ["example-org"], "repositories": ` + repositories(githubname.MaxNarrowedRepositories+1) + `, "permissions": {"contents": "read"}}`), "github.repositories", []string{fmt.Sprint(githubname.MaxNarrowedRepositories + 1), fmt.Sprint(githubname.MaxNarrowedRepositories)}},
		{"a github block with no permissions", githubWrite(`{"accounts": ["example-org"]}`), "github.permissions", []string{"required"}},
		{"an empty permissions object", githubWrite(`{"accounts": ["example-org"], "permissions": {}}`), "github.permissions", []string{"required"}},
		{"a permission name in upper case", githubWrite(`{"accounts": ["example-org"], "permissions": {"Contents": "read"}}`), "github.permissions", []string{`"Contents"`}},
		{"a level GitHub does not give", githubWrite(`{"accounts": ["example-org"], "permissions": {"contents": "none"}}`), "github.permissions.contents", []string{`"none"`, "read, write or admin"}},
		{"workflows at read", githubWrite(`{"accounts": ["example-org"], "permissions": {"contents": "write", "workflows": "read"}}`), "github.permissions.workflows", []string{`"read"`, "write alone"}},
		{"profile at admin", githubWrite(`{"accounts": ["example-org"], "permissions": {"profile": "admin"}}`), "github.permissions.profile", []string{`"admin"`, "write alone"}},
		{"a github block that names no metadata", githubWrite(`{"accounts": ["example-org"], "permissions": {"contents": "read"}}`), "github.permissions", []string{`"metadata": "read"`, "every installation token"}},
		{"metadata at write", githubWrite(`{"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "write"}}`), "github.permissions.metadata", []string{`"write"`, "read alone"}},
		{"a value that is neither", githubWrite(`{"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "raw"}`), "github.value", []string{`"raw"`, "installations", "token"}},
		{"a google block with no scopes", `{"name": "GOOGLE_READ", "google": {}, "proxy": "gcp"}`, "google.scopes", []string{"required"}},
		{"an empty scopes list", `{"name": "GOOGLE_READ", "google": {"scopes": []}, "proxy": "gcp"}`, "google.scopes", []string{"required"}},
		{"a whole scope URL", `{"name": "GOOGLE_READ", "google": {"scopes": ["https://www.googleapis.com/auth/cloud-platform"]}}`, "google.scopes[0]", []string{"after https://www.googleapis.com/auth/"}},
		{"a scope twice", `{"name": "GOOGLE_READ", "google": {"scopes": ["cloud-platform", "cloud-platform"]}}`, "google.scopes[1]", []string{"twice"}},
		{"github and google together", `{"name": "GITHUB_WRITE", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}}, "google": {"scopes": ["cloud-platform"]}}`, "github and google", []string{"both"}},
		{"a proxy string naming no kind of the file", adminPAT(`"gcs"`), "proxy", []string{`kind "gcs"`, "do not declare"}},
		{"a proxy block of no kind, which the kinds replaced", adminPAT(`{"hosts": ["api.github.com"]}`), "proxy", []string{"no longer read", "kinds", "GITHUB_ADMIN_PAT", "proxy=<kind>"}},
		{"an aws proxy block, which the kinds replaced", adminPAT(`{"hosts": ["sts.amazonaws.com"], "kind": "aws"}`), "proxy", []string{"no longer read", "kinds"}},
		{"a judged proxy block of a profile of another source", `{"name": "GITHUB_ADMIN_READ", "source": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 300}}`, "proxy", []string{"no longer read", "the source, GITHUB_ADMIN_PAT"}},
		{"a dpop block with no hosts", minted(`{"headers": ["x-api-key"], "kind": "dpop"}`), "proxy.hosts", []string{"required"}},
		{"a dpop block with an empty hosts list", minted(`{"hosts": [], "kind": "dpop"}`), "proxy.hosts", []string{"required"}},
		{"a dpop host with a scheme", minted(`{"hosts": ["dispatch.example.com", "https://github.com"], "kind": "dpop"}`), "proxy.hosts[1]", []string{"holds a scheme"}},
		{"a dpop allow row that names a host", minted(`{"hosts": ["dispatch.example.com"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/", "host": "dispatch.example.com"}]}`), "proxy", []string{"names the host", "dispatch.example.com"}},
		{"a judge in the proxy block of a dpop profile", minted(`{"hosts": ["dispatch.example.com"], "kind": "dpop", "judge": 300}`), "proxy.judge", []string{"kind dpop has no judge", "its kind decides its grant"}},
		{"a judge of null in the proxy block of a dpop profile", minted(`{"hosts": ["dispatch.example.com"], "kind": "dpop", "judge": null}`), "proxy.judge", []string{"kind dpop has no judge"}},
		{"guidance in the proxy block of a dpop profile", minted(`{"hosts": ["dispatch.example.com"], "kind": "dpop", "guidance": "grant it for the release job"}`), "proxy.guidance", []string{"kind dpop has no judge", "its kind decides its grant"}},
		{"an allow of its own beside a dpop block", `{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "kind": "dpop"}, "allow": []}`, "allow", []string{"kind dpop"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := policy.ParseProfiles([]byte(profilesFile(githubRead, c.profile)))
			if err == nil {
				t.Fatalf("ParseProfiles served a file with %s", c.name)
			}
			for _, want := range append([]string{"profiles[1]", c.field}, c.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "profiles[0]") && c.want[0] != "profiles[0]" {
				t.Errorf("the refusal %q names the file's first profile, which is valid", err)
			}
		})
	}
	t.Run("the control: a profile of each kind", func(t *testing.T) {
		valid := profilesFile(githubRead,
			githubWrite(`{"accounts": ["example-org", "ada"], "repositories": ["example-org/legion", "ada/proofs"], "permissions": {"contents": "write", "metadata": "read", "workflows": "write", "profile": "write", "administration": "admin"}, "value": "installations"}`),
			`{"name": "GOOGLE_READ", "source": "GCS_READER", "requesters": ["ada@example.com", "legion-daemon"], "maxGrant": "1h", "google": {"scopes": ["devstorage.read_only"]}}`,
			adminPAT(`"github", "allow": [{"method": "GET", "pathPrefix": "/", "host": "api.github.com"}]`),
			minted(`{"hosts": ["dispatch.example.com", "*.example.org"], "headers": [], "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"}`),
		)
		if _, err := policy.ParseProfiles([]byte(valid)); err != nil {
			t.Fatalf("ParseProfiles of a file of valid profiles: %v", err)
		}
	})
}

// TestParseProfilesRefusesUnknownFieldsAndASecondValue: a field the broker does not know, at any
// depth of the file, is refused rather than ignored, so a file written for a later broker is not
// served with that field's rule missing; so is anything after the file's one object, which its
// author would believe in force, a second value, a stray bracket or a word alike. A file that is
// not JSON, holds a field of the wrong type or holds a field the broker does not know is refused
// naming where: an unknown field by where its name starts, the second "name" of the proxy block's
// row and not the profile's own, and text that is not JSON by the byte at fault, the trailing
// comma of a list and not the bracket after it. A file that is empty, white space alone, or ends
// inside its object holds no object and says so. A file that is not the one object
// {"profiles": [...]} (a null, a list, a string, an object with no profiles list or a null one)
// is refused naming what it is, never read as a file of no profiles: a file of no profiles says
// so, {"profiles": []}. A proxy key takes a kind's name or a proxy block alone: a number, a list,
// a boolean and an empty string are each refused, naming the key, what it is and the byte where
// it starts.
func TestParseProfilesRefusesUnknownFieldsAndASecondValue(t *testing.T) {
	for name, c := range map[string]struct {
		file string
		want []string
	}{
		"a field of the file":          {`{"profiles": [], "secrets": []}`, []string{`unknown field "secrets"`, "byte offset 18, line 1, column 18"}},
		"a field of a profile":         {profilesFile(githubRead, `{"name": "GITHUB_ADMIN_PAT", "lifetime": 5, "proxy": "github"}`), []string{`unknown field "lifetime"`, "byte offset", "line 4, column 30"}},
		"a field of a github block":    {profilesFile(githubWrite(`{"accounts": ["example-org"], "owner": "example-org", "permissions": {"contents": "read"}}`)), []string{`unknown field "owner"`, "byte offset 114, line 1, column 114"}},
		"a field of a google block":    {profilesFile(`{"name": "GOOGLE_READ", "google": {"scopes": ["cloud-platform"], "audience": "x"}}`), []string{`unknown field "audience"`, "byte offset 80, line 1, column 80"}},
		"a name in a proxy block":      {profilesFile(adminPAT(`{"name": "GITHUB_ADMIN_PAT", "hosts": ["api.github.com"]}`)), []string{`unknown field "name"`, "byte offset 54, line 1, column 54"}},
		"a field of an allow entry":    {profilesFile(adminPAT(`{"hosts": ["api.github.com"], "allow": [{"method": "GET", "path": "/"}]}`)), []string{`unknown field "path"`, "byte offset 111, line 1, column 111"}},
		"a field of a profile's allow": {profilesFile(adminPAT(`"github", "allow": [{"method": "GET", "path": "/"}]`)), []string{`unknown field "path"`, "profiles.0.allow.0"}},
		"a proxy that is a number":     {profilesFile(adminPAT(`7`)), []string{"profiles.0.proxy is a JSON number", "the name of one of the file's kinds", "proxy block of kind dpop", "byte offset 53, line 1, column 53"}},
		"a proxy that is a list":       {profilesFile(adminPAT(`["github"]`)), []string{"profiles.0.proxy is a JSON array", "the name of one of the file's kinds", "byte offset 53"}},
		"a proxy that is a boolean":    {profilesFile(adminPAT(`true`)), []string{"profiles.0.proxy is a JSON boolean", "the name of one of the file's kinds"}},
		"a proxy that is empty":        {profilesFile(adminPAT(`""`)), []string{"profiles.0.proxy is an empty string", "the name of one of the file's kinds", "byte offset 53"}},
		"a field of a kind":            {`{"profiles": [], "kinds": [{"name": "kv", "hosts": ["kv.example.com"], "ways": ["swap"], "port": 443}]}`, []string{`unknown field "port"`, "in kinds.0"}},
		"kinds that is no list":        {`{"profiles": [], "kinds": {"name": "kv"}}`, []string{"kinds", "byte offset 27, line 1, column 27"}},
		"a second value":               {profilesFile(githubRead) + "\n" + profilesFile(), []string{"goes on after its one JSON object", "line 4, column 1"}},
		"a stray closing brace":        {profilesFile(githubRead) + "\n}\n", []string{"goes on after its one JSON object", "line 4, column 1"}},
		"a stray closing bracket":      {"{\"profiles\": []}\n  ]", []string{"goes on after its one JSON object", "line 2, column 3"}},
		"a word after the object":      {`{"profiles": []} done`, []string{"goes on after its one JSON object", "byte offset 18, line 1, column 18"}},
		"a file that is not JSON":      {"{\"profiles\": [\n  " + githubRead + ",\n]}", []string{"not JSON", "invalid character ','", "line 4, column 21"}},
		"a value that is no JSON":      {"{\n  \"profiles\": x\n}", []string{"not JSON", "invalid character 'x'", "byte offset 17, line 2, column 15"}},
		"a file that ends too soon":    {"{\"profiles\": [\n", []string{"ends inside its JSON object", "byte offset 16, line 2, column 1"}},
		"a field of the wrong type":    {profilesFile(githubRead, githubWrite(`{"accounts": "example-org", "permissions": {"contents": "read"}}`)), []string{"profiles.1.github.accounts", "byte offset"}},
		"an empty file":                {"", []string{"empty"}},
		"a file of white space":        {"\n  \n", []string{"empty", "no JSON object"}},
		"profiles that is no list":     {`{"profiles": {"name": "GITHUB_READ"}}`, []string{"profiles", "byte offset 14, line 1, column 14"}},
		"a kind that is not a string":  {profilesFile(adminPAT(`{"hosts": ["api.github.com"], "kind": 3}`)), []string{"profiles.0.proxy.kind"}},
		"a file that is null":          {"null\n", []string{"the file is null", "no JSON object"}},
		"a file that is a list":        {`[{"name": "GITHUB_READ"}]`, []string{"the file is a JSON array", "not one JSON object", "line 1, column 1"}},
		"a file that is a string":      {`"profiles"`, []string{"the file is a JSON string", "not one JSON object"}},
		"profiles that is null":        {`{"profiles": null}`, []string{`"profiles" is missing or null`, `{"profiles": []}`}},
		"a file with no profiles list": {`{}`, []string{`"profiles" is missing or null`, `{"profiles": []}`}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := policy.ParseProfiles([]byte(c.file))
			if err == nil {
				t.Fatalf("ParseProfiles served a file with %s", name)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
	t.Run("the control: white space after the object, and a file of no profiles", func(t *testing.T) {
		for _, file := range []string{profilesFile(githubRead) + "\n\n", `{"profiles": []}`, " \n{\"profiles\": []}"} {
			if _, err := policy.ParseProfiles([]byte(file)); err != nil {
				t.Errorf("ParseProfiles(%q): %v", file, err)
			}
		}
	})
}

// TestParseProfilesReadsEachFieldAsTheBrokerServesIt: a profile is answered in the one form the
// broker serves it in. Its source is its own name when the file names none, and its value
// installations; its accounts are GitHub logins, lowercased, as GitHub compares them, and sorted;
// its repositories are account/name with that account lowercased, sorted and each once; its
// requesters and scopes are sorted, as the tags they stand beside are. A proxy string is its
// ProxyKind, a dpop block its Proxy, and its allow is kept as written, an absent one apart from a
// present empty one, which allows nothing. Lookup answers the one profile the file holds by that
// name, the same each time, and nil for a name it does not hold. The file's kinds are Kinds, in
// the file's order, each list as the file writes it.
func TestParseProfilesReadsEachFieldAsTheBrokerServesIt(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(
		`{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon", "ada@example.com"], "maxGrant": "1h30m",
		  "github": {"accounts": ["Example-Org", "ada"], "repositories": ["example-org/widgets", "Example-Org/legion", "ada/proofs", "example-org/widgets"], "permissions": {"contents": "write", "metadata": "read", "workflows": "write"}}}`,
		`{"name": "GOOGLE_READ", "google": {"scopes": ["devstorage.read_only", "cloud-platform"]}, "proxy": "gcp"}`,
		`{"name": "SLACK_NONE", "source": "SLACK_TOKEN", "proxy": "slack", "allow": []}`,
		`{"name": "SLACK_POST", "source": "SLACK_TOKEN", "allow": [{"method": "POST", "pathPrefix": "/api/chat.postMessage", "host": "slack.com"}]}`,
		`{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	wantWrite := &policy.Profile{
		Name: "GITHUB_WRITE", Source: "LEGION_IMPLEMENT_APP", Tier: policy.TierAgent,
		Requesters: []string{"ada@example.com", "legion-daemon"}, MaxGrant: 90 * time.Minute,
		GitHub: &policy.GitHubBlock{
			Accounts:     []string{"ada", "example-org"},
			Repositories: []string{"ada/proofs", "example-org/legion", "example-org/widgets"},
			Permissions:  map[string]string{"contents": "write", "metadata": "read", "workflows": "write"},
			Value:        githubname.ValueInstallations,
		},
	}
	if got := profiles.Lookup("GITHUB_WRITE"); !reflect.DeepEqual(got, wantWrite) {
		t.Errorf("Lookup(GITHUB_WRITE) = %+v, want %+v", got, wantWrite)
	}
	for name, want := range map[string]*policy.Profile{
		"GOOGLE_READ": {Name: "GOOGLE_READ", Source: "GOOGLE_READ", Google: &policy.GoogleBlock{Scopes: []string{"cloud-platform", "devstorage.read_only"}}, ProxyKind: "gcp"},
		"SLACK_NONE":  {Name: "SLACK_NONE", Source: "SLACK_TOKEN", ProxyKind: "slack", Allow: []proxyrules.Allow{}},
		"SLACK_POST":  {Name: "SLACK_POST", Source: "SLACK_TOKEN", Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/chat.postMessage", Host: "slack.com"}}},
		"TEST_MINTED_TOKEN": {Name: "TEST_MINTED_TOKEN", Source: "TEST_MINTED_TOKEN", Proxy: &policy.ProxyBlock{
			Hosts: []string{"dispatch.example.com"}, Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/"}}, Kind: proxyrules.WayDPoP,
		}},
	} {
		if got := profiles.Lookup(name); !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%s) = %+v, want %+v", name, got, want)
		}
	}
	if none, empty := profiles.Lookup("GOOGLE_READ").Allow, profiles.Lookup("SLACK_NONE").Allow; none != nil || empty == nil {
		t.Errorf("GOOGLE_READ's allow is %#v and SLACK_NONE's %#v; want nil, no allow, and a present empty list, which allows nothing", none, empty)
	}
	if got := profiles.Kinds(); len(got) != 6 || got[0].Name != "example" || got[5].Name != "anthropic" || got[2].Headers == nil || got[0].Headers != nil || len(got[5].Allow) != 3 || got[5].MaxGrant != "1h" {
		t.Errorf("Kinds() = %+v, want policytest.Kinds as the file writes them, in its order", got)
	}
	if profiles.Lookup("GITHUB_WRITE") != profiles.Lookup("GITHUB_WRITE") {
		t.Error("two Lookups of one name answer two profiles; a served profile points into the one parse")
	}
	if got := profiles.Lookup("LEGION_IMPLEMENT_APP"); got != nil {
		t.Errorf("Lookup of a source no profile is named = %+v, want nil", got)
	}
	if got, want := profiles.Names(), []string{"GITHUB_WRITE", "GOOGLE_READ", "SLACK_NONE", "SLACK_POST", "TEST_MINTED_TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v, the file's order", got, want)
	}
	var none policy.Profiles
	if none.Lookup("GITHUB_WRITE") != nil || len(none.Names()) != 0 || none.Digest("GITHUB_WRITE") != "" || len(none.Kinds()) != 0 || len(none.FileRules().Names) != 0 {
		t.Error("the zero Profiles, a deployment with no profiles file, holds a profile")
	}
}

// TestProfilesIndexEachProfilesSource: SourceOf answers the source of the profile of a name, a
// profile that holds its source's name and one that names no source among them, and a name the
// file holds no profile of as itself, with false. Of answers the profiles of other names made from
// a source, in the file's order, and nil for a source only a profile of its own name uses, for a
// profile's name, and for a name no profile names. Where a file DecodeProfiles reads repeats a
// name, SourceOf answers the first, as Lookup and Digest do. The zero Profiles, a deployment with
// no profiles file, holds no profile and is no profile's source.
func TestProfilesIndexEachProfilesSource(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(
		githubRead,
		githubWrite(`{"accounts": ["example-org"], "permissions": {"contents": "write", "metadata": "read"}}`),
		`{"name": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"metadata": "read"}}}`,
		adminPAT(`"github"`),
	)))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, source string
		profile      bool
	}{
		{"GITHUB_READ", "LEGION_IMPLEMENT_APP", true},
		{"GITHUB_WRITE", "LEGION_IMPLEMENT_APP", true},
		{"LEGION_IMPLEMENT_APP", "LEGION_IMPLEMENT_APP", true},
		{"GITHUB_ADMIN_PAT", "GITHUB_ADMIN_PAT", true},
		{"NO_SUCH_KEY", "NO_SUCH_KEY", false},
	} {
		if source, profile := profiles.SourceOf(row.name); source != row.source || profile != row.profile {
			t.Errorf("SourceOf(%s) = %s, %t; want %s, %t", row.name, source, profile, row.source, row.profile)
		}
	}
	for _, row := range []struct {
		source   string
		profiles []string
	}{
		{"LEGION_IMPLEMENT_APP", []string{"GITHUB_READ", "GITHUB_WRITE"}},
		{"GITHUB_ADMIN_PAT", nil},
		{"GITHUB_READ", nil},
		{"NO_SUCH_KEY", nil},
	} {
		if got := profiles.Of(row.source); !reflect.DeepEqual(got, row.profiles) {
			t.Errorf("Of(%s) = %v; want %v", row.source, got, row.profiles)
		}
	}

	repeated, err := policy.DecodeProfiles([]byte(profilesFile(
		`{"name": "TWICE_KEY", "source": "FIRST_KEY", "proxy": {"hosts": ["first.example.com"]}}`,
		`{"name": "TWICE_KEY", "source": "SECOND_KEY", "proxy": {"hosts": ["second.example.com"]}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if source, _ := repeated.SourceOf("TWICE_KEY"); source != "FIRST_KEY" || repeated.Lookup("TWICE_KEY").Source != "FIRST_KEY" {
		t.Errorf("SourceOf(TWICE_KEY) = %s and Lookup's source %s; want the first profile's, FIRST_KEY", source, repeated.Lookup("TWICE_KEY").Source)
	}

	var none policy.Profiles
	if source, profile := none.SourceOf("GITHUB_READ"); source != "GITHUB_READ" || profile || none.Of("LEGION_IMPLEMENT_APP") != nil {
		t.Error("the zero Profiles, a deployment with no profiles file, holds a profile or a source")
	}
}

// TestCheckRequestersRefusesAServiceTheDeploymentDoesNotRegister: the file's own check reads a
// requester's shape alone (it runs where no services setting is), and the boot then reads each
// service entry against the deployment's registered services, as the loader reads a requesters
// tag; a person is no service and needs no registration.
func TestCheckRequestersRefusesAServiceTheDeploymentDoesNotRegister(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(githubRead,
		`{"name": "GITHUB_ADMIN_PAT", "requesters": ["ada@example.com", "legion-daemon"], "proxy": "github"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.CheckRequesters([]string{"legion-daemon"}); err != nil {
		t.Errorf("CheckRequesters with legion-daemon registered: %v", err)
	}
	err = profiles.CheckRequesters([]string{"other-service"})
	if err == nil {
		t.Fatal("CheckRequesters served a requester no registered service is")
	}
	for _, want := range []string{`profiles[1] "GITHUB_ADMIN_PAT"`, "requesters", `"legion-daemon"`, "no registered service"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	people, err := policy.ParseProfiles([]byte(profilesFile(`{"name": "GITHUB_ADMIN_PAT", "requesters": ["ada@example.com"], "proxy": "github"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := people.CheckRequesters(nil); err != nil {
		t.Errorf("CheckRequesters of a profile naming a person alone, with no services: %v", err)
	}
}

// TestProfilesDigestMovesWithEachFieldOfTheEntry: a profile's digest moves with every field of
// its entry, each of which decides what a live grant of it may do: the kind its proxy string names
// and its allow among them, an absent allow, a present empty one and one of a row each apart. A
// name the file does not hold has no digest, as a deployment with no profiles file gives every
// name none.
func TestProfilesDigestMovesWithEachFieldOfTheEntry(t *testing.T) {
	digest := func(t *testing.T, name, profile string) string {
		t.Helper()
		profiles, err := policy.ParseProfiles([]byte(profilesFile(githubRead, profile)))
		if err != nil {
			t.Fatal(err)
		}
		return profiles.Digest(name)
	}
	const pat = `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": "github"}`
	plain := digest(t, "GITHUB_ADMIN_PAT", pat)
	if len(plain) != 64 || strings.Trim(plain, "0123456789abcdef") != "" {
		t.Fatalf("Digest = %q, want SHA-256 in hex", plain)
	}
	if other := digest(t, "GITHUB_READ", pat); other == plain || other == "" {
		t.Errorf("GITHUB_READ's digest = %q beside GITHUB_ADMIN_PAT's %q; want one of its own", other, plain)
	}
	if absent := digest(t, "GITHUB_WRITE", pat); absent != "" {
		t.Errorf("the digest of a name no profile holds = %q, want none", absent)
	}
	edits := map[string]string{
		"another kind":           `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": "slack"}`,
		"nothing allowed":        `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": "github", "allow": []}`,
		"an allow row":           `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": "github", "allow": [{"method": "GET", "pathPrefix": "/"}]}`,
		"an allow row on a host": `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": "github", "allow": [{"method": "GET", "pathPrefix": "/", "host": "api.github.com"}]}`,
		"the source's tier":      `{"name": "GITHUB_ADMIN_PAT", "proxy": "github"}`,
		"requesters named":       `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "requesters": ["ada@example.com"], "proxy": "github"}`,
		"a maxGrant":             `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "maxGrant": "1h", "proxy": "github"}`,
		"another source":         `{"name": "GITHUB_ADMIN_PAT", "source": "OTHER_PAT", "tier": "human", "proxy": "github"}`,
		"no proxy string":        `{"name": "GITHUB_ADMIN_PAT", "tier": "human"}`,
	}
	seen := map[string]string{plain: "the plain entry"}
	for name, edited := range edits {
		moved := digest(t, "GITHUB_ADMIN_PAT", edited)
		if was, taken := seen[moved]; taken {
			t.Errorf("%s gave the digest %s, which %s gives too", name, moved, was)
		}
		seen[moved] = name
	}
	const write = `{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"contents": "write", "metadata": "read"}%s}}`
	github := digest(t, "GITHUB_WRITE", fmt.Sprintf(write, ""))
	for name, edit := range map[string]string{
		"a repository named": `, "repositories": ["example-org/legion"]`,
		"the bare token":     `, "value": "token"`,
	} {
		if moved := digest(t, "GITHUB_WRITE", fmt.Sprintf(write, edit)); moved == github {
			t.Errorf("%s left the GitHub profile's digest where it was", name)
		}
	}
	if same := digest(t, "GITHUB_WRITE", fmt.Sprintf(write, `, "value": "installations"`)); same != github {
		t.Error("writing value's default moved the digest, though the profile serves the same token")
	}
	if moved := digest(t, "GITHUB_WRITE", strings.Replace(fmt.Sprintf(write, ""), `"write"`, `"read"`, 1)); moved == github {
		t.Error("a permission's level left the digest where it was")
	}
}

// TestADigestIsTheOneItHadBeforeTheKinds pins the digest of three profiles the kinds leave as they
// were to the values the merge base (commit 233828e4aeb) computed for the
// same entries, printed by its Profiles.Digest: a GitHub profile and a google profile, neither of
// them behind the proxy, and a dpop block, the one block a kinds file still serves. Each request
// records the digest of the profile it was decided under (request_secrets.airlock_digest), and a
// release ends a grant whose profile's digest moved; a minted grant records its entry's digest
// (config.MintedEntryDigest, from the dpop block's Profiles.Digest) and is no longer live once it
// moves. So a digest the kinds moved here would end every such grant in production at the first
// broker of this release. A profile whose proxy block the kinds replace has a digest of its new
// form, which its grants end at once on: its kind is what the move to kinds changes.
func TestADigestIsTheOneItHadBeforeTheKinds(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(
		githubRead,
		`{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "headers": ["Authorization"], "kind": "dpop", "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}]}}`,
		`{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "repositories": ["example-org/legion"], "permissions": {"contents": "write", "metadata": "read"}}}`,
		`{"name": "GOOGLE_READ", "source": "GCS_READER", "requesters": ["ada@example.com"], "maxGrant": "1h", "google": {"scopes": ["devstorage.read_only"]}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"TEST_MINTED_TOKEN": "fec1197a7981d4f713582b9ff60d3129e9fbce01f6db89d05c8957490d83f3ec",
		"GITHUB_WRITE":      "5d1ba4af9bda69953e64e56c42517f3fd8c535012f0d76ac38b301a0063907ed",
		"GOOGLE_READ":       "1ae68cb8b561e4892aced4159e44a8efe7a4b04f16f9f201b2559c205549b333",
	} {
		if got := profiles.Digest(name); got != want {
			t.Errorf("Digest(%s) = %s, want %s, the digest the merge base gave the same entry", name, got, want)
		}
	}
}

// TestFileRulesHoldTheKindsAndABindingPerDPoPBlock: the airlock's rules as the file alone gives
// them, before any tag is read, are the file's kinds, as the file writes them, and one inline
// binding for each dpop block, under its profile's name, in the file's order, its way dpop, every
// field of the block carried as the file writes it and the absent lists kept apart from the empty
// ones. A profile behind a kind by its proxy string has no binding here: its way is its source's
// type's, which the policy load reads (Set.ProxyRules). The rules are valid, and bind the dpop
// block's hosts beside the kinds'.
func TestFileRulesHoldTheKindsAndABindingPerDPoPBlock(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(
		githubRead,
		`{"name": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"contents": "write", "metadata": "read"}}}`,
		`{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "headers": [], "allow": [{"method": "POST", "pathPrefix": "/api/v1/"}], "kind": "dpop"}}`,
		`{"name": "OTHER_MINTED_TOKEN", "proxy": {"hosts": ["approve.example.com"], "kind": "dpop"}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	want := proxyrules.Rules{Kinds: profiles.Kinds(), Names: []proxyrules.Binding{
		{Name: "TEST_MINTED_TOKEN", Way: proxyrules.WayDPoP, Inline: &proxyrules.Kind{Hosts: []string{"dispatch.example.com"}, Headers: []string{}, Allow: []proxyrules.Allow{{Method: "POST", PathPrefix: "/api/v1/"}}, Ways: []string{proxyrules.WayDPoP}}},
		{Name: "OTHER_MINTED_TOKEN", Way: proxyrules.WayDPoP, Inline: &proxyrules.Kind{Hosts: []string{"approve.example.com"}, Ways: []string{proxyrules.WayDPoP}}},
	}}
	rules := profiles.FileRules()
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("FileRules() = %+v, want %+v", rules, want)
	}
	if len(rules.Kinds) != 6 {
		t.Errorf("FileRules() holds %d kinds, want the file's 6", len(rules.Kinds))
	}
	if rules.Lookup("GITHUB_READ") != nil || rules.Lookup("LEGION_IMPLEMENT_APP") != nil {
		t.Error("a profile with no dpop block has a binding in the file's own rules")
	}
	if err := proxyrules.Validate(rules); err != nil {
		t.Errorf("the rules of a file ParseProfiles served are not valid: %v", err)
	}
}

// TestDecodeProfilesReadsAFileTheRulesRefuse: the profiles in force are read for the report of
// what a change does to them, and may be a file a stricter rule of this broker refuses; that file
// is read as it is, with its values in the form ParseProfiles answers. A proxy block of a file
// written before the kinds is read too, whatever its kind, and answered by LegacyProxy, and so is
// one written before a key's judge seconds and guidance became its tags, those two fields left
// out, so the pull request that moves the file to kinds can still report against it; a dpop block
// is the profile's Proxy, as ParseProfiles answers it. A file that is not JSON, holds a field the
// broker does not know, holds a maxGrant that is no duration, or is not the one object
// {"profiles": [...]} has no profiles to compare, and is refused: a null file read as no profiles
// would report every profile of the change as added.
func TestDecodeProfilesReadsAFileTheRulesRefuse(t *testing.T) {
	refused := profilesFile(adminPAT(`{"hosts": ["api.github.com"], "judge": 900, "guidance": "grant it for the release job"}`),
		githubWrite(`{"accounts": ["Example-Org"], "permissions": {"workflows": "read"}}`),
		`{"name": "AZURE_ADMIN", "proxy": {"hosts": ["login.microsoftonline.com"], "kind": "azure"}}`,
		`{"name": "TEST_MINTED_TOKEN", "proxy": {"hosts": ["dispatch.example.com"], "kind": "dpop"}}`,
		`{"name": "SLACK_READ", "source": "SLACK_TOKEN", "proxy": "slack", "allow": []}`)
	if _, err := policy.ParseProfiles([]byte(refused)); err == nil {
		t.Fatal("ParseProfiles served the file this row reads as one the rules refuse")
	}
	profiles, err := policy.DecodeProfiles([]byte(refused))
	if err != nil {
		t.Fatalf("DecodeProfiles: %v", err)
	}
	if pat := profiles.LegacyProxy("GITHUB_ADMIN_PAT"); pat == nil || !reflect.DeepEqual(pat.Hosts, []string{"api.github.com"}) || profiles.Lookup("GITHUB_ADMIN_PAT").Proxy != nil {
		t.Errorf("LegacyProxy(GITHUB_ADMIN_PAT) = %+v, want its proxy block's hosts, and no dpop block", pat)
	}
	if azure := profiles.LegacyProxy("AZURE_ADMIN"); azure == nil || azure.Kind != "azure" {
		t.Errorf("LegacyProxy(AZURE_ADMIN) = %+v, want its azure block", azure)
	}
	if minted := profiles.Lookup("TEST_MINTED_TOKEN"); minted == nil || minted.Proxy == nil || profiles.LegacyProxy("TEST_MINTED_TOKEN") != nil {
		t.Errorf("Lookup(TEST_MINTED_TOKEN) = %+v, want its dpop block as its Proxy, and no legacy block", minted)
	}
	if slack := profiles.Lookup("SLACK_READ"); slack == nil || slack.ProxyKind != "slack" || slack.Allow == nil || profiles.LegacyProxy("SLACK_READ") != nil || len(profiles.Kinds()) != 6 {
		t.Errorf("Lookup(SLACK_READ) = %+v, want the kinds form read as ParseProfiles reads it", slack)
	}
	if write := profiles.Lookup("GITHUB_WRITE"); write == nil || !reflect.DeepEqual(write.GitHub.Accounts, []string{"example-org"}) || write.GitHub.Value != githubname.ValueInstallations {
		t.Errorf("Lookup(GITHUB_WRITE) = %+v, want its account lowercased and value installations", write)
	}
	if parsed, err := policy.ParseProfiles([]byte(profilesFile(githubRead))); err != nil || parsed.LegacyProxy("GITHUB_READ") != nil || parsed.LegacyProxy("NO_SUCH_KEY") != nil {
		t.Errorf("ParseProfiles' answer holds a legacy block, or refused a file of kinds: %v", err)
	}
	for name, file := range map[string]string{
		"not JSON":                 "{\"profiles\": [",
		"an unknown field":         profilesFile(`{"name": "A", "lifetime": 5}`),
		"a maxGrant of no length":  profilesFile(`{"name": "A", "maxGrant": "a day"}`),
		"a file that is null":      "null",
		"profiles that is null":    `{"profiles": null}`,
		"a file that is a list":    `[]`,
		"a proxy that is a number": profilesFile(`{"name": "A", "proxy": 7}`),
	} {
		if _, err := policy.DecodeProfiles([]byte(file)); err == nil {
			t.Errorf("DecodeProfiles read a file with %s", name)
		}
	}
}

// TestParseProfilesRefusesEachMalformedKind holds one row per rule of a kind's shape: each refuses
// the file, naming the kind by its index and name and what of it is refused, so the author of the
// pull request can find it. A kind is held to the grammar a host is written in, to the Lean model's
// validate (proxyrules.Validate: a name of one lowercase word, held once, and a host at least) and
// to the policy load's rules of the fields the decision does not read (proxyrules.CheckKind: its
// ways, a kind row naming no host, a max_grant of a second at least, variables of shell names).
// The control is policytest.Kinds, a file the broker serves.
func TestParseProfilesRefusesEachMalformedKind(t *testing.T) {
	const valid = `{"name": "slack", "hosts": ["slack.com"], "ways": ["swap"]}`
	for _, c := range []struct {
		name string
		kind string
		want []string
	}{
		{"a name repeated", `{"name": "slack", "hosts": ["slack.com"], "ways": ["swap"]}`, []string{`kinds[1] "slack"`, "repeated"}},
		{"a name that is not one lowercase word", `{"name": "Slack2", "hosts": ["slack.com"], "ways": ["swap"]}`, []string{`kinds[1] "Slack2"`, "one lowercase word"}},
		{"no host", `{"name": "kv", "hosts": [], "ways": ["swap"]}`, []string{`kinds[1] "kv"`, "no host"}},
		{"a host with a scheme", `{"name": "kv", "hosts": ["kv.example.com", "https://kv.example.org"], "ways": ["swap"]}`, []string{`kinds[1] "kv"`, "hosts[1]", "holds a scheme"}},
		{"no way", `{"name": "kv", "hosts": ["kv.example.com"], "ways": []}`, []string{`kinds[1] "kv"`, "ways names no way"}},
		{"a way no kind takes", `{"name": "kv", "hosts": ["kv.example.com"], "ways": ["paste"]}`, []string{`kinds[1] "kv"`, `"paste"`}},
		{"a kind row naming a host", `{"name": "kv", "hosts": ["kv.example.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/", "host": "kv.example.com"}]}`, []string{`kinds[1] "kv"`, "names the host"}},
		{"a max_grant that is no duration", `{"name": "kv", "hosts": ["kv.example.com"], "ways": ["swap"], "max_grant": "a day"}`, []string{`kinds[1] "kv"`, "max_grant"}},
		{"a variable that is no shell name", `{"name": "kv", "hosts": ["kv.example.com"], "ways": ["swap"], "variables": ["KV-TOKEN"]}`, []string{`kinds[1] "kv"`, `"KV-TOKEN"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := policy.ParseProfiles([]byte(policytest.KindsFileOf("[" + valid + ", " + c.kind + "]")))
			if err == nil {
				t.Fatalf("ParseProfiles served a file with a kind of %s", c.name)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
	t.Run("the control: the test kinds", func(t *testing.T) {
		if _, err := policy.ParseProfiles([]byte(policytest.KindsFile())); err != nil {
			t.Fatalf("ParseProfiles of the test kinds: %v", err)
		}
	})
}
