package policy_test

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// agentInfraFile is testdata/deployment-profiles-kinds.json: the deployment repository main's
// broker/profiles.json (read at f62fbb2bdbe8) moved to kinds, as the plan's s-a-kinds moves it.
// Each proxy block's kind is declared under kinds; a profile of its source's own name loses its
// block and keeps its allow, its source tagged with the kind; the four profiles of the two GitHub
// Apps keep SC13's proxy string "github" until their sources are tagged; GITHUB_ENTERPRISE_ADMIN's
// five rows and AWS_ADMIN's one each name the one host the block bound. Every person's address is
// owner@example.com, and the per-person App profiles, which no proxy block names, are left out.
// Its 18 names behind the proxy are the 17 of the plan's read (7aee2d5b119c) and AWS_ADMIN, which
// main gained after it (#6585).
const agentInfraFile = "testdata/deployment-profiles-kinds.json"

// agentInfraOwner is the owner of every secret of the fixture's store, and its requesters.
const agentInfraOwner = "owner@example.com"

// agentInfraSecrets is the store behind agentInfraFile, as s-a-kinds tags it: each source of a
// name behind the proxy carries its kind's proxy tag, the narrowed ones (GITHUB_ENTERPRISE_ADMIN,
// AWS_ADMIN and the three ANTHROPIC_ADMIN_*) proxy-narrowed=true, and each a max-grant within its
// kind's max_grant; the two GitHub Apps whose profiles carry SC13's string carry no proxy tag. The
// three Anthropic admin secrets are anthropic-federation, human tier, admin and an hour at most.
// tagged false leaves every proxy tag off.
func agentInfraSecrets(tagged bool) []secrets.LocalSecret {
	with := func(s secrets.LocalSecret, tags map[string]string) secrets.LocalSecret {
		for k, v := range tags {
			if !tagged && (k == policy.TagProxy || k == policy.TagProxyNarrowed) {
				continue
			}
			s.Tags[k] = v
		}
		return s
	}
	app := func(name, owners string) secrets.LocalSecret {
		return with(policytest.TypeSecret(name, agentInfraOwner, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)),
			map[string]string{policy.TagGitHubOwners: owners, policy.TagMaxGrant: "12h"})
	}
	stored := func(name, kind, maxGrant string) secrets.LocalSecret {
		return with(policytest.Secret(name, agentInfraOwner, policy.TierHuman, name+"-v1"), map[string]string{policy.TagProxy: kind, policy.TagMaxGrant: maxGrant})
	}
	typed := func(name, secretType, kind, maxGrant string) secrets.LocalSecret {
		return with(policytest.TypeSecret(name, agentInfraOwner, policy.TierHuman, secretType, policytest.TypeValue(secretType)), map[string]string{policy.TagProxy: kind, policy.TagMaxGrant: maxGrant})
	}
	admin := func(s secrets.LocalSecret, kind string) secrets.LocalSecret {
		return with(s, map[string]string{policy.TagAdmin: policy.AdminTrue, policy.TagRequesters: agentInfraOwner, policy.TagMaxGrant: "1h", policy.TagProxy: kind, policy.TagProxyNarrowed: policy.ProxyNarrowedTrue})
	}
	return []secrets.LocalSecret{
		app("LEGION_IMPLEMENT_APP", "cybermonklabs sjawhar example-org"),
		app("GITHUB_APP_OWNER", "cybermonklabs sjawhar example-org"),
		app("GITHUB_ADMIN_APP", "example-org"),
		stored("AIRLOCK_PROBE", "github", "12h"),
		stored("AIRLOCK_AWS_PROBE", "aws", "12h"),
		stored("LEGION_SLACK_BOT_TOKEN", "slack", "12h"),
		stored("GITHUB_ADMIN_PAT", "github", "12h"),
		stored("GITHUB_PERSONAL_ADMIN_PAT", "github", "12h"),
		stored("TAILSCALE_API_KEY", "tailscale", "12h"),
		typed("GCLOUD_OWNER", policy.TypeGoogleLogin, "gcp", "24h"),
		typed("AZURE_ADMIN", policy.TypeAzureLogin, "azure", "1h"),
		typed("GWS_THEOREM_ADMIN_OAUTH", policy.TypeGoogleLogin, "gws", "24h"),
		admin(policytest.Secret("GITHUB_ENTERPRISE_ADMIN", agentInfraOwner, policy.TierHuman, "gea-v1"), "github"),
		admin(policytest.TypeSecret("ANTHROPIC_ADMIN_HDO", agentInfraOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)), "anthropic"),
		admin(policytest.TypeSecret("ANTHROPIC_ADMIN_HDO_V2", agentInfraOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)), "anthropic"),
		admin(policytest.TypeSecret("ANTHROPIC_ADMIN_PBC", agentInfraOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation)), "anthropic"),
		admin(policytest.TypeSecret("AWS_ADMIN", agentInfraOwner, policy.TierHuman, policy.TypeGoogleServiceAccount, policytest.TypeValue(policy.TypeGoogleServiceAccount)), "gcp"),
	}
}

// readDeploymentFile is agentInfraFile's text, with each of edits, an old and a new text, replaced
// once; an edit whose old text the file does not hold is a test bug.
func readDeploymentFile(t *testing.T, edits ...[2]string) string {
	t.Helper()
	data, err := os.ReadFile(agentInfraFile)
	if err != nil {
		t.Fatal(err)
	}
	file := string(data)
	for _, edit := range edits {
		if !strings.Contains(file, edit[0]) {
			t.Fatalf("%s holds no %q to edit", agentInfraFile, edit[0])
		}
		file = strings.Replace(file, edit[0], edit[1], 1)
	}
	return file
}

// anthropicRows is the anthropic kind's allow, and the allow of each ANTHROPIC_ADMIN_* profile.
var anthropicRows = []proxyrules.Allow{
	{Method: "GET", PathPrefix: "/v1/organizations/"},
	{Method: "POST", PathPrefix: "/v1/organizations/"},
	{Method: "DELETE", PathPrefix: "/v1/organizations/"},
}

// TestDeploymentMainMovedToKindsServesEveryProxiedName: the deployment repository main's profiles file moved to
// kinds, over a store its sources tagged as s-a-kinds tags them, serves every name it holds and
// refuses nothing; its proxy rules bind each of its 18 names behind the proxy to the kind its
// proxied profile had, with the way its source's type takes and its profile's own allow, and no
// other name; and they are valid. The three ANTHROPIC_ADMIN_* profiles are each served behind the
// anthropic kind of spec v70, by way mint, from an anthropic-federation source that is human tier,
// an administrator's and an hour at most, each keeping its three rows as its own allow. Controls:
// the same file with the anthropic kind's allow cut to GET alone refuses each of the three,
// naming its POST and DELETE rows, outside the kind; and the same store with no proxy tag refuses
// each name only the proxy scopes, and its source by its own name.
func TestDeploymentMainMovedToKindsServesEveryProxiedName(t *testing.T) {
	file := readDeploymentFile(t)
	profiles := policytest.Profiles(t, file, daemonService)
	set := policytest.ProfiledCurrent(t, secrets.NewLocal(agentInfraSecrets(true)...), file, daemonService).Get()
	if len(set.Refused) != 0 {
		t.Fatalf("the moved file refuses %+v; want every name served", set.Refused)
	}
	type bound struct {
		kind, way string
		allow     int
	}
	want := map[string]bound{
		"GITHUB_READ": {"github", "mint", -1}, "GITHUB_ADMIN_READ": {"github", "mint", -1}, "GITHUB_WRITE": {"github", "mint", -1},
		"GITHUB_APP_OWNER_AGENT": {"github", "mint", -1},
		"AIRLOCK_PROBE":          {"github", "swap", -1}, "GITHUB_ADMIN_PAT": {"github", "swap", -1}, "GITHUB_PERSONAL_ADMIN_PAT": {"github", "swap", -1},
		"AIRLOCK_AWS_PROBE": {"aws", "sign", -1}, "LEGION_SLACK_BOT_TOKEN": {"slack", "swap", -1}, "TAILSCALE_API_KEY": {"tailscale", "swap", -1},
		"GCLOUD_OWNER": {"gcp", "mint", -1}, "AZURE_ADMIN": {"azure", "exchange", -1}, "GWS_THEOREM_ADMIN_OAUTH": {"gws", "mint", -1},
		"GITHUB_ENTERPRISE_ADMIN": {"github", "swap", 5},
		"ANTHROPIC_ADMIN_HDO":     {"anthropic", "mint", 3}, "ANTHROPIC_ADMIN_HDO_V2": {"anthropic", "mint", 3}, "ANTHROPIC_ADMIN_PBC": {"anthropic", "mint", 3},
		"AWS_ADMIN": {"gcp", "mint", 1},
	}
	rules := set.ProxyRules()
	if !reflect.DeepEqual(rules.Kinds, profiles.Kinds()) || len(rules.Kinds) != 8 {
		t.Errorf("ProxyRules().Kinds = %+v; want the file's 8 kinds", rules.Kinds)
	}
	if len(rules.Names) != len(want) {
		t.Errorf("ProxyRules() binds %d names, want %d: %+v", len(rules.Names), len(want), rules.Names)
	}
	for name, w := range want {
		b := rules.Lookup(name)
		if b == nil {
			t.Errorf("%s is bound to no kind; want %s", name, w.kind)
			continue
		}
		allow := len(b.Allow)
		if b.Allow == nil {
			allow = -1
		}
		if b.Kind != w.kind || b.Way != w.way || allow != w.allow || b.Inline != nil {
			t.Errorf("%s is bound %+v; want kind %s, way %s and %d rows of its own (-1: none)", name, *b, w.kind, w.way, w.allow)
		}
		if served := set.Secrets[name]; served.Proxy != w.kind || served.Way != w.way || served.Profile == nil {
			t.Errorf("%s is served as %+v; want its profile behind %s by way %s", name, served, w.kind, w.way)
		}
	}
	for _, name := range []string{"LEGION_IMPLEMENT_APP", "LEGION_IMPLEMENT_APP_READ", "GITHUB_APP_OWNER", "GITHUB_APP_OWNER_READ", "GITHUB_ADMIN_APP"} {
		if served := servedProfile(t, set, name); served.Proxy != "" || rules.Lookup(name) != nil {
			t.Errorf("%s is served behind %q, bound %+v; want it behind no kind, as on main", name, served.Proxy, rules.Lookup(name))
		}
	}
	if err := proxyrules.Validate(rules); err != nil {
		t.Errorf("the moved file's rules are not valid: %v", err)
	}
	for _, name := range []string{"ANTHROPIC_ADMIN_HDO", "ANTHROPIC_ADMIN_HDO_V2", "ANTHROPIC_ADMIN_PBC"} {
		served := set.Secrets[name]
		if served.Type() != policy.TypeAnthropicFederation || served.Tier != policy.TierHuman || !served.Admin || served.MaxGrant != time.Hour || !served.ProxyNarrowed || !reflect.DeepEqual(served.ProxyAllow(), anthropicRows) {
			t.Errorf("%s = %+v with allow %v; want an anthropic-federation administrator's, human tier, an hour, narrowed by the three rows", name, served, served.ProxyAllow())
		}
	}
	if hosts := rules.AllHosts(); !slices.Contains(hosts, "api.anthropic.com") || !slices.Contains(hosts, "sts.amazonaws.com") {
		t.Errorf("AllHosts() = %v; want the kinds' hosts", hosts)
	}

	t.Run("the control: the anthropic kind allows GET alone", func(t *testing.T) {
		getOnly := readDeploymentFile(t, [2]string{`      "max_grant": "1h",
      "allow": [
        { "method": "GET", "pathPrefix": "/v1/organizations/" },
        { "method": "POST", "pathPrefix": "/v1/organizations/" },
        { "method": "DELETE", "pathPrefix": "/v1/organizations/" }
      ]`, `      "max_grant": "1h",
      "allow": [{ "method": "GET", "pathPrefix": "/v1/organizations/" }]`})
		set := policytest.ProfiledCurrent(t, secrets.NewLocal(agentInfraSecrets(true)...), getOnly, daemonService).Get()
		for _, name := range []string{"ANTHROPIC_ADMIN_HDO", "ANTHROPIC_ADMIN_HDO_V2", "ANTHROPIC_ADMIN_PBC"} {
			refusal := refusedProfile(t, set, name, policy.ReasonProfileAllowOutsideKind)
			if !strings.Contains(refusal.Detail, "POST /v1/organizations/ and DELETE /v1/organizations/") || strings.Contains(refusal.Detail, "GET") {
				t.Errorf("%s's refusal %q does not name its POST and DELETE rows alone", name, refusal.Detail)
			}
		}
		servedProfile(t, set, "GITHUB_ENTERPRISE_ADMIN")
	})
	t.Run("the control: no source carries a proxy tag", func(t *testing.T) {
		set := policytest.ProfiledCurrent(t, secrets.NewLocal(agentInfraSecrets(false)...), file, daemonService).Get()
		for _, name := range []string{"AIRLOCK_PROBE", "LEGION_SLACK_BOT_TOKEN", "GCLOUD_OWNER", "AZURE_ADMIN", "GITHUB_ENTERPRISE_ADMIN", "ANTHROPIC_ADMIN_HDO", "ANTHROPIC_ADMIN_PBC"} {
			refusedProfile(t, set, name, policy.ReasonProfileSourceNotProxied)
			refusedSecret(t, set, name, policy.ReasonProfileSourceNotProxied)
		}
		if got := servedProfile(t, set, "GITHUB_READ"); got.Proxy != "github" {
			t.Errorf("GITHUB_READ, behind its proxy string, is served behind %q; want github", got.Proxy)
		}
	})
}

// anthropicKinds is agentInfraFile's kinds as a JSON list, for a file of other profiles under the
// same kinds.
func anthropicKinds(t *testing.T) string {
	t.Helper()
	kinds, err := json.Marshal(policytest.Profiles(t, readDeploymentFile(t), daemonService).Kinds())
	if err != nil {
		t.Fatal(err)
	}
	return string(kinds)
}

// TestK47AnAnthropicSecretIsBoundByItsKindAndItsProfile is k47's pair at the policy load and the
// decision it feeds, under the anthropic kind of agentInfraFile, which binds api.anthropic.com
// alone, names Authorization, allows GET, POST and DELETE under /v1/organizations/, takes the way
// mint, names ANTHROPIC_ADMIN_TOKEN and caps a grant at an hour. Secret K, tagged proxy=anthropic
// with no profile and no proxy-narrowed tag, is served by its own name with the kind's allow alone;
// secret N, also proxy-narrowed=true, is served through its one profile, which holds the three rows
// of ANTHROPIC_ADMIN_HDO. For each, the rules the set builds forward (a)'s GET, POST and DELETE under
// /v1/organizations/, and refuse (b) POST /v1/oauth/token, (c) POST /v1/messages and (d) PUT
// /v1/organizations/workspaces/w1, each by the rule. Control: with the kind's allow taken away K's
// (c) is forwarded, so K's refusals come from the kind's allow; N's is still refused, by its
// profile's rule.
func TestK47AnAnthropicSecretIsBoundByItsKindAndItsProfile(t *testing.T) {
	kinds := anthropicKinds(t)
	file := policytest.KindsFileOf(kinds, `{"name": "AIRLOCK_ANTHROPIC_NARROWED", "allow": [
      {"method": "GET", "pathPrefix": "/v1/organizations/"},
      {"method": "POST", "pathPrefix": "/v1/organizations/"},
      {"method": "DELETE", "pathPrefix": "/v1/organizations/"}]}`)
	anthropic := func(name string, narrowed bool) secrets.LocalSecret {
		s := policytest.TypeSecret(name, agentInfraOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation))
		s.Tags[policy.TagAdmin] = policy.AdminTrue
		s = policytest.Proxied(s, "anthropic")
		if narrowed {
			s = policytest.Narrowed(s)
		}
		return s
	}
	set := policytest.ProfiledCurrent(t, secrets.NewLocal(anthropic("AIRLOCK_ANTHROPIC_KIND", false), anthropic("AIRLOCK_ANTHROPIC_NARROWED", true)), file, daemonService).Get()

	kind := set.ProxyRules().Kind("anthropic")
	if kind == nil || !reflect.DeepEqual(kind.Hosts, []string{"api.anthropic.com"}) || !reflect.DeepEqual(kind.HeaderNames(), []string{"Authorization"}) ||
		!reflect.DeepEqual(kind.Allow, anthropicRows) || !reflect.DeepEqual(kind.Ways, []string{"mint"}) ||
		!reflect.DeepEqual(kind.Variables, []string{"ANTHROPIC_ADMIN_TOKEN"}) || kind.MaxGrant != "1h" {
		t.Fatalf("the anthropic kind = %+v; want spec v70's", kind)
	}
	k := servedSecret(t, set, "AIRLOCK_ANTHROPIC_KIND")
	if k.Proxy != "anthropic" || k.Way != proxyrules.WayMint || k.ProxyAllow() != nil {
		t.Errorf("K = %+v; want it behind anthropic, way mint, with no allow of its own", k)
	}
	n := servedProfile(t, set, "AIRLOCK_ANTHROPIC_NARROWED")
	if n.Proxy != "anthropic" || n.Way != proxyrules.WayMint || !n.ProxyNarrowed || !reflect.DeepEqual(n.ProxyAllow(), anthropicRows) {
		t.Errorf("N = %+v; want it behind anthropic, way mint, narrowed by the three rows", n)
	}

	request := func(method, path string) *proxyrules.Request {
		return &proxyrules.Request{ConnectHost: "api.anthropic.com", ConnectPort: 443, SNI: "api.anthropic.com", HostHeader: "api.anthropic.com",
			Method: method, Path: path, RawPath: path, Headers: [][2]string{{"Authorization", "Bearer " + "agent-secrets-placeholder-k47"}}}
	}
	decide := func(rules proxyrules.Rules, name, method, path string) (bool, string) {
		return proxyrules.Decide(rules, proxyrules.Grant{Secret: name, Placeholder: "agent-secrets-placeholder-k47"}, "sk-ant-oat01-stand-in", request(method, path))
	}
	for _, name := range []string{"AIRLOCK_ANTHROPIC_KIND", "AIRLOCK_ANTHROPIC_NARROWED"} {
		for _, r := range [][2]string{{"GET", "/v1/organizations/workspaces"}, {"POST", "/v1/organizations/workspaces"}, {"DELETE", "/v1/organizations/workspaces/w1"}} {
			if ok, why := decide(set.ProxyRules(), name, r[0], r[1]); !ok {
				t.Errorf("%s: %s %s refused %q; want it forwarded", name, r[0], r[1], why)
			}
		}
		for _, r := range [][2]string{{"POST", "/v1/oauth/token"}, {"POST", "/v1/messages"}, {"PUT", "/v1/organizations/workspaces/w1"}} {
			if ok, why := decide(set.ProxyRules(), name, r[0], r[1]); ok || why != "not allowed by rule" {
				t.Errorf("%s: %s %s = %t, %q; want it refused by the kind's rule", name, r[0], r[1], ok, why)
			}
		}
	}

	t.Run("the control: the kind's allow taken away", func(t *testing.T) {
		rules := set.ProxyRules()
		open := proxyrules.Rules{Names: rules.Names}
		for _, k := range rules.Kinds {
			if k.Name == "anthropic" {
				k.Allow = nil
			}
			open.Kinds = append(open.Kinds, k)
		}
		if ok, why := decide(open, "AIRLOCK_ANTHROPIC_KIND", "POST", "/v1/messages"); !ok {
			t.Errorf("K's POST /v1/messages with no kind allow = refused %q; want it forwarded, which is what the kind's allow stops", why)
		}
		if ok, why := decide(open, "AIRLOCK_ANTHROPIC_NARROWED", "POST", "/v1/messages"); ok || why != "not allowed by the profile's rule" {
			t.Errorf("N's POST /v1/messages with no kind allow = %t, %q; want it refused by its profile's rule", ok, why)
		}
	})
}
