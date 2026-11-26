// profilescheck_kinds_test.go holds `profiles check` to the move of a deployment's profiles file
// to kinds, on the proxied profiles of a real deployment's file. The three fixtures under
// testdata/ are that file and its move:
//
//   - profiles-proxy-blocks.json is the deployment repository main's broker/profiles.json at
//     f62fbb2bdbe8 (read 2026-11-25), with its 18 proxied profiles verbatim: the 17 of the move's
//     plan and AWS_ADMIN, which main gained after the plan read it. It leaves out the 16 profiles
//     of persons' own Apps, which no proxy block names, writes owner@example.com for each person's
//     address, and leaves one GitHub account no proxy rule reads out of the accounts lists.
//   - profiles-kinds.json is its move to kinds: the kinds of the spec's table for those profiles'
//     hosts; each profile whose source is its own name loses its proxy block, and
//     GITHUB_ENTERPRISE_ADMIN keeps its five rows as its own allow, each on api.github.com,
//     the three ANTHROPIC_ADMIN_* profiles their three rows under the anthropic kind, and AWS_ADMIN
//     its one row on iamcredentials.googleapis.com; the four profiles made from a source the broker
//     also serves by its own name take the proxy string "github".
//   - secrets-tagged.json is the store after the move's tags, as `gcloud secrets list
//     --format='json(name,annotations)'` prints it: each own-name source tagged with its kind, the
//     five narrowed ones tagged proxy-narrowed=true, and each source of a name behind a kind given
//     a max-grant within that kind's max_grant, which the policy load requires.
//
// The helpers are this file's, and runCheck and profilesFile, which a build of the subcommand from
// before kinds also has, so this file runs against that build and shows what it cannot do.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The fixtures, as the test's own directory names them.
const (
	proxyBlocksFixture = "testdata/profiles-proxy-blocks.json"
	kindsFixture       = "testdata/profiles-kinds.json"
	tagsFixture        = "testdata/secrets-tagged.json"
)

// proxiedOnMain is each name behind the proxy under profiles-proxy-blocks.json, and the kind
// profiles-kinds.json, under secrets-tagged.json's tags, serves it behind.
var proxiedOnMain = map[string]string{
	"GITHUB_READ":               "github",
	"GITHUB_ADMIN_READ":         "github",
	"GITHUB_WRITE":              "github",
	"GITHUB_APP_OWNER_AGENT":    "github",
	"AIRLOCK_PROBE":             "github",
	"AIRLOCK_AWS_PROBE":         "aws",
	"LEGION_SLACK_BOT_TOKEN":    "slack",
	"GITHUB_ADMIN_PAT":          "github",
	"GITHUB_PERSONAL_ADMIN_PAT": "github",
	"TAILSCALE_API_KEY":         `tailscale`, // a raw string: a quoted word after a key name reads as a credential to the push check
	"GCLOUD_OWNER":              "gcp",
	"AZURE_ADMIN":               "azure",
	"GWS_THEOREM_ADMIN_OAUTH":   "gws",
	"GITHUB_ENTERPRISE_ADMIN":   "github",
	"ANTHROPIC_ADMIN_HDO":       "anthropic",
	"ANTHROPIC_ADMIN_HDO_V2":    "anthropic",
	"ANTHROPIC_ADMIN_PBC":       "anthropic",
	"AWS_ADMIN":                 "gcp",
}

// anthropicProfiles are the three profiles of the anthropic kind.
var anthropicProfiles = []string{"ANTHROPIC_ADMIN_HDO", "ANTHROPIC_ADMIN_HDO_V2", "ANTHROPIC_ADMIN_PBC"}

// jsonObject is a JSON object of a fixture as encoding/json decodes it.
type jsonObject = map[string]any

// kindsCopy is profiles-kinds.json with edit applied to it, written to a file of t's own, whose
// path it answers: a changed copy of the moved file, as a pull request would write it.
func kindsCopy(t *testing.T, edit func(file jsonObject)) string {
	t.Helper()
	var file jsonObject
	decodeFixture(t, kindsFixture, &file)
	edit(file)
	return profilesFile(t, string(encodeFixture(t, file)))
}

// tagsCopy is secrets-tagged.json with edit applied to the annotations of each secret, by the
// secret's id (agent-secrets-<slug>), written to a file of t's own, whose path it answers.
func tagsCopy(t *testing.T, edit func(annotations map[string]jsonObject)) string {
	t.Helper()
	var list []jsonObject
	decodeFixture(t, tagsFixture, &list)
	byID := map[string]jsonObject{}
	for _, secret := range list {
		name, _ := secret["name"].(string)
		byID[filepath.Base(name)], _ = secret["annotations"].(jsonObject)
	}
	edit(byID)
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, encodeFixture(t, list), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// decodeFixture decodes the fixture at path into v.
func decodeFixture(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// encodeFixture is v as JSON.
func encodeFixture(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// entryOf is the entry of the file's list (kinds or profiles) of name; a fixture without it is a
// test bug.
func entryOf(t *testing.T, file jsonObject, list, name string) jsonObject {
	t.Helper()
	entries, _ := file[list].([]any)
	for _, e := range entries {
		if entry, _ := e.(jsonObject); entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("the fixture's %s hold no %s", list, name)
	return nil
}

// rows is an allow list of method and path prefix pairs, each pair's host host where it is not "".
func rows(host string, pairs ...string) []any {
	var out []any
	for i := 0; i+1 < len(pairs); i += 2 {
		row := jsonObject{"method": pairs[i], "pathPrefix": pairs[i+1]}
		if host != "" {
			row["host"] = host
		}
		out = append(out, row)
	}
	return out
}

// linesWith is each line of out that starts with prefix, in order.
func linesWith(out, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// enterpriseRows are GITHUB_ENTERPRISE_ADMIN's five rows as the moved file holds them, each on
// api.github.com.
func enterpriseRows(host string) []any {
	const base = "/enterprises/example-enterprise/"
	return rows(host,
		"GET", base+"actions/cache/storage-limit", "PUT", base+"actions/cache/storage-limit",
		"GET", base+"audit-log/stream-key", "GET", base+"audit-log/streams", "POST", base+"audit-log/streams")
}

// TestProfilesCheckKeepsEveryProxiedNameOfTheMoveBehindTheProxy is the move's gate: the moved file
// --against the deployment's file in proxy blocks, with the store's tags after the move, exits 0,
// lists each of the 18 names behind the proxy under the old file as behind its kind under the new
// one, by its source's tag or its proxy string, lists none leaving, and lists no widening,
// since each narrowed profile keeps its rows as its own allow. The hosts the move adds to a name
// are listed, not failed: the github kind's two hosts beside AIRLOCK_PROBE's api.github.com, and
// the gws kind's *.googleapis.com in place of two Google hosts. GITHUB_ENTERPRISE_ADMIN and
// AWS_ADMIN, every row of whose allow names a host, gain none. And it lists each secret's proxy
// and proxy-narrowed tags.
func TestProfilesCheckKeepsEveryProxiedNameOfTheMoveBehindTheProxy(t *testing.T) {
	code, stdout, stderr := runCheck(t, kindsFixture, "--against", proxyBlocksFixture, "--secrets-json", tagsFixture)
	if code != 0 || stderr != "" {
		t.Fatalf("the move = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	for _, want := range []string{
		"profiles check: 18 names behind the proxy under " + proxyBlocksFixture + ", each under " + kindsFixture + ":",
		"profiles check: no name leaves the proxy",
		"widens: none",
		`secret "GITHUB_ENTERPRISE_ADMIN": proxy github`,
		`secret "GITHUB_ENTERPRISE_ADMIN": proxy-narrowed true`,
		`secret "ANTHROPIC_ADMIN_PBC": proxy anthropic`,
	} {
		if !slices.Contains(strings.Split(stdout, "\n"), want) {
			t.Errorf("the move's report has no line %q", want)
		}
	}
	served := linesWith(stdout, "served ")
	if len(served) != len(proxiedOnMain) {
		t.Errorf("the report lists %d served names, want the %d of the old file: %q", len(served), len(proxiedOnMain), served)
	}
	for name, kind := range proxiedOnMain {
		via := "its source "
		if p := entryOfFixture(t, name); p["proxy"] != nil {
			via = "its proxy string"
		}
		line := `served "` + name + `": behind the kind ` + kind + ", by " + via
		if !slices.ContainsFunc(served, func(s string) bool { return strings.HasPrefix(s, line) }) {
			t.Errorf("the report does not list %s behind the kind %s by %s: %q", name, kind, via, served)
		}
	}
	if widens := linesWith(stdout, "widens \""); widens != nil {
		t.Errorf("the move widens %q, want nothing", widens)
	}
	wantHosts := []string{
		`hosts "AIRLOCK_PROBE": github.com added`,
		`hosts "AIRLOCK_PROBE": uploads.github.com added`,
		`hosts "GWS_THEOREM_ADMIN_OAUTH": *.googleapis.com added`,
	}
	if got := linesWith(stdout, "hosts \""); !slices.Equal(got, wantHosts) {
		t.Errorf("the move lists the hosts added %q, want %q", got, wantHosts)
	}
}

// TestProfilesCheckServesTheFileOfProxyBlocks: the deployment's file in proxy blocks, which
// declares no kind, is one the broker serves (policy.ParseProfiles), so the check a pull request
// against it runs, --against its own base copy, exits 0 with nothing on stderr: the file is valid,
// declares no kind, nothing changed, and nothing widens among what it compared, which is every name
// behind a block, since each is behind its block under both files. It writes no UNKNOWN: a file
// that declares no kind has no kind a source's tag could name. The control that the run reads
// blocks at all is the fixture itself, which holds the 18 blocks proxiedOnMain lists.
func TestProfilesCheckServesTheFileOfProxyBlocks(t *testing.T) {
	var file jsonObject
	decodeFixture(t, proxyBlocksFixture, &file)
	blocks := 0
	for _, p := range file["profiles"].([]any) {
		if _, held := p.(jsonObject)["proxy"].(jsonObject); held {
			blocks++
		}
	}
	if blocks != len(proxiedOnMain) {
		t.Fatalf("the fixture holds %d proxy blocks, want the %d names proxiedOnMain lists", blocks, len(proxiedOnMain))
	}
	code, stdout, stderr := runCheck(t, proxyBlocksFixture, "--against", proxyBlocksFixture)
	if code != 0 || stderr != "" {
		t.Fatalf("the file of proxy blocks against itself = exit %d with stderr %q, want exit 0 and nothing on stderr", code, stderr)
	}
	for _, want := range []string{
		fmt.Sprintf("profiles check: %s is valid: %d profiles and 0 kinds", proxyBlocksFixture, len(file["profiles"].([]any))),
		"profiles check: nothing changed",
		"widens: none",
	} {
		if !slices.Contains(strings.Split(stdout, "\n"), want) {
			t.Errorf("the report has no line %q:\n%s", want, stdout)
		}
	}
	for _, absent := range []string{"UNKNOWN", "not compared"} {
		if strings.Contains(stdout, absent) {
			t.Errorf("the report of a file of no kinds writes %q:\n%s", absent, stdout)
		}
	}
}

// entryOfFixture is the profile name of profiles-kinds.json.
func entryOfFixture(t *testing.T, name string) jsonObject {
	t.Helper()
	var file jsonObject
	decodeFixture(t, kindsFixture, &file)
	return entryOf(t, file, "profiles", name)
}

// TestProfilesCheckSaysUnknownOfAKindOnlyTheStoreGives: with no --secrets-json, the moved file
// exits 3: it writes its whole report, one UNKNOWN line for each of its 19 profiles with no proxy
// string, whose kind only its source's tag gives, and none for the four behind their proxy
// string, which it holds to everything the file decides of them; stderr says what was not read.
// Controls: a file whose profiles each name their kind as a string exits 0 with no UNKNOWN line,
// and one of them with a grant cap above its kind's, which the file decides, is refused; and a
// file that declares no kind writes none, since no tag can name a kind of it.
func TestProfilesCheckSaysUnknownOfAKindOnlyTheStoreGives(t *testing.T) {
	code, stdout, stderr := runCheck(t, kindsFixture)
	var file jsonObject
	decodeFixture(t, kindsFixture, &file)
	var want []string
	for _, p := range file["profiles"].([]any) {
		profile := p.(jsonObject)
		if profile["proxy"] != nil {
			continue
		}
		name := profile["name"].(string)
		source, _ := profile["source"].(string)
		if source == "" {
			source = name
		}
		want = append(want, `profiles check: UNKNOWN: profile "`+name+`": its kind is its source `+source+`'s proxy tag, which was not read (no --secrets-json), so it was held to no kind`)
	}
	if len(want) != 19 {
		t.Fatalf("the fixture has %d profiles with no proxy string, and this test reads 19", len(want))
	}
	if got := linesWith(stdout, "profiles check: UNKNOWN: "); !slices.Equal(got, want) {
		t.Errorf("the run with no --secrets-json names UNKNOWN %q, want %q", got, want)
	}
	wantErr := "profiles check: " + kindsFixture + ": UNKNOWN: not read (no --secrets-json): the kind of each of 19 profiles, its source's proxy tag; nothing it read is refused\n"
	if code != exitUnknown || stderr != wantErr || !strings.Contains(stdout, `kind "anthropic": added`) {
		t.Fatalf("the moved file with no --secrets-json = exit %d with stderr %q; want exit %d, the whole report and %q", code, stderr, exitUnknown, wantErr)
	}

	kinds := `[{"name": "github", "hosts": ["api.github.com"], "ways": ["swap", "mint"], "max_grant": "12h"}]`
	named := `{"kinds": ` + kinds + `, "profiles": [
  {"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"metadata": "read"}}, "proxy": "github"},
  {"name": "ENTERPRISE_ADMIN", "proxy": "github", "maxGrant": "1h", "allow": [{"method": "GET", "pathPrefix": "/enterprises/", "host": "api.github.com"}]}
]}`
	if code, stdout, stderr := runCheck(t, profilesFile(t, named)); code != 0 || stderr != "" || strings.Contains(stdout, "UNKNOWN") {
		t.Fatalf("a file whose profiles name their kinds = exit %d, stderr %q, stdout %q; want exit 0 and no UNKNOWN line", code, stderr, stdout)
	}
	capped := strings.Replace(named, `"maxGrant": "1h"`, `"maxGrant": "24h"`, 1)
	if code, _, stderr := runCheck(t, profilesFile(t, capped)); code != 1 || !strings.Contains(stderr, `profile "ENTERPRISE_ADMIN": proxy-max-grant-above-kind: its max-grant 24h0m0s is above the kind github's max_grant 12h0m0s`) {
		t.Fatalf("a profile's maxGrant above its proxy string's kind's = exit %d, stderr %q; want exit 1 naming the load's max-grant refusal", code, stderr)
	}
	noKinds := `{"profiles": [{"name": "GITHUB_READ", "source": "LEGION_IMPLEMENT_APP", "github": {"accounts": ["example-org"], "permissions": {"metadata": "read"}}}]}`
	if code, stdout, stderr := runCheck(t, profilesFile(t, noKinds)); code != 0 || stderr != "" || strings.Contains(stdout, "UNKNOWN") {
		t.Fatalf("a file that declares no kind = exit %d, stderr %q, stdout %q; want exit 0 and no UNKNOWN line", code, stderr, stdout)
	}
}

// TestProfilesCheckListsANameThatLeavesTheProxy: a name behind the proxy under the old file that
// the new file and the store put behind nothing exits 1, listing that name as leaving with why and
// naming it on stderr, and lists the others as served. The rows are the two ways a name leaves: the
// file drops the proxy string of a profile whose source, an App the profile's github block
// scopes at its mint, carries no tag, or drops a proxied profile whose name no stored secret holds.
// A stored token's source that loses its proxy tag does not leave: the load refuses its profile, as
// only the proxy scopes its credential, and so does the check. The control is the move itself, in
// the test above, which lists none leaving.
func TestProfilesCheckListsANameThatLeavesTheProxy(t *testing.T) {
	for _, c := range []struct {
		name, leaves, why string
		file              func(t *testing.T) string
	}{
		{
			"one proxy string removed", "GITHUB_WRITE", "the profile names no kind, and its source LEGION_IMPLEMENT_APP carries no proxy tag",
			func(t *testing.T) string {
				return kindsCopy(t, func(file jsonObject) { delete(entryOf(t, file, "profiles", "GITHUB_WRITE"), "proxy") })
			},
		},
		{
			"a proxied profile removed", "GITHUB_APP_OWNER_AGENT", "no profile holds the name, and --secrets-json lists no secret of it",
			func(t *testing.T) string {
				return kindsCopy(t, func(file jsonObject) {
					file["profiles"] = slices.DeleteFunc(file["profiles"].([]any), func(p any) bool { return p.(jsonObject)["name"] == "GITHUB_APP_OWNER_AGENT" })
				})
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runCheck(t, c.file(t), "--against", proxyBlocksFixture, "--secrets-json", tagsFixture)
			if code != 1 {
				t.Fatalf("%s = exit %d, want 1", c.name, code)
			}
			if want := `served "` + c.leaves + `": leaves the proxy: ` + c.why; !slices.Contains(strings.Split(stdout, "\n"), want) {
				t.Errorf("the report does not list %s leaving: no line %q", c.leaves, want)
			}
			if leaving := linesWith(stdout, "served "); len(leaving) != len(proxiedOnMain) || strings.Count(stdout, ": leaves the proxy: ") != 1 {
				t.Errorf("the report lists %d served names and %d leaving, want %d and 1", len(leaving), strings.Count(stdout, ": leaves the proxy: "), len(proxiedOnMain))
			}
			if !strings.Contains(stderr, "1 name leaving the proxy: "+c.leaves) {
				t.Errorf("stderr does not name %s as leaving the proxy: %q", c.leaves, stderr)
			}
		})
	}
	untagged := tagsCopy(t, func(a map[string]jsonObject) { delete(a["agent-secrets-legion-slack-bot-token"], "proxy") })
	code, stdout, stderr := runCheck(t, kindsFixture, "--against", proxyBlocksFixture, "--secrets-json", untagged)
	if want := `profile "LEGION_SLACK_BOT_TOKEN": profile-source-not-proxied: `; code != 1 || stdout != "" || !strings.Contains(stderr, want) {
		t.Fatalf("a stored token's source that loses its proxy tag = exit %d, stdout %q, stderr %q; want exit 1, no report, and %q", code, stdout, stderr, want)
	}
}

// TestProfilesCheckListsWhatWidensAgainstTheOldFile is --against's widens list on
// GITHUB_ENTERPRISE_ADMIN, a name behind the proxy under both files: a row of its allow under the
// new file that its five rows under the old one do not cover is listed, and the run exits 0, since
// a widening is the reviewer's to read and a gate's to refuse. A prefix that runs on past a row's
// end (storage-limitX) is not covered, since a row matches at a "/" boundary alone; nor is a row
// wider than any old one; nor an allow left out, which allows any method and path. A row removed
// narrows the name and lists nothing, the false positive that stays quiet. A row's host is no
// widening, since an old row of no host covers every host; the rows' hosts left out list the
// github kind's two other hosts as added to it.
func TestProfilesCheckListsWhatWidensAgainstTheOldFile(t *testing.T) {
	const (
		storageLimit = "/enterprises/example-enterprise/actions/cache/storage-limit"
		streamKey    = "/enterprises/example-enterprise/audit-log/stream-key"
	)
	setRows := func(edit func([]any) []any) func(*testing.T) string {
		return func(t *testing.T) string {
			return kindsCopy(t, func(file jsonObject) {
				profile := entryOf(t, file, "profiles", "GITHUB_ENTERPRISE_ADMIN")
				if allow := edit(enterpriseRows("api.github.com")); allow != nil {
					profile["allow"] = allow
				} else {
					delete(profile, "allow")
				}
			})
		}
	}
	notNarrowed := func(t *testing.T) string {
		return tagsCopy(t, func(a map[string]jsonObject) { delete(a["agent-secrets-github-enterprise-admin"], "proxy-narrowed") })
	}
	for _, c := range []struct {
		name   string
		file   func(*testing.T) string
		tags   func(*testing.T) string
		widens []string
		hosts  []string
	}{
		{"a prefix past a row's end", setRows(func(r []any) []any {
			r[0].(jsonObject)["pathPrefix"] = storageLimit + "X"
			return r
		}), nil, []string{`widens "GITHUB_ENTERPRISE_ADMIN": GET ` + storageLimit + "X on api.github.com"}, nil},
		{"a row wider than any old row", setRows(func(r []any) []any {
			r[2].(jsonObject)["pathPrefix"] = "/enterprises/"
			return r
		}), nil, []string{`widens "GITHUB_ENTERPRISE_ADMIN": GET /enterprises/ on api.github.com`}, nil},
		{"the allow left out, served by the github kind's alone", setRows(func([]any) []any { return nil }), notNarrowed,
			[]string{`widens "GITHUB_ENTERPRISE_ADMIN": any method and path`},
			[]string{`hosts "GITHUB_ENTERPRISE_ADMIN": github.com added`, `hosts "GITHUB_ENTERPRISE_ADMIN": uploads.github.com added`}},
		{"one of the five rows removed", setRows(func(r []any) []any { return slices.Delete(r, 2, 3) }), nil, nil, nil},
		{"the five rows' host left out", setRows(func([]any) []any { return enterpriseRows("") }), nil, nil,
			[]string{`hosts "GITHUB_ENTERPRISE_ADMIN": github.com added`, `hosts "GITHUB_ENTERPRISE_ADMIN": uploads.github.com added`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tags := tagsFixture
			if c.tags != nil {
				tags = c.tags(t)
			}
			code, stdout, stderr := runCheck(t, c.file(t), "--against", proxyBlocksFixture, "--secrets-json", tags)
			if code != 0 || stderr != "" {
				t.Fatalf("%s = exit %d with stderr %q, want exit 0: a widening is listed, not refused", c.name, code, stderr)
			}
			if got := linesWith(stdout, `widens "`); !slices.Equal(got, c.widens) {
				t.Errorf("%s widens %q, want %q", c.name, got, c.widens)
			}
			if (c.widens == nil) != slices.Contains(strings.Split(stdout, "\n"), "widens: none") {
				t.Errorf("%s: the report's widens: none line disagrees with its widens %q", c.name, c.widens)
			}
			if got := linesWith(stdout, `hosts "GITHUB_ENTERPRISE_ADMIN"`); !slices.Equal(got, c.hosts) {
				t.Errorf("%s lists the hosts added to GITHUB_ENTERPRISE_ADMIN %q, want %q", c.name, got, c.hosts)
			}
		})
	}
	// Reading what the change does to an allow needs no store: the same widening with no
	// --secrets-json is listed for a name whose kind is its proxy string under both files. The
	// runs exit 3 all the same, since the other profiles' kinds are their sources' tags.
	widened := kindsCopy(t, func(file jsonObject) {
		read := entryOf(t, file, "profiles", "GITHUB_READ")
		read["allow"] = rows("", "GET", "/repos/")
	})
	if code, stdout, _ := runCheck(t, widened, "--against", kindsFixture); code != exitUnknown || linesWith(stdout, `widens "GITHUB_READ"`) != nil {
		t.Fatalf("a narrower allow for GITHUB_READ = exit %d, widens %q; want exit %d and nothing widened", code, linesWith(stdout, `widens "GITHUB_READ"`), exitUnknown)
	}
	if code, stdout, _ := runCheck(t, kindsFixture, "--against", widened); code != exitUnknown || !slices.Equal(linesWith(stdout, `widens "GITHUB_READ"`), []string{`widens "GITHUB_READ": any method and path`}) {
		t.Fatalf("GITHUB_READ's allow left out against it = exit %d, widens %q; want exit %d and GITHUB_READ widened to any method and path", code, linesWith(stdout, `widens "`), exitUnknown)
	}
}

// TestProfilesCheckComparesEachKindBothFilesDeclareWithoutTheTags: with no --secrets-json the
// check cannot name a stored secret behind a kind by its own proxy tag, whose allow and hosts are
// that kind's, so it compares each kind both files declare on its own: the slack kind gains a row
// and a host, listed as `widens kind` and `hosts kind`, and the run exits 3 naming the one kind
// compared so, and not the tailscale kind, which the old file does not declare. Controls: the new
// file against itself lists no kind widening and still exits 3, since the names behind slack are
// unread there too; and with the store's tags, which hold LONE behind slack by its own tag, the
// same change is listed under LONE's name, no kind is compared on its own, and the run exits 0.
func TestProfilesCheckComparesEachKindBothFilesDeclareWithoutTheTags(t *testing.T) {
	old := profilesFile(t, `{"kinds": [{"name": "slack", "hosts": ["slack.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/api/"}]}], "profiles": []}`)
	now := profilesFile(t, `{"kinds": [
  {"name": "slack", "hosts": ["slack.com", "files.slack.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/api/"}, {"method": "POST", "pathPrefix": "/api/chat.postMessage"}]},
  {"name": "tailscale", "hosts": ["api.tailscale.com"], "ways": ["swap"]}
], "profiles": []}`)
	wantWidens := []string{`widens kind "slack": POST /api/chat.postMessage`}
	wantHosts := []string{`hosts kind "slack": files.slack.com added`}
	code, stdout, stderr := runCheck(t, now, "--against", old)
	if code != exitUnknown || !strings.Contains(stderr, "the names behind 1 kind by their own proxy tag, compared as their kind alone") {
		t.Fatalf("a widened kind with no --secrets-json = exit %d, stderr %q; want exit %d naming the one kind compared on its own", code, stderr, exitUnknown)
	}
	if got, hosts := linesWith(stdout, "widens kind "), linesWith(stdout, "hosts kind "); !slices.Equal(got, wantWidens) || !slices.Equal(hosts, wantHosts) {
		t.Fatalf("the kinds compared on their own list %q and %q, want %q and %q", got, hosts, wantWidens, wantHosts)
	}
	if !strings.Contains(stdout, "profiles check: the kinds both files declare were compared on their own (1 kind)") {
		t.Errorf("the report does not say how many kinds were compared on their own:\n%s", stdout)
	}

	code, stdout, stderr = runCheck(t, now, "--against", now)
	if code != exitUnknown || slices.Concat(linesWith(stdout, "widens kind "), linesWith(stdout, "hosts kind ")) != nil || !strings.Contains(stdout, "widens: none among what was compared\n") {
		t.Fatalf("the new file against itself = exit %d, stderr %q; want exit %d, no kind widened and the none-among-what-was-compared line:\n%s", code, stderr, exitUnknown, stdout)
	}

	list := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(list, []byte(`[{"name": "projects/1/secrets/agent-secrets-lone", "annotations": {"owner": "ada@example.com", "tier": "agent", "proxy": "slack"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCheck(t, now, "--against", old, "--secrets-json", list)
	if code != 0 || stderr != "" {
		t.Fatalf("the same change with the store's tags = exit %d, stderr %q; want exit 0", code, stderr)
	}
	if got := slices.Concat(linesWith(stdout, "widens "), linesWith(stdout, "hosts ")); !slices.Equal(got, []string{`widens "LONE": POST /api/chat.postMessage`, `hosts "LONE": files.slack.com added`}) {
		t.Fatalf("with the tags the change lists %q, want it under LONE's name and no kind compared on its own", got)
	}
}

// TestProfilesCheckRefusesAProfileRowItsKindDoesNotCover is the last step of the anthropic kind's
// check: a copy of the moved file whose anthropic kind allows GET under /v1/organizations/ alone
// is refused, exit 1 and no report, naming for each of the three Anthropic profiles, in the load's
// words, its rows the kind no longer covers, POST and DELETE under /v1/organizations/, and not its
// GET, which the kind still covers. A profile's kind is its source's proxy tag, which the store
// holds: so the refusal needs --secrets-json, and without it the run names each profile UNKNOWN
// and exits 3, rather than reading as a pass. A profile whose kind is its proxy string is held to
// that kind with no store read, and refused.
func TestProfilesCheckRefusesAProfileRowItsKindDoesNotCover(t *testing.T) {
	getOnly := kindsCopy(t, func(file jsonObject) {
		entryOf(t, file, "kinds", "anthropic")["allow"] = rows("", "GET", "/v1/organizations/")
	})
	code, stdout, stderr := runCheck(t, getOnly, "--secrets-json", tagsFixture)
	if code != 1 || stdout != "" {
		t.Fatalf("the anthropic kind allowing GET alone = exit %d with stdout %q, want exit 1 and no report", code, stdout)
	}
	for _, name := range anthropicProfiles {
		line := linesWith(stderr, "profiles check: "+getOnly+`: profile "`+name+`": `)
		if want := "profile-allow-outside-kind: allow rows POST /v1/organizations/ and DELETE /v1/organizations/ are outside the kind anthropic's allow"; len(line) != 1 || !strings.Contains(line[0], want) {
			t.Errorf("the refusal does not name %s's POST and DELETE rows, and them alone, as outside the anthropic kind: %q", name, line)
		}
	}
	if n := strings.Count(stderr, "\n"); n != len(anthropicProfiles) {
		t.Errorf("the refusal is %d lines, want one for each Anthropic profile: %q", n, stderr)
	}

	// Without the store's tags the run cannot know the profiles' kind, and says so.
	code, stdout, stderr = runCheck(t, getOnly)
	if code != exitUnknown || !strings.Contains(stderr, "UNKNOWN: not read (no --secrets-json)") {
		t.Fatalf("the copy with no --secrets-json = exit %d, stderr %q; want exit %d: it cannot see the kind", code, stderr, exitUnknown)
	}
	for _, name := range anthropicProfiles {
		if want := `profiles check: UNKNOWN: profile "` + name + `": its kind is its source ` + name + `'s proxy tag, which was not read (no --secrets-json), so it was held to no kind`; !strings.Contains(stdout, want+"\n") {
			t.Errorf("the run with no --secrets-json does not name %s UNKNOWN: no line %q", name, want)
		}
	}

	// A profile's own proxy string is in the file, so its rows are held with no store read.
	stringKind := kindsCopy(t, func(file jsonObject) {
		entryOf(t, file, "kinds", "anthropic")["allow"] = rows("", "GET", "/v1/organizations/")
		entryOf(t, file, "profiles", "ANTHROPIC_ADMIN_PBC")["proxy"] = "anthropic"
	})
	if code, _, stderr := runCheck(t, stringKind); code != 1 || !strings.Contains(stderr, `profile "ANTHROPIC_ADMIN_PBC": profile-allow-outside-kind: allow rows POST /v1/organizations/ and DELETE /v1/organizations/ are outside the kind anthropic's allow`) {
		t.Fatalf("a profile behind its proxy string's kind, with rows outside it = exit %d, stderr %q; want exit 1 naming its POST and DELETE rows", code, stderr)
	}
}

// TestProfilesCheckReadsEachChangedCopyOfTheKindsFile runs the copies of the rules test's check
// of the kinds file against the moved file, with the store's tags: a copy with a kind of no host,
// and the two that name a secret in the kinds part, a kind named as a secret and a kind holding a
// field naming one, are refused naming the kind or the field; so are a copy that is not JSON,
// naming the position, and a copy that writes a proxy block the kinds replaced, naming kinds. The
// copies that add a host, a header or a variable to a kind, a row to a kind's allow, or a row
// inside its kind to a profile's allow, exit 0 and list what they change. A copy that removes a
// kind a secret's proxy tag still names is refused, as the load refuses that secret and its
// profile; without the tags it lists the kind removed and exits 3, since it cannot see the tag. The
// control is the moved file against itself, which changes nothing and widens nothing.
func TestProfilesCheckReadsEachChangedCopyOfTheKindsFile(t *testing.T) {
	appendTo := func(list string, name, field string, values ...any) func(*testing.T) string {
		return func(t *testing.T) string {
			return kindsCopy(t, func(file jsonObject) {
				entry := entryOf(t, file, list, name)
				existing, _ := entry[field].([]any)
				entry[field] = append(existing, values...)
			})
		}
	}
	edited := func(edit func(t *testing.T, file jsonObject)) func(*testing.T) string {
		return func(t *testing.T) string { return kindsCopy(t, func(file jsonObject) { edit(t, file) }) }
	}
	removedKind := edited(func(t *testing.T, f jsonObject) {
		f["kinds"] = slices.DeleteFunc(f["kinds"].([]any), func(k any) bool { return k.(jsonObject)["name"] == "tailscale" })
	})
	for _, c := range []struct {
		name  string
		file  func(*testing.T) string
		code  int
		lines []string
	}{
		{"a kind with no host", edited(func(t *testing.T, f jsonObject) { entryOf(t, f, "kinds", "slack")["hosts"] = []any{} }),
			1, []string{`kinds[1] "slack"`, "no host"}},
		{"a kind named as a secret", edited(func(t *testing.T, f jsonObject) { entryOf(t, f, "kinds", "slack")["name"] = "LEGION_SLACK_BOT_TOKEN" }),
			1, []string{`kinds[1] "LEGION_SLACK_BOT_TOKEN"`, "not one lowercase word"}},
		{"a kind with a field naming a secret", edited(func(t *testing.T, f jsonObject) { entryOf(t, f, "kinds", "slack")["secret"] = "LEGION_SLACK_BOT_TOKEN" }),
			1, []string{`unknown field "secret"`, "kinds.1"}},
		{"a proxy block the kinds replaced", edited(func(t *testing.T, f jsonObject) {
			entryOf(t, f, "profiles", "AIRLOCK_PROBE")["proxy"] = jsonObject{"hosts": []any{"api.github.com"}}
		}), 1, []string{`"AIRLOCK_PROBE"`, "proxy block is no longer read", "kinds"}},
		{"a file that is not JSON", func(t *testing.T) string { return profilesFile(t, `{"kinds": [], "profiles": [}`) },
			1, []string{"not JSON", "line 1, column 28"}},
		{"a host added to a kind", appendTo("kinds", "slack", "hosts", "files.slack.com"), 0, []string{`kind "slack": hosts files.slack.com added`}},
		{"a header added to a kind", appendTo("kinds", "slack", "headers", "X-Slack-Token"), 0, []string{`kind "slack": headers X-Slack-Token added`}},
		{"a variable added to a kind", appendTo("kinds", "gcp", "variables", "CLOUDSDK_AUTH_ACCESS_TOKEN"), 0, []string{`kind "gcp": variables CLOUDSDK_AUTH_ACCESS_TOKEN added`}},
		{"a row added to a kind's allow", appendTo("kinds", "anthropic", "allow", rows("", "GET", "/v1/models")...), 0, []string{`kind "anthropic": allow GET /v1/models added`}},
		{"a row inside its kind added to a profile's allow", appendTo("profiles", "ANTHROPIC_ADMIN_HDO", "allow", rows("", "GET", "/v1/organizations/users")...),
			0, []string{`profile "ANTHROPIC_ADMIN_HDO": allow GET /v1/organizations/users added`}},
		{"a kind removed", removedKind, 1, []string{
			`secret "TAILSCALE_API_KEY": proxy-kind-unknown: the proxy tag names "tailscale", which is no kind of the profiles file`,
			`profile "TAILSCALE_API_KEY": profile-source-missing: source TAILSCALE_API_KEY: proxy-kind-unknown (`,
		}},
		{"the control: the moved file itself", func(*testing.T) string { return kindsFixture }, 0, []string{"profiles check: nothing changed", "widens: none"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.file(t)
			code, stdout, stderr := runCheck(t, path, "--against", kindsFixture, "--secrets-json", tagsFixture)
			if code != c.code {
				t.Fatalf("%s = exit %d with stderr %q, want %d", c.name, code, stderr, c.code)
			}
			if c.code != 0 {
				if stdout != "" || !strings.Contains(stderr, path) {
					t.Fatalf("%s: a refusal printed a report, or does not name the file: stdout %q, stderr %q", c.name, stdout, stderr)
				}
				for _, want := range c.lines {
					if !strings.Contains(strings.ReplaceAll(stderr, path, ""), want) {
						t.Errorf("%s: the refusal does not say %q: %q", c.name, want, stderr)
					}
				}
				return
			}
			lines := strings.Split(stdout, "\n")
			for _, want := range c.lines {
				if !slices.Contains(lines, want) {
					t.Errorf("%s: the report has no line %q", c.name, want)
				}
			}
			if widens := linesWith(stdout, `widens "`); widens != nil {
				t.Errorf("%s widens %q, want nothing", c.name, widens)
			}
		})
	}
	code, stdout, stderr := runCheck(t, removedKind(t), "--against", kindsFixture)
	if code != exitUnknown || !slices.Contains(strings.Split(stdout, "\n"), `kind "tailscale": removed`) || !strings.Contains(stderr, "UNKNOWN") {
		t.Fatalf("the copy without the tailscale kind and no --secrets-json = exit %d, stderr %q; want exit %d and the kind listed removed", code, stderr, exitUnknown)
	}
}

// TestProfilesCheckRefusesASecretsListThatShowsNoTags: a --secrets-json file that is not there,
// that is not gcloud's list of secrets, that holds an entry with no name or a secret twice, or
// that lists no agent secret, is refused, exit 1 and no report, naming the file: such a list shows
// nothing of the store's tags, and read as a store of untagged secrets it would list every name
// behind a tag as leaving, or, read as a store with no secret, none. The control is the fixture's
// list beside one entry that is no agent secret, which is read, the entry counted as not read.
func TestProfilesCheckRefusesASecretsListThatShowsNoTags(t *testing.T) {
	write := func(t *testing.T, body string) string {
		path := filepath.Join(t.TempDir(), "secrets.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, c := range []struct{ name, body, says string }{
		{"an object, not a list", `{"secrets": []}`, "is not the list of secrets gcloud secrets list prints"},
		{"an entry with no name", `[{"annotations": {"proxy": "github"}}]`, "entry 0 has no name"},
		{"an empty list", `[]`, "lists no agent secret"},
		{"no agent secret", `[{"name": "projects/805240912354/secrets/broker-signin-client-secret", "annotations": {}}]`, "lists no agent secret"},
		{"a secret twice", `[{"name": "projects/1/secrets/agent-secrets-airlock-probe"}, {"name": "projects/1/secrets/agent-secrets-airlock-probe"}]`, "lists the secret projects/1/secrets/agent-secrets-airlock-probe twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := write(t, c.body)
			code, stdout, stderr := runCheck(t, kindsFixture, "--against", proxyBlocksFixture, "--secrets-json", path)
			if code != 1 || stdout != "" || !strings.Contains(stderr, path) || !strings.Contains(stderr, c.says) {
				t.Fatalf("%s = exit %d, stdout %q, stderr %q; want exit 1, no report, and a refusal naming the file and saying %q", c.name, code, stdout, stderr, c.says)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "gone.json")
	if code, stdout, stderr := runCheck(t, kindsFixture, "--secrets-json", missing); code != 1 || stdout != "" || !strings.Contains(stderr, missing) {
		t.Fatalf("--secrets-json a file that is not there = exit %d, stdout %q, stderr %q; want exit 1 naming it", code, stdout, stderr)
	}
	var list []any
	decodeFixture(t, tagsFixture, &list)
	list = append(list, jsonObject{"name": "projects/805240912354/secrets/agent-secrets-kms-key-ring", "annotations": nil})
	list = append(list, jsonObject{"name": "projects/805240912354/secrets/broker-signin-client-secret"})
	path := write(t, string(encodeFixture(t, list)))
	code, stdout, stderr := runCheck(t, kindsFixture, "--against", proxyBlocksFixture, "--secrets-json", path)
	if want := "profiles check: tags read from " + path + ": 18 agent secrets (1 entry of no agent secret not read)\n"; code != 0 || stderr != "" || !strings.Contains(stdout, want) {
		t.Fatalf("the fixture's list beside an entry of no agent secret = exit %d, stderr %q; want exit 0 and the line %q", code, stderr, want)
	}
}
