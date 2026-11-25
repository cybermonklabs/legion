package policy_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// githubRead is a profile the broker serves: a GitHub App's reads on one account, as a bare token
// behind the proxy. It is the first profile of each malformed row's file, so a row's refusal must
// name the second profile, profiles[1], and not the file's first.
const githubRead = `{"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "tier": "agent",
  "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"},
  "proxy": {"hosts": ["api.github.com", "github.com"]}}`

// profilesFile is a profiles file holding the profiles given, in order.
func profilesFile(profiles ...string) string {
	return `{"profiles": [` + strings.Join(profiles, ",\n") + `]}`
}

// githubWrite is the second profile of a malformed row: a GitHub profile whose github block is
// the row's.
func githubWrite(block string) string {
	return `{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "github": ` + block + `}`
}

// adminPAT is the second profile of a malformed row about a proxy block: a stored value behind the
// proxy, whose proxy block is the row's.
func adminPAT(block string) string {
	return `{"name": "GITHUB_ADMIN_PAT", "proxy": ` + block + `}`
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
		{"a profile with no name", `{"source": "LEGION_IMPLEMENT_APP", "proxy": {"hosts": ["api.github.com"]}}`, "name", []string{"required"}},
		{"a name in lower case", `{"name": "github_write", "proxy": {"hosts": ["api.github.com"]}}`, "name", []string{"not an agent secret name"}},
		{"a name written with hyphens", `{"name": "GITHUB-WRITE", "proxy": {"hosts": ["api.github.com"]}}`, "name", []string{"not an agent secret name"}},
		{"a name another profile holds", githubRead, "name", []string{"profiles[0]"}},
		{"a source in lower case", `{"name": "GITHUB_WRITE", "source": "legion-implement-app", "proxy": {"hosts": ["api.github.com"]}}`, "source", []string{"not an agent secret name"}},
		{"a tier that is neither agent nor human", `{"name": "GITHUB_WRITE", "tier": "admin", "proxy": {"hosts": ["api.github.com"]}}`, "tier", []string{`"admin"`}},
		{"requesters present and empty", `{"name": "GITHUB_WRITE", "requesters": [], "proxy": {"hosts": ["api.github.com"]}}`, "requesters", []string{"nobody"}},
		{"a requester in upper case", `{"name": "GITHUB_WRITE", "requesters": ["Ada@Example.com"], "proxy": {"hosts": ["api.github.com"]}}`, "requesters[0]", []string{"Ada@Example.com"}},
		{"two requesters in one entry", `{"name": "GITHUB_WRITE", "requesters": ["ada@example.com", "bob@example.com legion-daemon"], "proxy": {"hosts": ["api.github.com"]}}`, "requesters[1]", []string{"one entry"}},
		{"a requester list with a comma", `{"name": "GITHUB_WRITE", "requesters": ["ada@example.com,bob@example.com"], "proxy": {"hosts": ["api.github.com"]}}`, "requesters[0]", []string{"comma"}},
		{"a maxGrant that is no duration", `{"name": "GITHUB_WRITE", "maxGrant": "a day", "proxy": {"hosts": ["api.github.com"]}}`, "maxGrant", []string{`"a day"`}},
		{"a maxGrant under a second", `{"name": "GITHUB_WRITE", "maxGrant": "500ms", "proxy": {"hosts": ["api.github.com"]}}`, "maxGrant", []string{"1s"}},
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
		{"a google block with no scopes", `{"name": "GOOGLE_READ", "google": {}, "proxy": {"hosts": ["storage.googleapis.com"]}}`, "google.scopes", []string{"required"}},
		{"an empty scopes list", `{"name": "GOOGLE_READ", "google": {"scopes": []}, "proxy": {"hosts": ["storage.googleapis.com"]}}`, "google.scopes", []string{"required"}},
		{"a whole scope URL", `{"name": "GOOGLE_READ", "google": {"scopes": ["https://www.googleapis.com/auth/cloud-platform"]}}`, "google.scopes[0]", []string{"after https://www.googleapis.com/auth/"}},
		{"a scope twice", `{"name": "GOOGLE_READ", "google": {"scopes": ["cloud-platform", "cloud-platform"]}}`, "google.scopes[1]", []string{"twice"}},
		{"github and google together", `{"name": "GITHUB_WRITE", "github": {"accounts": ["example-org"], "permissions": {"contents": "read", "metadata": "read"}}, "google": {"scopes": ["cloud-platform"]}}`, "github and google", []string{"both"}},
		{"a proxy block with no hosts", adminPAT(`{"judge": 300}`), "proxy.hosts", []string{"required"}},
		{"a proxy block with an empty hosts list", adminPAT(`{"hosts": []}`), "proxy.hosts", []string{"required"}},
		{"a proxy host with a scheme", adminPAT(`{"hosts": ["api.github.com", "https://github.com"]}`), "proxy.hosts[1]", []string{"holds a scheme"}},
		{"a judge above the longest lifetime", adminPAT(`{"hosts": ["api.github.com"], "judge": 1800}`), "proxy", []string{"judge 1800", "900"}},
		{"a kind the proxy does not know", adminPAT(`{"hosts": ["api.github.com"], "kind": "gcp"}`), "proxy", []string{`kind "gcp"`}},
		{"guidance longer than the judge reads", adminPAT(`{"hosts": ["api.github.com"], "judge": 600, "guidance": "` + strings.Repeat("g", 2001) + `"}`), "proxy.guidance", []string{"2001", "2000"}},
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
			adminPAT(`{"hosts": ["api.github.com", "*.githubusercontent.com"], "headers": [], "allow": [{"method": "GET", "pathPrefix": "/"}], "kind": "", "judge": 900, "guidance": "`+strings.Repeat("g", 2000)+`"}`),
		)
		if _, err := policy.ParseProfiles([]byte(valid)); err != nil {
			t.Fatalf("ParseProfiles of a file of valid profiles: %v", err)
		}
	})
}

// resultsProxy is the proxy block of the results capability, oauthwire.AirlockResultsName: one
// exact host, Authorization alone, the results routes and kind dpop.
const resultsProxy = `"proxy": {"hosts": ["dispatch.example.test"], "headers": ["Authorization"], "kind": "dpop",
  "allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}, {"method": "POST", "pathPrefix": "/api/v1/issues/"}]}`

// TestParseProfilesHoldsTheResultsProfileToANameAndAProxyBlock: the results capability,
// DISPATCH_RESULTS_TOKEN, is a name and a proxy block of kind dpop and nothing else, since the
// broker mints its credential for a CI run's result grant from no stored secret. A profile of kind
// dpop that sets a source, a tier, requesters, a maxGrant, a github block or a google block is
// refused naming that field, whatever its name; the results capability with no proxy block is
// refused naming proxy. The control is the name and the dpop block alone, which parses into the
// airlock's entry of kind dpop.
func TestParseProfilesHoldsTheResultsProfileToANameAndAProxyBlock(t *testing.T) {
	// results is a profile of the results capability's name: the fields given, then its proxy
	// block.
	results := func(fields string) string {
		return `{"name": "` + oauthwire.AirlockResultsName + `", ` + fields + resultsProxy + `}`
	}
	for _, c := range []struct {
		name, profile, profileName, field string
	}{
		{"a source", results(`"source": "RESULTS_SOURCE", `), oauthwire.AirlockResultsName, "source"},
		{"a tier", results(`"tier": "agent", `), oauthwire.AirlockResultsName, "tier"},
		{"requesters", results(`"requesters": ["legion-daemon"], `), oauthwire.AirlockResultsName, "requesters"},
		{"a maxGrant", results(`"maxGrant": "15m", `), oauthwire.AirlockResultsName, "maxGrant"},
		{"a github block", results(`"github": {"accounts": ["example-org"], "permissions": {"metadata": "read"}}, `), oauthwire.AirlockResultsName, "github"},
		{"a google block", results(`"google": {"scopes": ["cloud-platform"]}, `), oauthwire.AirlockResultsName, "google"},
		{"a source on a dpop block of another name", `{"name": "OTHER_TOKEN", "source": "RESULTS_SOURCE", ` + resultsProxy + `}`, "OTHER_TOKEN", "source"},
		{"no proxy block", `{"name": "` + oauthwire.AirlockResultsName + `"}`, oauthwire.AirlockResultsName, "proxy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := policy.ParseProfiles([]byte(profilesFile(githubRead, c.profile)))
			if err == nil {
				t.Fatalf("ParseProfiles served a file whose profile %s has %s", c.profileName, c.name)
			}
			if want := fmt.Sprintf("profiles[1] %q: %s ", c.profileName, c.field); !strings.HasPrefix(err.Error(), want) {
				t.Errorf("the refusal %q does not open %q: it must name the profile and the field it refuses", err, want)
			}
		})
	}
	t.Run("the control: the name and the dpop block alone", func(t *testing.T) {
		profiles, err := policy.ParseProfiles([]byte(profilesFile(githubRead, results(""))))
		if err != nil {
			t.Fatalf("ParseProfiles of the results capability as a name and a dpop block: %v", err)
		}
		entry := profiles.ProxyRules().Lookup(oauthwire.AirlockResultsName)
		if entry == nil || entry.Kind != proxyrules.KindDPoP || !reflect.DeepEqual(entry.Hosts, []string{"dispatch.example.test"}) {
			t.Fatalf("the airlock's entry %s = %+v; want the profile's block, of kind %s on dispatch.example.test", oauthwire.AirlockResultsName, entry, proxyrules.KindDPoP)
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
// so, {"profiles": []}.
func TestParseProfilesRefusesUnknownFieldsAndASecondValue(t *testing.T) {
	for name, c := range map[string]struct {
		file string
		want []string
	}{
		"a field of the file":          {`{"profiles": [], "secrets": []}`, []string{`unknown field "secrets"`, "byte offset 18, line 1, column 18"}},
		"a field of a profile":         {profilesFile(githubRead, `{"name": "GITHUB_ADMIN_PAT", "lifetime": 5, "proxy": {"hosts": ["api.github.com"]}}`), []string{`unknown field "lifetime"`, "byte offset", "line 4, column 30"}},
		"a field of a github block":    {profilesFile(githubWrite(`{"accounts": ["example-org"], "owner": "example-org", "permissions": {"contents": "read"}}`)), []string{`unknown field "owner"`, "byte offset 114, line 1, column 114"}},
		"a field of a google block":    {profilesFile(`{"name": "GOOGLE_READ", "google": {"scopes": ["cloud-platform"], "audience": "x"}}`), []string{`unknown field "audience"`, "byte offset 80, line 1, column 80"}},
		"a name in a proxy block":      {profilesFile(adminPAT(`{"name": "GITHUB_ADMIN_PAT", "hosts": ["api.github.com"]}`)), []string{`unknown field "name"`, "byte offset 54, line 1, column 54"}},
		"a field of an allow entry":    {profilesFile(adminPAT(`{"hosts": ["api.github.com"], "allow": [{"method": "GET", "path": "/"}]}`)), []string{`unknown field "path"`, "byte offset 111, line 1, column 111"}},
		"a second value":               {profilesFile(githubRead) + "\n" + profilesFile(), []string{"goes on after its one JSON object", "line 4, column 1"}},
		"a stray closing brace":        {profilesFile(githubRead) + "\n}\n", []string{"goes on after its one JSON object", "line 4, column 1"}},
		"a stray closing bracket":      {"{\"profiles\": []}\n  ]", []string{"goes on after its one JSON object", "line 2, column 3"}},
		"a word after the object":      {`{"profiles": []} done`, []string{"goes on after its one JSON object", "byte offset 18, line 1, column 18"}},
		"a file that is not JSON":      {"{\"profiles\": [\n  " + githubRead + ",\n]}", []string{"not JSON", "invalid character ','", "line 4, column 56"}},
		"a value that is no JSON":      {"{\n  \"profiles\": x\n}", []string{"not JSON", "invalid character 'x'", "byte offset 17, line 2, column 15"}},
		"a file that ends too soon":    {"{\"profiles\": [\n", []string{"ends inside its JSON object", "byte offset 16, line 2, column 1"}},
		"a field of the wrong type":    {profilesFile(githubRead, githubWrite(`{"accounts": "example-org", "permissions": {"contents": "read"}}`)), []string{"profiles.1.github.accounts", "byte offset"}},
		"an empty file":                {"", []string{"empty"}},
		"a file of white space":        {"\n  \n", []string{"empty", "no JSON object"}},
		"profiles that is no list":     {`{"profiles": {"name": "GITHUB_READ"}}`, []string{"profiles", "byte offset 14, line 1, column 14"}},
		"a judge that is not a number": {profilesFile(adminPAT(`{"hosts": ["api.github.com"], "judge": "300"}`)), []string{"profiles.0.proxy.judge"}},
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
// requesters and scopes are sorted, as the tags they stand beside are. Lookup answers the one
// profile the file holds by that name, the same each time, and nil for a name it does not hold.
func TestParseProfilesReadsEachFieldAsTheBrokerServesIt(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(
		`{"name": "GITHUB_WRITE", "source": "LEGION_IMPLEMENT_APP", "tier": "agent", "requesters": ["legion-daemon", "ada@example.com"], "maxGrant": "1h30m",
		  "github": {"accounts": ["Example-Org", "ada"], "repositories": ["example-org/widgets", "Example-Org/legion", "ada/proofs", "example-org/widgets"], "permissions": {"contents": "write", "metadata": "read", "workflows": "write"}}}`,
		`{"name": "GOOGLE_READ", "google": {"scopes": ["devstorage.read_only", "cloud-platform"]}, "proxy": {"hosts": ["storage.googleapis.com"]}}`,
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
	wantRead := &policy.Profile{
		Name: "GOOGLE_READ", Source: "GOOGLE_READ",
		Google: &policy.GoogleBlock{Scopes: []string{"cloud-platform", "devstorage.read_only"}},
		Proxy:  &policy.ProxyBlock{Hosts: []string{"storage.googleapis.com"}},
	}
	if got := profiles.Lookup("GOOGLE_READ"); !reflect.DeepEqual(got, wantRead) {
		t.Errorf("Lookup(GOOGLE_READ) = %+v, want %+v", got, wantRead)
	}
	if profiles.Lookup("GITHUB_WRITE") != profiles.Lookup("GITHUB_WRITE") {
		t.Error("two Lookups of one name answer two profiles; a served profile points into the one parse")
	}
	if got := profiles.Lookup("LEGION_IMPLEMENT_APP"); got != nil {
		t.Errorf("Lookup of a source no profile is named = %+v, want nil", got)
	}
	if got, want := profiles.Names(), []string{"GITHUB_WRITE", "GOOGLE_READ"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v, the file's order", got, want)
	}
	var none policy.Profiles
	if none.Lookup("GITHUB_WRITE") != nil || len(none.Names()) != 0 || none.Digest("GITHUB_WRITE") != "" || len(none.ProxyRules().Secrets) != 0 {
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
		adminPAT(`{"hosts": ["api.github.com"]}`),
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
		`{"name": "GITHUB_ADMIN_PAT", "requesters": ["ada@example.com", "legion-daemon"], "proxy": {"hosts": ["api.github.com"]}}`)))
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
	people, err := policy.ParseProfiles([]byte(profilesFile(`{"name": "GITHUB_ADMIN_PAT", "requesters": ["ada@example.com"], "proxy": {"hosts": ["api.github.com"]}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := people.CheckRequesters(nil); err != nil {
		t.Errorf("CheckRequesters of a profile naming a person alone, with no services: %v", err)
	}
}

// TestProfilesDigestLeavesGuidanceOut: a profile's digest moves with every field of its entry
// that decides what a live grant of it may do, and not with its guidance, which tells the judge
// how to decide requests to come: writing guidance ends no grant. A name the file does not hold
// has no digest, as a deployment with no profiles file gives every name none.
func TestProfilesDigestLeavesGuidanceOut(t *testing.T) {
	digest := func(t *testing.T, name, profile string) string {
		t.Helper()
		profiles, err := policy.ParseProfiles([]byte(profilesFile(githubRead, profile)))
		if err != nil {
			t.Fatal(err)
		}
		return profiles.Digest(name)
	}
	const pat = `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "judge": 300%s}}`
	plain := digest(t, "GITHUB_ADMIN_PAT", fmt.Sprintf(pat, ""))
	if len(plain) != 64 || strings.Trim(plain, "0123456789abcdef") != "" {
		t.Fatalf("Digest = %q, want SHA-256 in hex", plain)
	}
	if guided := digest(t, "GITHUB_ADMIN_PAT", fmt.Sprintf(pat, `, "guidance": "grant it for the release job"`)); guided != plain {
		t.Errorf("guidance moved the digest: %s, want %s", guided, plain)
	}
	if other := digest(t, "GITHUB_READ", fmt.Sprintf(pat, "")); other == plain || other == "" {
		t.Errorf("GITHUB_READ's digest = %q beside GITHUB_ADMIN_PAT's %q; want one of its own", other, plain)
	}
	if absent := digest(t, "GITHUB_WRITE", fmt.Sprintf(pat, "")); absent != "" {
		t.Errorf("the digest of a name no profile holds = %q, want none", absent)
	}
	for name, edited := range map[string]string{
		"a host added":        `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com", "github.com"], "judge": 300}}`,
		"a judge longer":      `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "judge": 600}}`,
		"no header":           `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "headers": [], "judge": 300}}`,
		"nothing allowed":     `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "allow": [], "judge": 300}}`,
		"another kind":        `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "kind": "aws", "judge": 300}}`,
		"the source's tier":   `{"name": "GITHUB_ADMIN_PAT", "proxy": {"hosts": ["api.github.com"], "judge": 300}}`,
		"requesters named":    `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "requesters": ["ada@example.com"], "proxy": {"hosts": ["api.github.com"], "judge": 300}}`,
		"a maxGrant":          `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "maxGrant": "1h", "proxy": {"hosts": ["api.github.com"], "judge": 300}}`,
		"another source":      `{"name": "GITHUB_ADMIN_PAT", "source": "OTHER_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "judge": 300}}`,
		"no proxy block":      `{"name": "GITHUB_ADMIN_PAT", "tier": "human"}`,
		"a header of its own": `{"name": "GITHUB_ADMIN_PAT", "tier": "human", "proxy": {"hosts": ["api.github.com"], "headers": ["x-api-key"], "judge": 300}}`,
	} {
		if moved := digest(t, "GITHUB_ADMIN_PAT", edited); moved == plain {
			t.Errorf("%s left the digest where it was: %s", name, moved)
		}
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

// TestProxyRulesHoldOneEntryPerProxyBlock: the airlock's rules are the profiles' proxy blocks,
// one entry for each, under its profile's name and in the file's order, every field carried as
// the file writes it, the guidance the judge reads among them, and the absent lists kept apart
// from the empty ones. A profile with no proxy block has no entry, so its credential is released
// as it is today for a name the rules do not hold.
func TestProxyRulesHoldOneEntryPerProxyBlock(t *testing.T) {
	profiles, err := policy.ParseProfiles([]byte(profilesFile(
		githubRead,
		`{"name": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"contents": "write", "metadata": "read"}}}`,
		`{"name": "AWS_KEY", "proxy": {"hosts": ["*.amazonaws.com"], "headers": [], "allow": [{"method": "GET", "pathPrefix": "/"}], "kind": "aws", "judge": 300, "guidance": "grant it for the backup job"}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	judge := uint32(300)
	want := proxyrules.Rules{Secrets: []proxyrules.SecretRule{
		{Name: "GITHUB_READ", Hosts: []string{"api.github.com", "github.com"}},
		{Name: "AWS_KEY", Hosts: []string{"*.amazonaws.com"}, Headers: []string{}, Allow: []proxyrules.Allow{{Method: "GET", PathPrefix: "/"}},
			Kind: proxyrules.KindAWS, Judge: &judge, Guidance: "grant it for the backup job"},
	}}
	rules := profiles.ProxyRules()
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("ProxyRules() = %+v, want %+v", rules, want)
	}
	if rules.Lookup("LEGION_IMPLEMENT_APP") != nil {
		t.Error("a profile with no proxy block has an entry in the airlock's rules")
	}
	if err := proxyrules.Validate(rules); err != nil {
		t.Errorf("the rules of a file ParseProfiles served are not valid: %v", err)
	}
}

// TestDecodeProfilesReadsAFileTheRulesRefuse: the profiles in force are read for the report of
// what a change does to them, and may be a file a stricter rule of this broker refuses; that file
// is read as it is, with its values in the form ParseProfiles answers. A file that is not JSON,
// holds a field the broker does not know, holds a maxGrant that is no duration, or is not the one
// object {"profiles": [...]} has no profiles to compare, and is refused: a null file read as no
// profiles would report every profile of the change as added.
func TestDecodeProfilesReadsAFileTheRulesRefuse(t *testing.T) {
	refused := profilesFile(adminPAT(`{"hosts": ["api.github.com"], "judge": 1800}`),
		githubWrite(`{"accounts": ["Example-Org"], "permissions": {"workflows": "read"}}`))
	if _, err := policy.ParseProfiles([]byte(refused)); err == nil {
		t.Fatal("ParseProfiles served the file this row reads as one the rules refuse")
	}
	profiles, err := policy.DecodeProfiles([]byte(refused))
	if err != nil {
		t.Fatalf("DecodeProfiles: %v", err)
	}
	if pat := profiles.Lookup("GITHUB_ADMIN_PAT"); pat == nil || pat.Proxy == nil || pat.Proxy.Judge == nil || *pat.Proxy.Judge != 1800 {
		t.Errorf("Lookup(GITHUB_ADMIN_PAT) = %+v, want its proxy block's judge of 1800", pat)
	}
	if write := profiles.Lookup("GITHUB_WRITE"); write == nil || !reflect.DeepEqual(write.GitHub.Accounts, []string{"example-org"}) || write.GitHub.Value != githubname.ValueInstallations {
		t.Errorf("Lookup(GITHUB_WRITE) = %+v, want its account lowercased and value installations", write)
	}
	for name, file := range map[string]string{
		"not JSON":                "{\"profiles\": [",
		"an unknown field":        profilesFile(`{"name": "A", "lifetime": 5}`),
		"a maxGrant of no length": profilesFile(`{"name": "A", "maxGrant": "a day"}`),
		"a file that is null":     "null",
		"profiles that is null":   `{"profiles": null}`,
		"a file that is a list":   `[]`,
	} {
		if _, err := policy.DecodeProfiles([]byte(file)); err == nil {
			t.Errorf("DecodeProfiles read a file with %s", name)
		}
	}
}
