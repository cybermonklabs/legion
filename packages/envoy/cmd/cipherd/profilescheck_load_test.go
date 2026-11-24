// profilescheck_load_test.go holds `profiles check --secrets-json` to the policy load itself: for
// each refusal the load makes of a name behind Cipher's proxy, a store and a profiles file where
// the load makes it, run once through the load and once through the check with the store's tags
// as gcloud lists them, and the check must refuse with the load's reason and detail. So the check
// cannot pass a file the load refuses for a fault of its kinds, its profiles or its proxy tags.
package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// faultKinds is the kinds part of each file the fault rows below write: github takes a stored
// value and a minted token, slack a stored value alone, gcp a minted token alone, capped caps a
// grant at an hour, and narrow allows GET under /v1/ alone.
const faultKinds = `[
  {"name": "github", "hosts": ["api.github.com", "github.com"], "ways": ["swap", "mint"]},
  {"name": "slack", "hosts": ["slack.com"], "ways": ["swap"]},
  {"name": "gcp", "hosts": ["*.googleapis.com"], "ways": ["mint"]},
  {"name": "capped", "hosts": ["capped.example.com"], "ways": ["swap"], "max_grant": "1h"},
  {"name": "narrow", "hosts": ["narrow.example.com"], "ways": ["swap"], "allow": [{"method": "GET", "pathPrefix": "/v1/"}]}
]`

// faultOwner owns every stored secret of the fault rows.
const faultOwner = "ada@example.com"

// stored is the agent-tier stored secret name, holding a value of no type, with tags beside its
// owner and tier.
func stored(name string, tags map[string]string) awstest.LocalSecret {
	s := policytest.Secret(name, faultOwner, policy.TierAgent, "value")
	maps.Copy(s.Tags, tags)
	return s
}

// app is the github-app secret name, which a profile's github block cuts to the account acme.
func app(name string, tags map[string]string) awstest.LocalSecret {
	s := policytest.TypeSecret(name, faultOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	maps.Copy(s.Tags, tags)
	return s
}

// gcloudList is store's secrets as `gcloud secrets list --format='json(name,annotations)'` lists
// them, each under the project's secret name for its id, written to a file of t's own.
func gcloudList(t *testing.T, store []awstest.LocalSecret) string {
	t.Helper()
	type entry struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	}
	list := make([]entry, len(store))
	for i, s := range store {
		list[i] = entry{"projects/1/secrets/agent-secrets-" + strings.TrimPrefix(s.Name, policytest.Prefix), s.Tags}
	}
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProfilesCheckRefusesWhatTheLoadRefusesOfANameBehindTheProxy runs each refusal the policy load
// makes of a name behind Cipher's proxy through the load and through the check. Each row names the
// subject the load refuses (a profile, or a stored secret served by its own name) and the reason;
// the row first holds the load to that refusal, then the check, given the same file and the
// store's tags, to exit 1 naming the subject, the load's reason and the load's detail, and no
// reason but that row's for the subject. The rows of a profile whose source the load does not
// serve are one for each state of that source the store's tags show (Profiles.SourceMissingFault):
// refused, a profile, and absent; the fourth, a source waiting for its value, is
// TestProfilesCheckReadsASourceWaitingForItsValueAsServed's. The control, a narrowed profile within
// its kind, is served by the load and passes the check.
func TestProfilesCheckRefusesWhatTheLoadRefusesOfANameBehindTheProxy(t *testing.T) {
	narrowed := map[string]string{policy.TagProxyNarrowed: policy.ProxyNarrowedTrue}
	const appBlock = `"github": {"accounts": ["acme"], "permissions": {"metadata": "read"}}`
	for _, c := range []struct {
		name     string
		profiles []string
		store    []awstest.LocalSecret
		// profile is the refused profile's name, or "" for a stored secret refused by its own
		// name, secret; reason is the load's reason for it, "" for the control.
		profile, secret, reason string
	}{
		{
			"a proxy string naming another kind than its source's tag",
			[]string{`{"name": "TOKEN_READ", "source": "TOKEN", "proxy": "github"}`},
			[]awstest.LocalSecret{stored("TOKEN", map[string]string{policy.TagProxy: "slack"})},
			"TOKEN_READ", "", policy.ReasonProxyKindConflict,
		},
		{
			"a stored token's profile behind no kind",
			[]string{`{"name": "TOKEN"}`},
			[]awstest.LocalSecret{stored("TOKEN", nil)},
			"TOKEN", "", policy.ReasonProfileSourceNotProxied,
		},
		{
			"an allow behind no kind",
			[]string{`{"name": "APP_READ", "source": "APP", ` + appBlock + `, "allow": [{"method": "GET", "pathPrefix": "/repos/"}]}`},
			[]awstest.LocalSecret{app("APP", nil)},
			"APP_READ", "", policy.ReasonProfileSourceNotProxied,
		},
		{
			"a stored token behind a kind that only mints",
			[]string{`{"name": "TOKEN", "proxy": "gcp"}`},
			[]awstest.LocalSecret{stored("TOKEN", nil)},
			"TOKEN", "", policy.ReasonProxyWay,
		},
		{
			"no max-grant under a kind that caps a grant",
			[]string{`{"name": "TOKEN", "proxy": "capped"}`},
			[]awstest.LocalSecret{stored("TOKEN", nil)},
			"TOKEN", "", policy.ReasonProxyMaxGrant,
		},
		{
			"a profile's maxGrant above its kind's",
			[]string{`{"name": "TOKEN", "proxy": "capped", "maxGrant": "2h"}`},
			[]awstest.LocalSecret{stored("TOKEN", map[string]string{policy.TagMaxGrant: "12h"})},
			"TOKEN", "", policy.ReasonProxyMaxGrant,
		},
		{
			"an allow whose source carries no proxy-narrowed tag",
			[]string{`{"name": "TOKEN", "proxy": "narrow", "allow": [{"method": "GET", "pathPrefix": "/v1/items"}]}`},
			[]awstest.LocalSecret{stored("TOKEN", nil)},
			"TOKEN", "", policy.ReasonProfileAllowNotNarrowed,
		},
		{
			"a row on a host its kind does not bind",
			[]string{`{"name": "TOKEN", "proxy": "github", "allow": [{"method": "GET", "pathPrefix": "/x", "host": "slack.com"}]}`},
			[]awstest.LocalSecret{stored("TOKEN", narrowed)},
			"TOKEN", "", policy.ReasonProfileAllowHost,
		},
		{
			"a row outside its kind's allow",
			[]string{`{"name": "TOKEN", "proxy": "narrow", "allow": [{"method": "POST", "pathPrefix": "/v1/"}]}`},
			[]awstest.LocalSecret{stored("TOKEN", narrowed)},
			"TOKEN", "", policy.ReasonProfileAllowOutsideKind,
		},
		{
			"a proxy tag naming no kind of the file",
			nil,
			[]awstest.LocalSecret{stored("LONE", map[string]string{policy.TagProxy: "nokind"})},
			"", "LONE", policy.ReasonProxyKindUnknown,
		},
		{
			"a proxy tag of a kind whose ways do not take the secret's type",
			nil,
			[]awstest.LocalSecret{stored("LONE", map[string]string{policy.TagProxy: "gcp"})},
			"", "LONE", policy.ReasonProxyWay,
		},
		{
			"a proxy-narrowed tag that is not true",
			nil,
			[]awstest.LocalSecret{stored("LONE", map[string]string{policy.TagProxy: "slack", policy.TagProxyNarrowed: "yes"})},
			"", "LONE", policy.ReasonProxyNarrowedMalformed,
		},
		{
			"a narrowed secret no profile names",
			nil,
			[]awstest.LocalSecret{stored("LONE", map[string]string{policy.TagProxy: "slack", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue})},
			"", "LONE", policy.ReasonProxyNarrowedProfiles,
		},
		{
			"a profile whose source the load refuses",
			[]string{`{"name": "TOKEN_READ", "source": "TOKEN", "proxy": "github"}`},
			[]awstest.LocalSecret{stored("TOKEN", map[string]string{policy.TagProxy: "nokind"})},
			"TOKEN_READ", "", policy.ReasonProfileSourceMissing,
		},
		{
			"a profile whose source is another profile",
			[]string{`{"name": "TOKEN_READ", "source": "TOKEN", "proxy": "github"}`, `{"name": "READ_OF_READ", "source": "TOKEN_READ", "proxy": "github"}`},
			[]awstest.LocalSecret{stored("TOKEN", nil)},
			"READ_OF_READ", "", policy.ReasonProfileSourceMissing,
		},
		{
			"a profile whose source is absent",
			[]string{`{"name": "TOKEN_READ", "source": "GONE", "proxy": "github"}`},
			[]awstest.LocalSecret{stored("OTHER", nil)},
			"TOKEN_READ", "", policy.ReasonProfileSourceMissing,
		},
		{
			"the control: a narrowed profile within its kind",
			[]string{`{"name": "TOKEN", "proxy": "narrow", "allow": [{"method": "GET", "pathPrefix": "/v1/items"}]}`},
			[]awstest.LocalSecret{stored("TOKEN", narrowed)},
			"TOKEN", "", "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := policytest.KindsFileOf(faultKinds, c.profiles...)
			// The load first, so the row is the refusal it names.
			set := policytest.ProfiledCurrent(t, awstest.NewLocal(c.store...), file).Get()
			key, subject := "profile:"+c.profile, `profile "`+c.profile+`"`
			if c.profile == "" {
				key, subject = policytest.ID(c.secret), `secret "`+c.secret+`"`
			}
			refusal, refused := set.Refused[key]
			if c.reason == "" {
				if refused {
					t.Fatalf("the load refuses the control's %s: %+v", subject, refusal)
				}
			} else if !refused || refusal.Reason != c.reason {
				t.Fatalf("the load refuses %s %+v (refused %t); the row means %s", subject, refusal, refused, c.reason)
			}

			code, stdout, stderr := runCheck(t, profilesFile(t, file), "--secrets-json", gcloudList(t, c.store))
			if c.reason == "" {
				if code != 0 || stderr != "" {
					t.Fatalf("the check of the control = exit %d, stderr %q; want exit 0, as the load serves it", code, stderr)
				}
				return
			}
			if code != 1 || stdout != "" {
				t.Fatalf("the check = exit %d with stdout %q; want exit 1 and no report, as the load refuses %s", code, stdout, subject)
			}
			want := subject + ": " + refusal.Reason + ": " + refusal.Detail + "\n"
			if !strings.Contains(stderr, want) {
				t.Fatalf("the check does not refuse %s with the load's reason and detail: no %q in %q", subject, want, stderr)
			}
			var reasons []string
			for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
				if _, rest, ok := strings.Cut(line, ": "+subject+": "); ok {
					reason, _, _ := strings.Cut(rest, ": ")
					reasons = append(reasons, reason)
				}
			}
			if !slices.Equal(reasons, []string{c.reason}) {
				t.Fatalf("the check refuses %s for %q; want %s alone", subject, reasons, c.reason)
			}
		})
	}
}

// TestProfilesCheckReadsASourceWaitingForItsValueAsServed is the fourth state of a source the load
// does not serve, which the check cannot see: a secret `agent-secrets secret create` made before
// its value is waiting for it, and the load refuses a profile of it naming pending-value. The
// store's list, gcloud's name and annotations, holds no version, so the check reads the source by
// its tags as a served one and refuses nothing. This pins that limit, which the README states:
// the check reads the store's tags and not its values. The control is the same source with its
// value, which both the load and the check serve.
func TestProfilesCheckReadsASourceWaitingForItsValueAsServed(t *testing.T) {
	file := policytest.KindsFileOf(faultKinds, `{"name": "TOKEN_READ", "source": "TOKEN", "proxy": "github"}`)
	created := time.Now()
	waiting := stored("TOKEN", map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"})
	waiting.Value, waiting.CreatedAt = "", &created
	for _, c := range []struct {
		name   string
		source awstest.LocalSecret
		detail string
	}{
		{"waiting for its value", waiting, "source TOKEN: " + policy.ReasonPendingValue},
		{"the control: with its value", stored("TOKEN", nil), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			set := policytest.ProfiledCurrent(t, awstest.NewLocal(c.source), file).Get()
			refusal, refused := set.Refused["profile:TOKEN_READ"]
			if refused != (c.detail != "") || refusal.Detail != c.detail {
				t.Fatalf("the load refuses TOKEN_READ %+v (refused %t); want detail %q", refusal, refused, c.detail)
			}
			if code, _, stderr := runCheck(t, profilesFile(t, file), "--secrets-json", gcloudList(t, []awstest.LocalSecret{c.source})); code != 0 || stderr != "" {
				t.Fatalf("the check = exit %d, stderr %q; want exit 0, since the list shows no value state", code, stderr)
			}
		})
	}
}
