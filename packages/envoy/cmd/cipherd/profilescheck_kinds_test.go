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
//     also serves by its own name take SC13's proxy string, "github".
//   - secrets-tagged.json is the store after the move's tags, as `gcloud secrets list
//     --format='json(name,annotations)'` prints it: each own-name source tagged with its kind, and
//     the five narrowed ones tagged proxy-narrowed=true.
//
// The helpers are this file's, and runCheck and profilesFile, which a build of the subcommand from
// before kinds also has, so this file runs against that build and shows what it cannot do.
package main

import (
	"encoding/json"
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
// one, by its source's tag or SC13's proxy string, lists none leaving, and lists no widening,
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

// entryOfFixture is the profile name of profiles-kinds.json.
func entryOfFixture(t *testing.T, name string) jsonObject {
	t.Helper()
	var file jsonObject
	decodeFixture(t, kindsFixture, &file)
	return entryOf(t, file, "profiles", name)
}

// TestProfilesCheckListsANameThatLeavesTheProxy: a name behind the proxy under the old file that
// the new file and the store put behind nothing exits 1, listing that name as leaving with why and
// naming it on stderr, and lists the others as served. The rows are the two ways a name leaves:
// the file drops SC13's proxy string of a profile whose source carries no tag, or the store loses
// the proxy tag of a source served by its own name. The control is the move itself, in the test
// above, which lists none leaving.
func TestProfilesCheckListsANameThatLeavesTheProxy(t *testing.T) {
	for _, c := range []struct {
		name, leaves, why string
		file, tags        func(t *testing.T) string
	}{
		{
			"one SC13 proxy string removed", "GITHUB_WRITE", "the profile names no kind, and its source LEGION_IMPLEMENT_APP carries no proxy tag",
			func(t *testing.T) string {
				return kindsCopy(t, func(file jsonObject) { delete(entryOf(t, file, "profiles", "GITHUB_WRITE"), "proxy") })
			},
			func(*testing.T) string { return tagsFixture },
		},
		{
			"one source's proxy tag removed", "LEGION_SLACK_BOT_TOKEN", "the profile names no kind, and its source LEGION_SLACK_BOT_TOKEN carries no proxy tag",
			func(*testing.T) string { return kindsFixture },
			func(t *testing.T) string {
				return tagsCopy(t, func(a map[string]jsonObject) { delete(a["agent-secrets-legion-slack-bot-token"], "proxy") })
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runCheck(t, c.file(t), "--against", proxyBlocksFixture, "--secrets-json", c.tags(t))
			if code != 1 {
				t.Fatalf("%s = exit %d, want 1", c.name, code)
			}
			if want := `served "` + c.leaves + `": leaves the proxy: ` + c.why; !slices.Contains(strings.Split(stdout, "\n"), want) {
				t.Errorf("the report does not list %s leaving: no line %q", c.leaves, want)
			}
			if leaving := linesWith(stdout, "served "); len(leaving) != len(proxiedOnMain) || strings.Count(stdout, ": leaves the proxy: ") != 1 {
				t.Errorf("the report lists %d served names and %d leaving, want %d and 1", len(leaving), strings.Count(stdout, ": leaves the proxy: "), len(proxiedOnMain))
			}
			if !strings.Contains(stderr, "1 name leaves the proxy: "+c.leaves) {
				t.Errorf("stderr does not name %s as leaving the proxy: %q", c.leaves, stderr)
			}
		})
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
	// --secrets-json is listed for a name whose kind is SC13's proxy string under both files.
	widened := kindsCopy(t, func(file jsonObject) {
		read := entryOf(t, file, "profiles", "GITHUB_READ")
		read["allow"] = rows("", "GET", "/repos/")
	})
	if code, stdout, _ := runCheck(t, widened, "--against", kindsFixture); code != 0 || linesWith(stdout, `widens "GITHUB_READ"`) != nil {
		t.Fatalf("a narrower allow for GITHUB_READ = exit %d, widens %q; want exit 0 and nothing widened", code, linesWith(stdout, `widens "GITHUB_READ"`))
	}
	if code, stdout, _ := runCheck(t, kindsFixture, "--against", widened); code != 0 || !slices.Equal(linesWith(stdout, `widens "GITHUB_READ"`), []string{`widens "GITHUB_READ": any method and path`}) {
		t.Fatalf("GITHUB_READ's allow left out against it = exit %d, widens %q; want exit 0 and GITHUB_READ widened to any method and path", code, linesWith(stdout, `widens "`))
	}
}

// TestProfilesCheckRefusesAProfileRowItsKindDoesNotCover is the last step of the anthropic kind's
// check: a copy of the moved file whose anthropic kind allows GET under /v1/organizations/ alone
// is refused, exit 1 and no report, naming for each of the three Anthropic profiles its rows the
// kind no longer covers, POST and DELETE under /v1/organizations/, and not its GET, which the kind
// still covers. A profile's kind is its source's proxy tag, which the store holds: so the refusal
// needs --secrets-json, and without it the run says, for each profile, that its rows were not held
// to a kind, rather than reading as a pass. A profile whose kind is its proxy string is held to
// that kind with no store read. The other refusals of a profile's allow the policy load makes,
// which the file and the tags decide, are refused here too: a row whose host its kind does not
// bind, a source without the proxy-narrowed tag, and a proxy string naming another kind than its
// source's tag.
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
		if len(line) != 1 || !strings.Contains(line[0], "allow rows POST /v1/organizations/ and DELETE /v1/organizations/ are outside the kind anthropic's allow") {
			t.Errorf("the refusal does not name %s's POST and DELETE rows, and them alone, as outside the anthropic kind: %q", name, line)
		}
	}
	if n := strings.Count(stderr, "\n"); n != len(anthropicProfiles) {
		t.Errorf("the refusal is %d lines, want one for each Anthropic profile: %q", n, stderr)
	}

	// Without the store's tags the run cannot know the profiles' kind, and says so.
	code, stdout, _ = runCheck(t, getOnly)
	if code != 0 {
		t.Fatalf("the copy with no --secrets-json = exit %d, want 0: it cannot see the kind", code)
	}
	for _, name := range anthropicProfiles {
		if want := `profiles check: profile "` + name + `" has an allow, and its kind is its source ` + name + `'s proxy tag, which only --secrets-json reads: its rows were not held to that kind's allow and hosts, nor ` + name + ` to the proxy-narrowed tag`; !strings.Contains(stdout, want+"\n") {
			t.Errorf("the run with no --secrets-json does not say %s's rows were not held to a kind: no line %q", name, want)
		}
	}

	// A profile's own proxy string is in the file, so its rows are held with no store read.
	stringKind := kindsCopy(t, func(file jsonObject) {
		entryOf(t, file, "kinds", "anthropic")["allow"] = rows("", "GET", "/v1/organizations/")
		entryOf(t, file, "profiles", "ANTHROPIC_ADMIN_PBC")["proxy"] = "anthropic"
	})
	if code, _, stderr := runCheck(t, stringKind); code != 1 || !strings.Contains(stderr, `profile "ANTHROPIC_ADMIN_PBC": allow rows POST /v1/organizations/ and DELETE /v1/organizations/ are outside the kind anthropic's allow`) {
		t.Fatalf("a profile behind its proxy string's kind, with rows outside it = exit %d, stderr %q; want exit 1 naming its POST and DELETE rows", code, stderr)
	}

	for _, c := range []struct {
		name string
		file func(*testing.T) string
		tags func(*testing.T) string
		says string
	}{
		{"a row on a host its kind does not bind", func(t *testing.T) string {
			return kindsCopy(t, func(file jsonObject) {
				entryOf(t, file, "profiles", "AWS_ADMIN")["allow"].([]any)[0].(jsonObject)["host"] = "sts.amazonaws.com"
			})
		}, nil, `profile "AWS_ADMIN": allow row POST /v1/projects/-/serviceAccounts/aws-admin@theorem-aws-admin.iam.gserviceaccount.com:generateIdToken on sts.amazonaws.com names the host sts.amazonaws.com, which the kind gcp does not bind (*.googleapis.com, *.pkg.dev)`},
		{"a narrowing source without the proxy-narrowed tag", func(*testing.T) string { return kindsFixture }, func(t *testing.T) string {
			return tagsCopy(t, func(a map[string]jsonObject) { delete(a["agent-secrets-github-enterprise-admin"], "proxy-narrowed") })
		}, `profile "GITHUB_ENTERPRISE_ADMIN": its allow narrows its source GITHUB_ENTERPRISE_ADMIN at the proxy, and GITHUB_ENTERPRISE_ADMIN carries no proxy-narrowed=true tag`},
		{"a proxy string naming another kind than its source's tag", func(*testing.T) string { return kindsFixture }, func(t *testing.T) string {
			return tagsCopy(t, func(a map[string]jsonObject) { a["agent-secrets-legion-implement-app"]["proxy"] = "slack" })
		}, `profile "GITHUB_READ": its proxy string names the kind github, and its source LEGION_IMPLEMENT_APP's proxy tag names slack`},
	} {
		t.Run(c.name, func(t *testing.T) {
			tags := tagsFixture
			if c.tags != nil {
				tags = c.tags(t)
			}
			code, stdout, stderr := runCheck(t, c.file(t), "--secrets-json", tags)
			if code != 1 || stdout != "" || !strings.Contains(stderr, c.says+"\n") {
				t.Fatalf("%s = exit %d, stdout %q, stderr %q; want exit 1, no report, and a line %q", c.name, code, stdout, stderr, c.says)
			}
		})
	}
}

// TestProfilesCheckReadsEachChangedCopyOfTheKindsFile runs the copies of the rules test's check
// of the kinds file against the moved file: a copy with a kind of no host, and the two that name a
// secret in the kinds part, a kind named as a secret and a kind holding a field naming one, are
// refused naming the kind or the field; so are a copy that is not JSON, naming the position, and a
// copy that writes a proxy block the kinds replaced, naming kinds. The copies that add a host, a
// header or a variable to a kind, a row to a kind's allow, a row inside its kind to a profile's
// allow, or that remove a kind, exit 0 and list what they change. The control is the moved file
// against itself, which changes nothing and widens nothing.
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
		{"a kind removed", edited(func(t *testing.T, f jsonObject) {
			f["kinds"] = slices.DeleteFunc(f["kinds"].([]any), func(k any) bool { return k.(jsonObject)["name"] == "tailscale" })
		}), 0, []string{`kind "tailscale": removed`, `kind "tailscale": hosts api.tailscale.com`}},
		{"the control: the moved file itself", func(*testing.T) string { return kindsFixture }, 0, []string{"profiles check: nothing changed", "widens: none among the names compared"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.file(t)
			code, stdout, stderr := runCheck(t, path, "--against", kindsFixture)
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
