package proxyrules

import (
	"encoding/base64"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/fold"
)

// validRules sets every field of a kind, leaves every optional field out of the second kind,
// and gives the third empty lists where the second has none, so the nil defaults are checked
// beside the empty lists. Its bindings name each kind, one with a profile's own allow whose row
// names a host, and one holds an inline rule. The profiles file is what the broker reads rules
// from, and its tests (policy's) hold how a file is read into these kinds and bindings.
var validRules = Rules{
	Kinds: []Kind{
		{
			Name:      "github",
			Hosts:     []string{"api.github.com", "*.githubusercontent.com"},
			Headers:   []string{"authorization", "x-github-token"},
			Variables: []string{"GH_TOKEN", "GITHUB_TOKEN"},
			Ways:      []string{WayMint, WaySwap},
			Allow:     []Allow{{Method: "GET", PathPrefix: "/repos/"}},
			MaxGrant:  "12h",
		},
		{Name: "plain", Hosts: []string{"example.com"}},
		{Name: "sealed", Hosts: []string{"sealed.example.com"}, Headers: []string{}, Allow: []Allow{}},
	},
	Names: []Binding{
		{Name: "GH", Kind: "github", Way: WayMint, Allow: []Allow{{Method: "GET", PathPrefix: "/repos/o/", Host: "api.github.com"}}},
		{Name: "PLAIN", Kind: "plain", Way: WaySwap},
		{Name: "SEALED", Kind: "sealed", Way: WaySwap, Allow: []Allow{}},
		{Name: "MINTED", Way: WayDPoP, Inline: &Kind{Name: "minted", Hosts: []string{"dispatch.test"}, Allow: []Allow{{Method: "POST", PathPrefix: "/api/"}}}},
	},
}

// rulesOf builds a Rules of those kinds and bindings, for the table rows below.
func rulesOf(kinds []Kind, names ...Binding) Rules { return Rules{Kinds: kinds, Names: names} }

func TestValidate(t *testing.T) {
	if err := Validate(validRules); err != nil {
		t.Fatalf("Validate of valid rules: %v", err)
	}
	gh := Kind{Name: "github", Hosts: []string{"api.github.com"}}

	cases := []struct {
		name  string
		rules Rules
		want  []string // every piece the error names
	}{
		{
			name:  "a repeated kind",
			rules: rulesOf([]Kind{gh, {Name: "github", Hosts: []string{"b.example.com"}}}),
			want:  []string{`kinds[1]`, `"github"`, "repeated"},
		},
		{
			name:  "a kind name with a capital",
			rules: rulesOf([]Kind{{Name: "Anthropic", Hosts: []string{"api.anthropic.com"}}}),
			want:  []string{`kinds[0]`, `"Anthropic"`, "not one lowercase word"},
		},
		{
			name:  "a kind name with a hyphen",
			rules: rulesOf([]Kind{{Name: "git-hub", Hosts: []string{"api.github.com"}}}),
			want:  []string{`kinds[0]`, `"git-hub"`, "not one lowercase word"},
		},
		{
			name:  "an empty kind name",
			rules: rulesOf([]Kind{{Name: "", Hosts: []string{"api.github.com"}}}),
			want:  []string{`kinds[0]`, `""`, "not one lowercase word"},
		},
		{
			name:  "a kind name with a digit",
			rules: rulesOf([]Kind{{Name: "github2", Hosts: []string{"api.github.com"}}}),
			want:  []string{`kinds[0]`, `"github2"`, "not one lowercase word"},
		},
		{
			// A kind binds its credential to its hosts: one that names none would hold every
			// value of the kind back and carry it nowhere.
			name:  "a kind with no host",
			rules: rulesOf([]Kind{gh, {Name: "release"}}),
			want:  []string{`kinds[1]`, `"release"`, "names no host"},
		},
		{
			name:  "a kind with an empty host list",
			rules: rulesOf([]Kind{{Name: "a", Hosts: []string{}}}),
			want:  []string{`kinds[0]`, `"a"`, "names no host"},
		},
		{
			name:  "a host written as a URL",
			rules: rulesOf([]Kind{{Name: "slack", Hosts: []string{"https://slack.com"}}}),
			want:  []string{`kinds[0]`, `"slack"`, `"https://slack.com"`, "binds the credential nowhere"},
		},
		{
			name:  "a bare star, which no lookup can ever reach",
			rules: rulesOf([]Kind{{Name: "gcp", Hosts: []string{"*"}}}),
			want:  []string{`kinds[0]`, `"gcp"`, `"*"`, "binds the credential nowhere"},
		},
		{
			name:  "a host with a path",
			rules: rulesOf([]Kind{{Name: "slack", Hosts: []string{"slack.com/"}}}),
			want:  []string{`kinds[0]`, `"slack.com/"`, "binds the credential nowhere"},
		},
		{
			name:  "a host with a port, which the request's name never carries",
			rules: rulesOf([]Kind{{Name: "slack", Hosts: []string{"slack.com:443"}}}),
			want:  []string{`kinds[0]`, `"slack.com:443"`, "binds the credential nowhere"},
		},
		{
			name:  "an empty label",
			rules: rulesOf([]Kind{{Name: "slack", Hosts: []string{".slack.com"}}}),
			want:  []string{`kinds[0]`, `".slack.com"`, "binds the credential nowhere"},
		},
		{
			name:  "one bad host among good ones",
			rules: rulesOf([]Kind{{Name: "gcp", Hosts: []string{"oauth2.googleapis.com", "*"}}}),
			want:  []string{`kinds[0]`, `"*"`, "binds the credential nowhere"},
		},
		{
			name:  "an IPv4 address, which no TLS client names in its SNI",
			rules: rulesOf([]Kind{{Name: "slack", Hosts: []string{"93.184.216.34"}}}),
			want:  []string{`kinds[0]`, `"93.184.216.34"`, "binds the credential nowhere"},
		},
		{
			name:  "a suffix only an address ends in",
			rules: rulesOf([]Kind{{Name: "slack", Hosts: []string{"*.216.34"}}}),
			want:  []string{`kinds[0]`, `"*.216.34"`, "binds the credential nowhere"},
		},
		{
			name:  "a repeated kind wins over the kind's own fault",
			rules: rulesOf([]Kind{gh, {Name: "github"}}),
			want:  []string{`kinds[1]`, "repeated"},
		},
		{
			name:  "the kinds are read before the bindings",
			rules: rulesOf([]Kind{{Name: "Bad", Hosts: []string{"a.example.com"}}}, Binding{Name: "X", Kind: "nowhere"}),
			want:  []string{`kinds[0]`, `"Bad"`},
		},
		{
			name:  "a repeated binding",
			rules: rulesOf([]Kind{gh}, Binding{Name: "GH", Kind: "github"}, Binding{Name: "GH", Kind: "github"}),
			want:  []string{`names[1]`, `"GH"`, "repeated"},
		},
		{
			name:  "a binding naming a kind the rules do not declare",
			rules: rulesOf([]Kind{gh}, Binding{Name: "SLACK", Kind: "slack"}),
			want:  []string{`names[0]`, `"SLACK"`, `the kind "slack" is no kind of the rules`},
		},
		{
			name:  "a binding naming no kind and holding no inline rule",
			rules: rulesOf([]Kind{gh}, Binding{Name: "NONE"}),
			want:  []string{`names[0]`, `"NONE"`, `the kind "" is no kind of the rules`},
		},
		{
			name: "a binding naming a kind and holding an inline rule",
			rules: rulesOf([]Kind{gh}, Binding{Name: "BOTH", Kind: "github",
				Inline: &Kind{Name: "minted", Hosts: []string{"dispatch.test"}}}),
			want: []string{`names[0]`, `"BOTH"`, `names the kind "github" and holds an inline rule`},
		},
		{
			name:  "an inline rule with no host",
			rules: rulesOf(nil, Binding{Name: "MINTED", Inline: &Kind{Name: "minted"}}),
			want:  []string{`names[0]`, `"MINTED"`, "names no host"},
		},
		{
			name:  "an inline rule with a host that binds nowhere",
			rules: rulesOf(nil, Binding{Name: "MINTED", Inline: &Kind{Name: "minted", Hosts: []string{"dispatch.test:443"}}}),
			want:  []string{`names[0]`, `"dispatch.test:443"`, "binds the credential nowhere"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.rules)
			if err == nil {
				t.Fatalf("Validate: no error")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}

	ok := []struct {
		name  string
		rules Rules
	}{
		{"no kind and no binding at all", Rules{}},
		{"a kind and no binding", rulesOf([]Kind{gh})},
		{"a host name", rulesOf([]Kind{{Name: "a", Hosts: []string{"slack.com"}}})},
		{"a suffix", rulesOf([]Kind{{Name: "a", Hosts: []string{"*.googleapis.com"}}})},
		{"a name with digits in all but its last label", rulesOf([]Kind{{Name: "a", Hosts: []string{"1e100.net", "*.0.example.com"}}})},
		{"two bindings of one kind", rulesOf([]Kind{gh}, Binding{Name: "A", Kind: "github"}, Binding{Name: "B", Kind: "github"})},
		{"an inline rule, whose own name is no kind's", rulesOf(nil, Binding{Name: "M", Inline: &Kind{Name: "Not A Kind", Hosts: []string{"dispatch.test"}}})},
		// A profile's row that names a host outside its kind's hosts is the policy load's to
		// refuse; Validate holds the shape of the kinds and the bindings alone.
		{"a profile row of any host", rulesOf([]Kind{gh}, Binding{Name: "A", Kind: "github", Allow: []Allow{{Method: "GET", PathPrefix: "/", Host: "elsewhere.example"}}})},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate(c.rules); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

// anthropicKind is the anthropic kind of the profiles file: Anthropic's Admin API alone, with a
// token the broker mints by workload identity federation.
func anthropicKind() Kind {
	return Kind{
		Name:      "anthropic",
		Hosts:     []string{"api.anthropic.com"},
		Headers:   []string{"Authorization"},
		Ways:      []string{WayMint},
		Variables: []string{"ANTHROPIC_ADMIN_TOKEN"},
		MaxGrant:  "1h",
		Allow:     anthropicRows(),
	}
}

// anthropicRows are the three allow rows of the anthropic kind, and of each Anthropic admin
// secret's profile in the profiles file.
func anthropicRows() []Allow {
	return []Allow{
		{Method: "GET", PathPrefix: "/v1/organizations/"},
		{Method: "POST", PathPrefix: "/v1/organizations/"},
		{Method: "DELETE", PathPrefix: "/v1/organizations/"},
	}
}

func TestCheckKind(t *testing.T) {
	if err := CheckKind(anthropicKind()); err != nil {
		t.Fatalf("CheckKind of the anthropic kind: %v", err)
	}
	cases := []struct {
		name string
		edit func(*Kind)
		want []string
	}{
		{"no ways", func(k *Kind) { k.Ways = nil }, []string{"ways names no way", "swap, mint, sign, exchange, dpop"}},
		// No path of the proxy takes an App's JSON Web Token in a call, so a kind may not say
		// its credential goes in that way.
		{"app-token, which no path of the proxy takes", func(k *Kind) { k.Ways = []string{WayMint, "app-token"} }, []string{`ways names "app-token", which is not one of swap, mint, sign, exchange, dpop`}},
		{"an empty way list", func(k *Kind) { k.Ways = []string{} }, []string{"ways names no way"}},
		{"an unknown way", func(k *Kind) { k.Ways = []string{WayMint, "sniff"} }, []string{`ways names "sniff", which is not one of swap, mint`}},
		{"a way in another case", func(k *Kind) { k.Ways = []string{"Mint"} }, []string{`ways names "Mint", which is not one of`}},
		{"a repeated way", func(k *Kind) { k.Ways = []string{WaySwap, WayMint, WaySwap} }, []string{`ways names "swap" twice`}},
		{"a host on a kind row", func(k *Kind) { k.Allow[1].Host = "api.anthropic.com" }, []string{`allow row POST /v1/organizations/ names the host "api.anthropic.com"`, "only a profile's row names one"}},
		{"a max_grant that is no duration", func(k *Kind) { k.MaxGrant = "1 hour" }, []string{`max_grant "1 hour" is not a Go duration of at least a second`}},
		{"a max_grant below a second", func(k *Kind) { k.MaxGrant = "500ms" }, []string{`max_grant "500ms"`}},
		{"a zero max_grant", func(k *Kind) { k.MaxGrant = "0s" }, []string{`max_grant "0s"`}},
		{"a negative max_grant", func(k *Kind) { k.MaxGrant = "-1h" }, []string{`max_grant "-1h"`}},
		{"a variable starting with a digit", func(k *Kind) { k.Variables = []string{"1TOKEN"} }, []string{`variables names "1TOKEN", which is no shell variable name`}},
		{"a variable with a hyphen", func(k *Kind) { k.Variables = []string{"ANTHROPIC-TOKEN"} }, []string{`variables names "ANTHROPIC-TOKEN"`}},
		{"an empty variable", func(k *Kind) { k.Variables = []string{"GH_TOKEN", ""} }, []string{`variables names ""`}},
		{"a variable with a non-ASCII letter", func(k *Kind) { k.Variables = []string{"TOK\u00c9N"} }, []string{"variables names", "which is no shell variable name"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := anthropicKind()
			c.edit(&k)
			err := CheckKind(k)
			if err == nil {
				t.Fatalf("CheckKind: no error")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
	ok := []struct {
		name string
		edit func(*Kind)
	}{
		{"each way of Ways, once", func(k *Kind) { k.Ways = append([]string(nil), Ways...) }},
		{"no max_grant", func(k *Kind) { k.MaxGrant = "" }},
		{"a max_grant of one second", func(k *Kind) { k.MaxGrant = "1s" }},
		{"no allow", func(k *Kind) { k.Allow = nil }},
		{"variables of underscores and digits", func(k *Kind) { k.Variables = []string{"_X", "GH_TOKEN", "a1"} }},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			k := anthropicKind()
			c.edit(&k)
			if err := CheckKind(k); err != nil {
				t.Errorf("CheckKind: %v", err)
			}
		})
	}
}

// TestTheRulesLookups holds the lookups the proxy and the release read the rules by: a name's
// binding, the kind it is decided under (its inline rule, or the kind it names), its hosts, and
// the headers a kind's credential goes into.
func TestTheRulesLookups(t *testing.T) {
	if b := validRules.Lookup("GH"); b == nil || b.Kind != "github" {
		t.Errorf("Lookup(GH) = %+v, want the binding of kind github", b)
	}
	if b := validRules.Lookup("gh"); b != nil {
		t.Errorf("Lookup(gh) = %+v, want nil: names are compared as they are written", b)
	}
	if k := validRules.KindOf(validRules.Lookup("MINTED")); k == nil || k.Name != "minted" {
		t.Errorf("KindOf(MINTED) = %+v, want its inline rule", k)
	}
	if k := validRules.KindOf(&Binding{Name: "X", Kind: "nowhere"}); k != nil {
		t.Errorf("KindOf of a binding naming no declared kind = %+v, want nil", k)
	}
	if k := validRules.KindOf(nil); k != nil {
		t.Errorf("KindOf(nil) = %+v, want nil", k)
	}
	if got, want := validRules.HostsOf("GH"), []string{"api.github.com", "*.githubusercontent.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("HostsOf(GH) = %q, want %q", got, want)
	}
	if got := validRules.HostsOf("UNKNOWN"); got != nil {
		t.Errorf("HostsOf(UNKNOWN) = %q, want nil", got)
	}
	if got := (Kind{}).HeaderNames(); !reflect.DeepEqual(got, []string{"authorization"}) {
		t.Errorf("HeaderNames of a kind with no headers = %q, want the default", got)
	}
	if got := (Kind{Headers: []string{}}).HeaderNames(); got == nil || len(got) != 0 {
		t.Errorf("HeaderNames of a kind with an empty list = %q, want that empty list", got)
	}
}

// TestHostShapeOK holds HostShapeOK to what Decide carries a value on. Each row is read against
// the request a TLS client makes to one host: that host as the CONNECT target and the Host
// header, and the name the client sends in its SNI, which for an address is none (RFC 6066
// section 3; Go's crypto/tls decides it with net.ParseIP). A refused host is read against the
// host whose service someone writing it plainly meant to bind, and Decide carries the value to
// none of them; an accepted one is read against a host it binds, and Decide carries the value
// there. HostBound alone is not the test, because an address matches itself exactly: what
// refuses every request to one is check's comparison of the SNI with the CONNECT host.
// Comparing a refused host with itself would prove nothing for the other shapes either, since
// HostBound matches any string by itself exactly, and "slack.com:443" is never a request host.
func TestHostShapeOK(t *testing.T) {
	cases := []struct {
		host string
		want bool
		// The request host to read the kind against: one Decide carries the value to by it
		// when the shape is accepted, and the name the kind meant to bind when it is refused.
		request string
	}{
		{host: "slack.com", want: true, request: "slack.com"},
		{host: "*.googleapis.com", want: true, request: "oauth2.googleapis.com"},
		{host: "API.GitHub.com", want: true, request: "api.github.com"},
		{host: "a-b.example.com", want: true, request: "a-b.example.com"},
		{host: "localhost", want: true, request: "localhost"},
		{host: "1e100.net", want: true, request: "1e100.net"},
		{host: "*.0.example.com", want: true, request: "a.0.example.com"},
		{host: "93.184.216.34", want: false, request: "93.184.216.34"},
		{host: "*.216.34", want: false, request: "93.184.216.34"},
		{host: "https://slack.com", want: false, request: "slack.com"},
		{host: "*", want: false, request: "slack.com"},
		{host: "slack.com/", want: false, request: "slack.com"},
		{host: "slack.com:443", want: false, request: "slack.com"},
		{host: ".slack.com", want: false, request: "a.slack.com"},
		{host: "slack..com", want: false, request: "slack.com"},
		{host: "slack.com.", want: false, request: "slack.com"},
		{host: "*.", want: false, request: "slack.com"},
		{host: "", want: false, request: "slack.com"},
		{host: "-slack.com", want: false, request: "slack.com"},
		{host: "slack-.com", want: false, request: "slack.com"},
		{host: "slack_com", want: false, request: "slack.com"},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			if got := HostShapeOK(c.host); got != c.want {
				t.Fatalf("HostShapeOK(%q) = %v, want %v", c.host, got, c.want)
			}
			sni := c.request
			if net.ParseIP(c.request) != nil {
				sni = ""
			}
			rules := rulesOf([]Kind{{Name: "slack", Hosts: []string{c.host}}}, Binding{Name: "SLACK", Kind: "slack"})
			grant := Grant{Secret: "SLACK", Placeholder: "agent-secrets-placeholder-slack"}
			r := &Request{
				ConnectHost: c.request, ConnectPort: 443, SNI: sni, HostHeader: c.request,
				Method: "GET", Path: "/", RawPath: "/", Headers: [][2]string{{"Authorization", "Bearer " + grant.Placeholder}},
			}
			carried, why := Decide(rules, grant, "the-value", r)
			if c.want && !carried {
				t.Errorf("HostShapeOK accepts %q, but Decide refuses a request to %q by it: %s", c.host, c.request, why)
			}
			if !c.want && carried {
				t.Errorf("HostShapeOK refuses %q, but Decide carries the value to %q by it", c.host, c.request)
			}
		})
	}
}

func TestHostBound(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		host  string
		want  bool
	}{
		{"exact", []string{"api.github.com"}, "api.github.com", true},
		{"the host's case is ignored", []string{"api.github.com"}, "API.GitHub.COM", true},
		{"the pattern's case is ignored", []string{"API.GITHUB.COM"}, "api.github.com", true},
		{"one label under a suffix", []string{"*.example.com"}, "a.example.com", true},
		{"several labels under a suffix", []string{"*.example.com"}, "a.b.c.example.com", true},
		{"the suffix itself", []string{"*.example.com"}, "example.com", false},
		{"a suffix is not a prefix match", []string{"*.example.com"}, "notexample.com", false},
		{"another host", []string{"api.github.com"}, "evil.example.com", false},
		{"no host at all", nil, "api.github.com", false},
		// strings.ToLower and strings.EqualFold fold U+212A KELVIN SIGN onto "k", which
		// would let a host the rules never name reach a rule.
		{"kelvin sign is not k", []string{"githuk.com"}, "githu\u212a.com", false},
		{"k is not kelvin sign", []string{"githu\u212a.com"}, "githuk.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HostBound(c.hosts, c.host); got != c.want {
				t.Errorf("HostBound(%q, %q) = %v, want %v", c.hosts, c.host, got, c.want)
			}
		})
	}
}

// decideRules, decideGrant, decideValue and decideRequest are an allowed call: a kind "api"
// that allows GET under /v1/, and the name "s" bound to it with no allow of its profile's own.
// Each case below breaks exactly one thing about it.
func decideRules() Rules {
	return rulesOf(
		[]Kind{{Name: "api", Hosts: []string{"api.example.com"}, Allow: []Allow{{Method: "GET", PathPrefix: "/v1/"}}}},
		Binding{Name: "s", Kind: "api", Way: WaySwap},
	)
}

func decideGrant() Grant { return Grant{Secret: "s", Placeholder: "PH"} }

const decideValue = "the-real-value"

func decideRequest() *Request {
	return &Request{
		ConnectHost: "api.example.com",
		ConnectPort: 443,
		SNI:         "api.example.com",
		HostHeader:  "api.example.com",
		Method:      "GET",
		Path:        "/v1/things",
		RawPath:     "/v1/things",
		Headers:     [][2]string{{"Authorization", "Bearer PH"}},
		Query:       "page=2",
		Body:        "{}",
	}
}

// decided runs one call through Decide and Rewrite, fails t when the two disagree on whether it
// is carried, and answers Decide's reason, "" for a carried call.
func decided(t *testing.T, rules Rules, g Grant, value string, r *Request) string {
	t.Helper()
	ok, why := Decide(rules, g, value, r)
	if ok != (why == "") {
		t.Errorf("Decide ok = %v with reason %q", ok, why)
	}
	if got := Rewrite(rules, g, value, r); (got == nil) == ok {
		t.Errorf("Rewrite = %v, but Decide ok = %v", got, ok)
	}
	return why
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Rules, *Grant, *string, *Request)
		want string // the refusal, or "" for an allowed request
	}{
		{"allowed", func(*Rules, *Grant, *string, *Request) {}, ""},
		{
			// The empty placeholder is read before the name is looked up.
			"empty placeholder",
			func(_ *Rules, g *Grant, _ *string, _ *Request) { g.Placeholder, g.Secret = "", "no-such-secret" },
			"empty placeholder",
		},
		{
			"unknown secret",
			func(_ *Rules, g *Grant, _ *string, _ *Request) { g.Secret = "other" },
			"unknown secret",
		},
		{
			"secret not behind airlock: a kind with no host",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) { rules.Kinds[0].Hosts = nil },
			"secret not behind airlock",
		},
		{
			"secret not behind airlock: a binding naming no declared kind",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) { rules.Names[0].Kind = "other" },
			"secret not behind airlock",
		},
		{
			"host not bound",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.ConnectHost, r.SNI, r.HostHeader = "evil.example.com", "evil.example.com", "evil.example.com"
			},
			"host not bound",
		},
		{
			"sni mismatch",
			func(_ *Rules, _ *Grant, _ *string, r *Request) { r.SNI = "other.example.com" },
			"sni mismatch",
		},
		{
			"host header mismatch",
			func(_ *Rules, _ *Grant, _ *string, r *Request) { r.HostHeader = "other.example.com" },
			"host header mismatch",
		},
		{
			"method not allowed by rule",
			func(_ *Rules, _ *Grant, _ *string, r *Request) { r.Method = "POST" },
			"not allowed by rule",
		},
		{
			"path not allowed by rule",
			func(_ *Rules, _ *Grant, _ *string, r *Request) { r.Path, r.RawPath = "/v2/things", "/v2/things" },
			"not allowed by rule",
		},
		{
			// A nil allow list allows anything; an empty one allows nothing.
			"an empty allow list allows nothing",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) { rules.Kinds[0].Allow = []Allow{} },
			"not allowed by rule",
		},
		{
			// The override header is read below the allow lists: a method a list refuses
			// is refused by the list, whichever headers the request carries.
			"the allow list is read before the method override header",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.Method = "POST"
				r.Headers = append(r.Headers, [2]string{"X-HTTP-Method-Override", "DELETE"})
			},
			"not allowed by rule",
		},
		{
			// And above the scan for the value, so a request carrying both is refused for
			// the header the allow list cannot bound.
			"the method override header is read before the value scan",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.Headers[0][1] = "Bearer " + decideValue
				r.Headers = append(r.Headers, [2]string{"X-HTTP-Method-Override", "DELETE"})
			},
			"request carries a method override header",
		},
		{
			"no allow list allows any method and path",
			func(rules *Rules, _ *Grant, _ *string, r *Request) {
				rules.Kinds[0].Allow = nil
				r.Method, r.Path, r.RawPath = "DELETE", "/anything", "/anything"
			},
			"",
		},
		{
			"request already carries the value",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.Headers[0][1] = "Bearer " + decideValue
			},
			"request already carries the value",
		},
		{
			"the value inside a Basic credential",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.Headers[0][1] = "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+decideValue))
			},
			"request already carries the value",
		},
		{
			// The check reads every header, not only the ones the kind names.
			"the value in a header the kind does not name",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.Headers = append(r.Headers, [2]string{"X-Other", decideValue})
			},
			"request already carries the value",
		},
		{
			// No way is exempt from that refusal.
			"the value carried for a name whose way is sign",
			func(rules *Rules, _ *Grant, _ *string, r *Request) {
				rules.Names[0].Way = WaySign
				r.Headers[0][1] = decideValue
			},
			"request already carries the value",
		},
		{
			// Every string contains the empty one, so an empty value refuses nothing.
			"an empty value is not carried",
			func(_ *Rules, _ *Grant, value *string, _ *Request) { *value = "" },
			"",
		},
		{
			// An inline rule is the kind its binding is decided under.
			"a binding's inline rule",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) {
				rules.Names[0] = Binding{Name: "s", Way: WayDPoP, Inline: &Kind{Name: "minted", Hosts: []string{"api.example.com"}, Allow: []Allow{{Method: "GET", PathPrefix: "/v1/"}}}}
				rules.Kinds = nil
			},
			"",
		},
		{
			"a binding's inline rule that does not bind the host",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) {
				rules.Names[0] = Binding{Name: "s", Way: WayDPoP, Inline: &Kind{Name: "minted", Hosts: []string{"dispatch.test"}}}
			},
			"host not bound",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules, g, value, r := decideRules(), decideGrant(), decideValue, decideRequest()
			c.edit(&rules, &g, &value, r)
			if why := decided(t, rules, g, value, r); why != c.want {
				t.Errorf("Decide reason = %q, want %q", why, c.want)
			}
		})
	}
}

// TestTheConnectPortAndTheDotSegmentRefusals covers the CONNECT port's refusal and the dot
// segment's, and the near misses on each side of the line each of them draws: the ports a
// CONNECT may name beside 443, the segments a reader resolves against the ones that only look
// like them, and a path with no segment at all.
func TestTheConnectPortAndTheDotSegmentRefusals(t *testing.T) {
	// decide answers the refusal of one request, "" for an allowed one. anyPath drops the
	// kind's allow list, which is what isolates the dot-segment refusal: with no allow list the
	// only thing left to refuse a path is that.
	decide := func(t *testing.T, anyPath bool, port uint16, path string) string {
		t.Helper()
		rules, r := decideRules(), decideRequest()
		if anyPath {
			rules.Kinds[0].Allow = nil
		}
		r.ConnectPort, r.Path, r.RawPath = port, path, path
		return decided(t, rules, decideGrant(), decideValue, r)
	}

	t.Run("the connect port", func(t *testing.T) {
		for _, c := range []struct {
			name string
			port uint16
			want string
		}{
			{"443, the one port the rules speak about", 443, ""},
			{"8443, another service of the same name", 8443, "connect port is not 443"},
			{"80, where no value may travel at all", 80, "connect port is not 443"},
			{"0, the port of a request that names none", 0, "connect port is not 443"},
			{"65535", 65535, "connect port is not 443"},
		} {
			t.Run(c.name, func(t *testing.T) {
				if got := decide(t, false, c.port, "/v1/things"); got != c.want {
					t.Errorf("Decide on port %d = %q, want %q", c.port, got, c.want)
				}
			})
		}
	})

	t.Run("a dot segment, whatever the kind allows", func(t *testing.T) {
		for _, c := range []struct {
			name string
			path string
			want string
		}{
			{"a dot-dot segment inside the prefix", "/v1/../admin", "path has a dot segment"},
			{"a dot-dot segment after a deeper path", "/v1/things/../../admin", "path has a dot segment"},
			{"a dot-dot segment alone", "/..", "path has a dot segment"},
			{"a dot segment alone", "/.", "path has a dot segment"},
			{"a dot segment in the middle", "/a/./b", "path has a dot segment"},
			{"..foo is no dot segment", "/v1/..foo", ""},
			{"... is no dot segment", "/v1/.../x", ""},
			{"a path with no segment at all", "", ""},
			{"a path a reader resolves to itself", "/v1/things", ""},
		} {
			t.Run(c.name, func(t *testing.T) {
				if got := decide(t, true, 443, c.path); got != c.want {
					t.Errorf("Decide on %q with no allow list = %q, want %q", c.path, got, c.want)
				}
			})
		}
	})

	t.Run("a dot segment is read before the allow list", func(t *testing.T) {
		// The first path is inside the kind's "/v1/" prefix as it is written and outside it
		// as a reader resolves it, which is the defect this refusal closes; the second is
		// outside it either way, and is still refused for the dot segment rather than by the
		// rule, because the dot-segment test is read first.
		for _, c := range []struct{ name, path string }{
			{"a path the allow list would have allowed", "/v1/../admin"},
			{"a path the allow list refuses anyway", "/admin/../v1/things"},
		} {
			t.Run(c.name, func(t *testing.T) {
				if got := decide(t, false, 443, c.path); got != "path has a dot segment" {
					t.Errorf("Decide on %q = %q, want %q", c.path, got, "path has a dot segment")
				}
			})
		}
	})
}

// TestThePathRulesUnderAnAllowList covers the five path rules beside the dot segment: a path
// with two slashes in a row, which a reader that merges them reads as another route; a path the
// command sent with "/" percent-encoded, which the decoded path reads as a separator and a service
// may read as a byte of one segment; a path the command sent with a "\" in either spelling, which
// a reader may take for a "/"; a dot segment with a ";" parameter, which Tomcat, Jetty and Spring
// cut off before they resolve dot segments; and a decoded path that still holds an escaped "/",
// ".", "\" or "%", which a server that decodes twice reads as one, however the command spelled
// the "%25" before it. Each is refused when the kind or the profile has an allow list, and above
// those lists. With neither list every path is allowed already, so each is carried: real clients
// send such paths, Cloud Storage's JSON API among them, which names the object dir/obj
// "dir%2Fobj". Each row's raw path is one a request sends for its decoded path.
func TestThePathRulesUnderAnAllowList(t *testing.T) {
	anyPath := []Allow{{Method: "GET", PathPrefix: "/"}}
	// decide answers the refusal of one GET of path, sent as rawPath, with the kind's allow and
	// the profile's set as given.
	decide := func(t *testing.T, kindAllow, profileAllow []Allow, path, rawPath string) string {
		t.Helper()
		rules, r := decideRules(), decideRequest()
		rules.Kinds[0].Allow, rules.Names[0].Allow = kindAllow, profileAllow
		r.Path, r.RawPath = path, rawPath
		return decided(t, rules, decideGrant(), decideValue, r)
	}
	const (
		empty     = "path has an empty segment"
		encoded   = "path has an encoded slash"
		backslash = "path has a backslash"
		parameter = "path has a parameter on a dot segment"
		double    = "path has a double-encoded character"
		dot       = "path has a dot segment"
	)
	lists := []struct {
		name          string
		kind, profile []Allow
	}{
		{"under the kind's allow", anyPath, nil},
		{"under the profile's allow", nil, anyPath},
		{"under both", anyPath, anyPath},
	}
	for _, c := range []struct {
		name          string
		path, rawPath string
		want          string // the refusal under an allow list
	}{
		{"an empty segment", "/a//b", "/a//b", empty},
		{"an empty segment at the start", "//v1/things", "//v1/things", empty},
		{"the path of two slashes", "//", "//", empty},
		{"an empty segment at the end", "/v1/things//", "/v1/things//", empty},
		{"a trailing slash is no empty segment", "/v1/things/", "/v1/things/", ""},
		{"Cloud Storage's object dir/obj", "/storage/v1/b/b/o/dir/obj", "/storage/v1/b/b/o/dir%2Fobj", encoded},
		{"an encoded slash, lower case", "/a/b", "/a%2fb", encoded},
		{"an encoded slash at the end", "/v1/things/", "/v1/things%2F", encoded},
		{"an encoded percent before 2F", "/v1/%2Fthings", "/v1/%252Fthings", double},
		{"a double-encoded slash with its 2 and F escaped too", "/v1/%2Fx", "/v1/%25%32%46x", double},
		{"a double-encoded slash with its F escaped", "/v1/%2Fx", "/v1/%252%46x", double},
		{"a double-encoded slash with its 2 escaped", "/v1/%2fx", "/v1/%25%32fx", double},
		{"a triple-encoded slash", "/v1/%252fx", "/v1/%25252fx", double},
		{"a double-encoded dot-dot spelled escape by escape", "/v1/%2E%2E/admin", "/v1/%25%32%45%25%32%45/admin", double},
		{"a double-encoded backslash spelled escape by escape", "/v1/%5Cx", "/v1/%25%35%43x", double},
		{"an encoded percent alone", "/v1/100%25", "/v1/100%2525", double},
		{"a literal backslash", `/v1\things`, `/v1\things`, backslash},
		{"an encoded backslash that walks out of /v1/", `/v1/x\..\..\admin`, "/v1/x%5C..%5C..%5Cadmin", backslash},
		{"an encoded backslash, lower case", `/a\b`, "/a%5cb", backslash},
		{"an encoded percent before 5C", "/a%5Cb", "/a%255Cb", double},
		{"a dot-dot segment with an empty parameter", "/v1/..;/admin", "/v1/..;/admin", parameter},
		{"a dot-dot segment with a parameter", "/v1/..;jsessionid=1/admin", "/v1/..;jsessionid=1/admin", parameter},
		{"a dot segment with a parameter", "/v1/.;/things", "/v1/.;/things", parameter},
		{"a dot-dot segment whose ; was sent as %3B", "/v1/..;/admin", "/v1/..%3B/admin", parameter},
		{"a dot-dot segment whose dots were sent as %2e", "/v1/..;/admin", "/v1/%2e%2e;/admin", parameter},
		{"three dots before a ; are no dot segment", "/v1/...;/x", "/v1/...;/x", ""},
		{"a ; before the dots", "/v1/;../x", "/v1/;../x", ""},
		{"dots after a name before the ;", "/v1/a..;/x", "/v1/a..;/x", ""},
		{"a double-encoded dot", "/v1/%2e%2e/admin", "/v1/%252e%252e/admin", double},
		{"a double-encoded slash, lower case", "/v1/a%2fb", "/v1/a%252fb", double},
		{"an encoded percent before another letter", "/v1/%41", "/v1/%2541", ""},
		{"a cut-off double escape", "/v1/%2", "/v1/%252", ""},
		{"a percent the command sent as itself, decoded", "/v1/100%", "/v1/100%25", ""},
		{"the dot segment is read first, and with no list too", "/v1/..//x", "/v1/..//x", dot},
		{"the empty segment is read before the encoded slash", "/v1///x", "/v1/%2F/x", empty},
		{"the encoded slash is read before the backslash", `/v1/x/\y`, "/v1/x%2F%5Cy", encoded},
		{"the backslash is read before the dot-segment parameter", `/v1/..;/a\b`, "/v1/..;/a%5Cb", backslash},
		{"the dot-segment parameter is read before the double encoding", "/v1/..;/a%2Fb", "/v1/..;/a%252Fb", parameter},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, l := range lists {
				if got := decide(t, l.kind, l.profile, c.path, c.rawPath); got != c.want {
					t.Errorf("%s, Decide on %q (sent as %q) = %q, want %q", l.name, c.path, c.rawPath, got, c.want)
				}
			}
			// With neither list only the dot segment, which no client means, is refused.
			want := ""
			if c.want == dot {
				want = dot
			}
			if got := decide(t, nil, nil, c.path, c.rawPath); got != want {
				t.Errorf("with no allow list, Decide on %q (sent as %q) = %q, want %q", c.path, c.rawPath, got, want)
			}
		})
	}
	t.Run("each is read before the allow list", func(t *testing.T) {
		// Each path is inside the kind's "/v1/" prefix as it is written, or decoded, and is
		// refused for its path rather than let through by the rule.
		for _, c := range []struct{ path, rawPath, want string }{
			{"/v1//things", "/v1//things", empty},
			{"/v1/things", "/v1%2Fthings", encoded},
			{`/v1/a\b`, "/v1/a%5Cb", backslash},
			{"/v1/x/..;/y", "/v1/x/..;/y", parameter},
			{"/v1/a%2Fb", "/v1/a%252Fb", double},
		} {
			if got := decide(t, []Allow{{Method: "GET", PathPrefix: "/v1/"}}, nil, c.path, c.rawPath); got != c.want {
				t.Errorf("Decide on %q (sent as %q) = %q, want %q", c.path, c.rawPath, got, c.want)
			}
		}
	})
	t.Run("with no list the path is forwarded as it was sent", func(t *testing.T) {
		rules, r := decideRules(), decideRequest()
		rules.Kinds[0].Allow = nil
		r.Path, r.RawPath = "/storage/v1/b/b/o/dir/obj", "/storage/v1/b/b/o/dir%2Fobj"
		out := Rewrite(rules, decideGrant(), decideValue, r)
		if out == nil || out.Path != r.Path || out.RawPath != r.RawPath {
			t.Fatalf("Rewrite = %+v, want the request with its path and raw path as they came", out)
		}
	})
}

// TestAllowHosts holds AllowHosts to allowOK's host rule: an allow that is missing, or that holds
// a row of no host, leaves the kind's hosts (nil, false); otherwise the rows' hosts, each once in
// the order first named (true), and for a present empty allow an empty list that is not nil. A
// host is folded to lower case, as allowOK compares it, so two spellings of one host are one. Each
// narrowed answer is checked against allowOK itself: a request on a row's method and prefix is
// allowed at the row's own host, and at no host the answer leaves out.
func TestAllowHosts(t *testing.T) {
	row := func(method, prefix, host string) Allow { return Allow{Method: method, PathPrefix: prefix, Host: host} }
	for _, c := range []struct {
		name     string
		allow    []Allow
		hosts    []string
		narrowed bool
	}{
		{"a missing allow", nil, nil, false},
		{"a present empty allow", []Allow{}, []string{}, true},
		{"rows of one host", []Allow{row("GET", "/a/", "api.github.com"), row("PUT", "/a/", "api.github.com")}, []string{"api.github.com"}, true},
		{"rows of two hosts, in the order first named", []Allow{row("GET", "/a/", "uploads.github.com"), row("GET", "/b/", "api.github.com"), row("PUT", "/a/", "uploads.github.com")}, []string{"uploads.github.com", "api.github.com"}, true},
		{"two spellings of one host are one, folded as allowOK folds them", []Allow{row("GET", "/a/", "API.GitHub.com"), row("PUT", "/a/", "api.github.com")}, []string{"api.github.com"}, true},
		{"a row of no host among rows of one", []Allow{row("GET", "/a/", "api.github.com"), row("GET", "/b/", "")}, nil, false},
		{"a row of no host alone", []Allow{row("GET", "/a/", "")}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			hosts, narrowed := AllowHosts(c.allow)
			if !reflect.DeepEqual(hosts, c.hosts) || narrowed != c.narrowed {
				t.Fatalf("AllowHosts = %#v, %v; want %#v, %v", hosts, narrowed, c.hosts, c.narrowed)
			}
			if !narrowed {
				return
			}
			for _, a := range c.allow {
				if !allowOK(c.allow, &Request{ConnectHost: fold.ASCII(a.Host), Method: a.Method, Path: a.PathPrefix}) {
					t.Errorf("allowOK refuses %s %s at the row's own host %s", a.Method, a.PathPrefix, fold.ASCII(a.Host))
				}
				for _, host := range []string{"api.github.com", "uploads.github.com", "github.com"} {
					r := &Request{ConnectHost: host, Method: a.Method, Path: a.PathPrefix}
					if allowOK(c.allow, r) && !slices.Contains(hosts, host) {
						t.Errorf("allowOK allows %s %s at %s, which AllowHosts leaves out of %q", a.Method, a.PathPrefix, host, hosts)
					}
				}
			}
		})
	}
}

// TestThePrefixBoundary holds an allow row's prefix to a "/" boundary: the path is the prefix,
// or continues it where the prefix ends with "/" or the path's next character is "/". A plain
// starts-with would let "/storage-limitX" through a row for "/storage-limit", another route. The
// boundary is the same on a kind's row and on a profile's.
func TestThePrefixBoundary(t *testing.T) {
	const limit = "/enterprises/t/actions/cache/storage-limit"
	rules := rulesOf(
		[]Kind{{Name: "github", Hosts: []string{"api.github.com"}, Allow: []Allow{{Method: "GET", PathPrefix: limit}, {Method: "GET", PathPrefix: "/repos/"}}}},
		Binding{Name: "KIND", Kind: "github"},
		Binding{Name: "PROFILE", Kind: "github", Allow: []Allow{{Method: "GET", PathPrefix: "/repos/o"}}},
	)
	request := func(path string) *Request {
		return &Request{ConnectHost: "api.github.com", ConnectPort: 443, SNI: "api.github.com", HostHeader: "api.github.com",
			Method: "GET", Path: path, RawPath: path, Headers: [][2]string{{"Authorization", "Bearer PH"}}}
	}
	for _, c := range []struct {
		name, secret, path, want string
	}{
		{"the prefix itself", "KIND", limit, ""},
		{"past the prefix at a slash", "KIND", limit + "/7", ""},
		{"beside the prefix", "KIND", limit + "X", "not allowed by rule"},
		{"beside the prefix, with a slash after", "KIND", limit + "X/7", "not allowed by rule"},
		{"short of the prefix", "KIND", "/enterprises/t/actions/cache/storage-limi", "not allowed by rule"},
		{"a prefix that ends with a slash holds what follows", "KIND", "/repos/anything", ""},
		{"a prefix that ends with a slash does not hold the path without it", "KIND", "/repos", "not allowed by rule"},
		{"a profile row's prefix itself", "PROFILE", "/repos/o", ""},
		{"past a profile row's prefix at a slash", "PROFILE", "/repos/o/r", ""},
		{"beside a profile row's prefix", "PROFILE", "/repos/other", "not allowed by the profile's rule"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := decided(t, rules, Grant{Secret: c.secret, Placeholder: "PH"}, decideValue, request(c.path)); got != c.want {
				t.Errorf("Decide %s on %q = %q, want %q", c.secret, c.path, got, c.want)
			}
		})
	}
}

// TestTheProfileAllow holds the decision to both lists: a request goes through only when its
// kind's allow and its binding's own allow (its profile's) both allow it. The kind's is read
// first, so a profile row never widens its kind: a request the profile allows and the kind does
// not is refused by the kind's rule. A profile's row that names a host allows the request on
// that host alone.
func TestTheProfileAllow(t *testing.T) {
	kind := Kind{Name: "github", Hosts: []string{"api.github.com", "uploads.github.com"},
		Allow: []Allow{{Method: "GET", PathPrefix: "/enterprises/t/"}, {Method: "PUT", PathPrefix: "/enterprises/t/actions/"}}}
	request := func(host, method, path string) *Request {
		return &Request{ConnectHost: host, ConnectPort: 443, SNI: host, HostHeader: host,
			Method: method, Path: path, RawPath: path, Headers: [][2]string{{"Authorization", "Bearer PH"}}}
	}
	decide := func(t *testing.T, kindAllow, profileAllow []Allow, r *Request) string {
		t.Helper()
		k := kind
		k.Allow = kindAllow
		rules := rulesOf([]Kind{k}, Binding{Name: "T", Kind: "github", Allow: profileAllow})
		return decided(t, rules, Grant{Secret: "T", Placeholder: "PH"}, decideValue, r)
	}
	profile := []Allow{{Method: "GET", PathPrefix: "/enterprises/t/audit-log/streams"}}
	for _, c := range []struct {
		name         string
		kindAllow    []Allow
		profileAllow []Allow
		r            *Request
		want         string
	}{
		{"both allow it", kind.Allow, profile, request("api.github.com", "GET", "/enterprises/t/audit-log/streams/7"), ""},
		{"the kind allows it and the profile does not", kind.Allow, profile, request("api.github.com", "GET", "/enterprises/t/audit-log/stream-key"), "not allowed by the profile's rule"},
		{"the profile allows it and the kind does not: no widening", kind.Allow, []Allow{{Method: "GET", PathPrefix: "/enterprises/t/billing/"}}, request("api.github.com", "GET", "/enterprises/other/billing/x"), "not allowed by rule"},
		{"a profile row outside the kind's allow allows nothing", kind.Allow, []Allow{{Method: "DELETE", PathPrefix: "/enterprises/t/"}}, request("api.github.com", "DELETE", "/enterprises/t/x"), "not allowed by rule"},
		{"no allow of the profile's own: the kind's alone", kind.Allow, nil, request("api.github.com", "GET", "/enterprises/t/runners"), ""},
		{"an empty allow of the profile's own allows nothing", kind.Allow, []Allow{}, request("api.github.com", "GET", "/enterprises/t/runners"), "not allowed by the profile's rule"},
		{"a kind with no allow: the profile's alone", nil, profile, request("api.github.com", "POST", "/graphql"), "not allowed by the profile's rule"},
		{"a kind with no allow and a profile that allows it", nil, profile, request("api.github.com", "GET", "/enterprises/t/audit-log/streams"), ""},
		{"neither has an allow", nil, nil, request("api.github.com", "POST", "/graphql"), ""},
		{"a profile row's host allows that host", kind.Allow, []Allow{{Method: "GET", PathPrefix: "/enterprises/t/", Host: "api.github.com"}}, request("api.github.com", "GET", "/enterprises/t/x"), ""},
		{"a profile row's host allows no other host of the kind", kind.Allow, []Allow{{Method: "GET", PathPrefix: "/enterprises/t/", Host: "api.github.com"}}, request("uploads.github.com", "GET", "/enterprises/t/x"), "not allowed by the profile's rule"},
		{"a profile row's host is compared ASCII-folded", kind.Allow, []Allow{{Method: "GET", PathPrefix: "/enterprises/t/", Host: "API.GitHub.com"}}, request("api.github.com", "GET", "/enterprises/t/x"), ""},
		{"a profile row with no host allows every host of the kind", kind.Allow, []Allow{{Method: "GET", PathPrefix: "/enterprises/t/"}}, request("uploads.github.com", "GET", "/enterprises/t/x"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := decide(t, c.kindAllow, c.profileAllow, c.r); got != c.want {
				t.Errorf("Decide %s %s at %s = %q, want %q", c.r.Method, c.r.Path, c.r.ConnectHost, got, c.want)
			}
		})
	}
}

// TestTheMethodOverrideRefusal covers the refusal of a header a service reads the method
// from: each of the three names, in the cases a client may send them in, the near misses
// the fold and the whole-name comparison draw a line at, under an allow list of the kind's or
// of the profile's, and the one shape that is not refused at all, a kind and a profile with no
// allow list, which allow every method already.
func TestTheMethodOverrideRefusal(t *testing.T) {
	// decide answers the refusal of one request carrying headers, "" for an allowed one, with
	// the kind's allow list and the profile's set as given.
	decide := func(t *testing.T, kindAllow, profileAllow []Allow, headers [][2]string) string {
		t.Helper()
		rules, r := decideRules(), decideRequest()
		rules.Kinds[0].Allow = kindAllow
		rules.Names[0].Allow = profileAllow
		r.Headers = append(r.Headers, headers...)
		return decided(t, rules, decideGrant(), decideValue, r)
	}
	rows := []Allow{{Method: "GET", PathPrefix: "/v1/"}}

	t.Run("under a kind with an allow list", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			header [2]string
			want   string
		}{
			{"X-HTTP-Method-Override, the one Google's APIs honour", [2]string{"X-HTTP-Method-Override", "DELETE"}, "request carries a method override header"},
			{"x-http-method-override, lower-cased as HTTP/2 sends it", [2]string{"x-http-method-override", "PATCH"}, "request carries a method override header"},
			{"X-HTTP-Method", [2]string{"X-HTTP-Method", "DELETE"}, "request carries a method override header"},
			{"X-Method-Override", [2]string{"X-Method-Override", "PUT"}, "request carries a method override header"},
			{"X-METHOD-OVERRIDE, every letter folded", [2]string{"X-METHOD-OVERRIDE", "PUT"}, "request carries a method override header"},
			// The value is the service's to read, so the header is refused whatever it
			// holds, the empty value and the request's own method included.
			{"an empty value", [2]string{"X-HTTP-Method-Override", ""}, "request carries a method override header"},
			{"the method the request already names", [2]string{"X-HTTP-Method-Override", "GET"}, "request carries a method override header"},
			// Near misses: the comparison is of the whole name, ASCII-folded.
			{"a longer name that only starts like one", [2]string{"X-HTTP-Method-Override-2", "DELETE"}, ""},
			{"a name the Unicode fold would reach and this one does not", [2]string{"X-HTTP-Met\u212aod", "DELETE"}, ""},
			{"a header the kind has nothing to say about", [2]string{"X-Request-Id", "7"}, ""},
		} {
			t.Run(c.name, func(t *testing.T) {
				if got := decide(t, rows, nil, [][2]string{c.header}); got != c.want {
					t.Errorf("Decide with %q = %q, want %q", c.header, got, c.want)
				}
			})
		}
	})

	t.Run("either list's allow refuses it", func(t *testing.T) {
		header := [][2]string{{"X-HTTP-Method-Override", "DELETE"}}
		if got := decide(t, rows, nil, header); got != "request carries a method override header" {
			t.Errorf("with the kind's allow alone, Decide = %q, want the header refused", got)
		}
		if got := decide(t, nil, rows, header); got != "request carries a method override header" {
			t.Errorf("with the profile's allow alone, Decide = %q, want the header refused", got)
		}
		if got := decide(t, rows, rows, header); got != "request carries a method override header" {
			t.Errorf("with both allow lists, Decide = %q, want the header refused", got)
		}
	})

	t.Run("a request carrying none of them passes under both lists", func(t *testing.T) {
		if got := decide(t, rows, rows, nil); got != "" {
			t.Errorf("Decide = %q, want the request allowed", got)
		}
	})

	t.Run("neither list: the header is not refused", func(t *testing.T) {
		// Such a name allows every method already, so a header naming another takes nothing
		// away from it and is not refused.
		for _, h := range [][2]string{
			{"X-HTTP-Method-Override", "DELETE"},
			{"x-http-method", "DELETE"},
			{"x-method-override", "DELETE"},
		} {
			t.Run(h[0], func(t *testing.T) {
				if got := decide(t, nil, nil, [][2]string{h}); got != "" {
					t.Errorf("Decide with %q and no allow list = %q, want the request allowed", h, got)
				}
			})
		}
	})

	t.Run("an empty allow list is refused by the list itself", func(t *testing.T) {
		// An empty list allows nothing, which is read first, so the header is never the
		// reason such a request is refused.
		header := [][2]string{{"X-HTTP-Method-Override", "DELETE"}}
		if got := decide(t, []Allow{}, nil, header); got != "not allowed by rule" {
			t.Errorf("Decide = %q, want %q", got, "not allowed by rule")
		}
		if got := decide(t, nil, []Allow{}, header); got != "not allowed by the profile's rule" {
			t.Errorf("Decide = %q, want %q", got, "not allowed by the profile's rule")
		}
	})
}

// TestTheMethodOverrideParameterRefusal covers the refusal of a query parameter a service reads
// the method from: a name that decodes, as Express and PHP decode it and as PHP and Symfony cut it
// at a NUL, to "_method", in the cases, the escapes and the PHP spellings a client may send it in
// and wherever it stands in the query, after a "&" or a ";", with or without a value; a name that
// does not decode; the near misses the whole-name comparison draws a line at; and the one shape
// that is not refused, a kind and a profile with no allow list, which allow every method already.
// Its first four rows are the names a comparison of the name as sent let through.
func TestTheMethodOverrideParameterRefusal(t *testing.T) {
	const refused = "request carries a method override parameter"
	rows := []Allow{{Method: "GET", PathPrefix: "/v1/"}}
	// decide answers the refusal of one request with the query given, "" for an allowed one,
	// with the kind's allow list and the profile's set as given.
	decide := func(t *testing.T, kindAllow, profileAllow []Allow, query string) string {
		t.Helper()
		rules, r := decideRules(), decideRequest()
		rules.Kinds[0].Allow, rules.Names[0].Allow = kindAllow, profileAllow
		r.Query = query
		return decided(t, rules, decideGrant(), decideValue, r)
	}
	for _, c := range []struct{ name, query, want string }{
		{"an escaped letter", "_meth%6Fd=DELETE", refused},
		{"an escaped _ and escaped letters", "%5F%6Detho%64=DELETE", refused},
		{"a dot for the _", ".method=DELETE", refused},
		{"a + before it", "+_method=DELETE", refused},
		{"_method", "_method=DELETE", refused},
		{"_method after another parameter", "page=2&_method=PUT", refused},
		{"_method before another parameter", "_method=PUT&page=2", refused},
		{"_method with no value", "page=2&_method", refused},
		{"_method with an empty value", "_method=", refused},
		{"_method naming the request's own method", "_method=GET", refused},
		{"_METHOD, every letter folded", "_METHOD=PUT", refused},
		{"_method with its _ percent-encoded", "%5Fmethod=PUT", refused},
		{"_method with its _ percent-encoded in lower case", "%5fmethod=PUT", refused},
		{"_method with an encoded space before it", "%20_method=PUT", refused},
		{"_method with a space before it", "page=2& _method=PUT", refused},
		{"_method with an opening bracket for its _", "[method=PUT", refused},
		{"_method with a dot and an encoded letter", ".meth%6fd=PUT", refused},
		// PHP and Symfony cut a decoded name at its first NUL.
		{"_method with a NUL after it", "_method%00=PUT", refused},
		{"a dot for the _, a NUL and more after it", ".method%00x=PUT", refused},
		{"an escaped _ with a NUL after it", "%5Fmethod%00=PUT", refused},
		{"_method with a NUL and a longer name after it", "_method%00s=PUT", refused},
		// The query is split on ";" as well as "&".
		{"_method after a semicolon", "x;_method=DELETE", refused},
		{"_method after a value and a semicolon", "a=1;_method=PUT", refused},
		{"_method before a semicolon", "_method=PUT;a=1", refused},
		// A name that does not decode is one the airlock cannot say the service reads otherwise.
		{"_method with an escape that does not decode", "_method%zz=1", refused},
		{"another name with a cut-off escape", "q%=1", refused},
		{"another name with an escape of no hex digit", "%G0=1", refused},
		// Near misses: the name is decoded and compared whole, and only the text before its
		// first "=".
		{"a longer name that ends like it", "x_method=PUT", ""},
		{"a longer name that starts like it", "_methods=PUT", ""},
		{"a longer name that starts like it, encoded", "%5Fmethods=PUT", ""},
		{"a longer name that ends like it, encoded", "x%5Fmethod=PUT", ""},
		{"a name that decodes to _method and a dot after it", "_method%2e=PUT", ""},
		{"_method as an array, which the frameworks read as an array and no method", "_method[]=PUT", ""},
		{"_method as an array of one key", "_method[x]=PUT", ""},
		{"a NUL before _method", "%00_method=PUT", ""},
		{"a + before method, which the trim takes", "+method=PUT", ""},
		{"_method as a value", "a=_method", ""},
		{"_method as a value after a semicolon", "a=1;b=_method", ""},
		{"a value that does not decode", "a=%zz", ""},
		{"no query at all", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, l := range []struct {
				name          string
				kind, profile []Allow
			}{{"under the kind's allow", rows, nil}, {"under the profile's allow", nil, rows}, {"under both", rows, rows}} {
				if got := decide(t, l.kind, l.profile, c.query); got != c.want {
					t.Errorf("%s, Decide with query %q = %q, want %q", l.name, c.query, got, c.want)
				}
			}
			if got := decide(t, nil, nil, c.query); got != "" {
				t.Errorf("with no allow list, Decide with query %q = %q, want the request allowed", c.query, got)
			}
		})
	}
	t.Run("it is read after the allow lists and the override header, before the value scan", func(t *testing.T) {
		rules, r := decideRules(), decideRequest()
		r.Query, r.Method = "_method=DELETE", "POST"
		if got := decided(t, rules, decideGrant(), decideValue, r); got != "not allowed by rule" {
			t.Errorf("a POST the kind does not allow: Decide = %q, want %q", got, "not allowed by rule")
		}
		r.Method = "GET"
		r.Headers = append(r.Headers, [2]string{"X-HTTP-Method-Override", "DELETE"})
		if got := decided(t, rules, decideGrant(), decideValue, r); got != "request carries a method override header" {
			t.Errorf("with the header too: Decide = %q, want the header named", got)
		}
		r.Headers = [][2]string{{"Authorization", "Bearer " + decideValue}}
		if got := decided(t, rules, decideGrant(), decideValue, r); got != refused {
			t.Errorf("with the value carried too: Decide = %q, want %q", got, refused)
		}
	})
}

// withoutCipherRepository is headers less every Cipher-Repository header, which Rewrite removes.
func withoutCipherRepository(headers [][2]string) [][2]string {
	var out [][2]string
	for _, h := range headers {
		if fold.ASCII(h[0]) != CipherRepositoryHeader {
			out = append(out, h)
		}
	}
	return out
}

// rewritten runs Rewrite over one kind and one set of headers, and answers the headers it gave
// back. It fails the test when Rewrite refuses, when it changed the request it read, or when the
// answer's headers are not the request's less its Cipher-Repository headers, in order, each with
// its name.
func rewritten(t *testing.T, k Kind, headers [][2]string) [][2]string {
	t.Helper()
	k.Name = "api"
	if k.Hosts == nil {
		k.Hosts = []string{"api.example.com"}
	}
	rules := rulesOf([]Kind{k}, Binding{Name: "s", Kind: "api"})
	r := decideRequest()
	r.Headers = headers
	before := append([][2]string(nil), headers...)

	out := Rewrite(rules, decideGrant(), decideValue, r)
	if out == nil {
		t.Fatalf("Rewrite refused: %q", mustReason(rules, r))
	}
	for i, h := range r.Headers {
		if h != before[i] {
			t.Errorf("Rewrite changed the request it read: header %d is %q, was %q", i, h, before[i])
		}
	}
	if out.Query != r.Query || out.Body != r.Body {
		t.Errorf("Rewrite changed query %q or body %q, want %q and %q", out.Query, out.Body, r.Query, r.Body)
	}
	if out.ConnectHost != r.ConnectHost || out.SNI != r.SNI || out.HostHeader != r.HostHeader ||
		out.Method != r.Method || out.Path != r.Path || out.RawPath != r.RawPath {
		t.Errorf("Rewrite changed the request's route: %+v, want %+v", out, r)
	}
	kept := withoutCipherRepository(headers)
	if len(out.Headers) != len(kept) {
		t.Fatalf("Rewrite gave %d headers, want %d", len(out.Headers), len(kept))
	}
	for i, h := range out.Headers {
		if h[0] != kept[i][0] {
			t.Errorf("header %d is named %q, want %q", i, h[0], kept[i][0])
		}
	}
	return out.Headers
}

func mustReason(rules Rules, r *Request) string {
	_, why := Decide(rules, decideGrant(), decideValue, r)
	return why
}

func basic(payload string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(payload))
}

// TestAdmitIsTheDecisionBeforeTheValue: Admit is check's rules up to the one that reads the
// value, so on every case the committed FuzzAirlock corpus and a fixed run of random byte strings
// generate (genCase), a request Admit refuses Decide refuses with the same reason, for the case's
// value and for no value at all; and a request Admit lets through, Decide lets through with no
// value, and with the case's value lets through or refuses for that value alone. It needs no
// model, so it runs in every `go test`. The run must reach each of Admit's seventeen refusals and
// the value's own, so that each rule is one a disagreement could show on.
func TestAdmitIsTheDecisionBeforeTheValue(t *testing.T) {
	const carries = "request already carries the value"
	var cases [][]byte
	seeds, err := os.ReadDir(filepath.Join("testdata", "fuzz", "FuzzAirlock"))
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range seeds {
		text, err := os.ReadFile(filepath.Join("testdata", "fuzz", "FuzzAirlock", seed.Name()))
		if err != nil {
			t.Fatal(err)
		}
		// A seed file is the line "go test fuzz v1" and one []byte literal of Go.
		header, literal, _ := strings.Cut(strings.TrimSpace(string(text)), "\n")
		quoted, ok := strings.CutPrefix(literal, "[]byte(")
		quoted, closed := strings.CutSuffix(quoted, ")")
		data, err := strconv.Unquote(quoted)
		if header != "go test fuzz v1" || !ok || !closed || err != nil {
			t.Fatalf("seed %s is no one []byte literal: %q", seed.Name(), text)
		}
		cases = append(cases, []byte(data))
	}
	if len(cases) == 0 {
		t.Fatal("the committed corpus holds no seed, so the run below would read only random cases")
	}
	random := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		data := make([]byte, random.IntN(64))
		for i := range data {
			data[i] = byte(random.UintN(256))
		}
		cases = append(cases, data)
	}
	reached := map[string]int{}
	for _, data := range cases {
		c := genCase(data)
		kind, admitted := Admit(c.Rules, c.Grant, c.Request)
		_, decided := Decide(c.Rules, c.Grant, c.Value, c.Request)
		_, valueless := Decide(c.Rules, c.Grant, "", c.Request)
		reached[admitted]++
		if decided == carries {
			reached[carries]++
		}
		switch {
		case (kind == nil) != (admitted != ""):
			t.Fatalf("Admit answered the kind %v with the reason %q for %x; want a kind exactly when it gives no reason", kind, admitted, data)
		case valueless != admitted:
			t.Fatalf("Admit answered %q and Decide with no value %q for %x; want the same answer", admitted, valueless, data)
		case admitted != "" && decided != admitted:
			t.Fatalf("Admit refused %x for %q and Decide with the value answered %q; want the same refusal", data, admitted, decided)
		case admitted == "" && decided != "" && decided != carries:
			t.Fatalf("Admit let %x through and Decide with the value refused it for %q; want only the value's own refusal", data, decided)
		}
	}
	for _, why := range []string{
		"", "empty placeholder", "unknown secret", "secret not behind airlock", "host not bound", "connect port is not 443",
		"sni mismatch", "host header mismatch", "path has a dot segment", "path has an empty segment",
		"path has an encoded slash", "path has a backslash", "path has a parameter on a dot segment",
		"path has a double-encoded character", "not allowed by rule", "not allowed by the profile's rule",
		"request carries a method override header", "request carries a method override parameter", carries,
	} {
		if reached[why] == 0 {
			t.Errorf("no case of the %d reached the answer %q, so a disagreement on its rule could not show", len(cases), why)
		}
	}
}

func TestRewrite(t *testing.T) {
	t.Run("the named header, and no other", func(t *testing.T) {
		headers := rewritten(t, Kind{}, [][2]string{
			{"X-Before", "PH stays"},
			{"Authorization", "Bearer PH"},
			{"X-After", "PH stays"},
		})
		if headers[1][1] != "Bearer "+decideValue {
			t.Errorf("authorization = %q, want %q", headers[1][1], "Bearer "+decideValue)
		}
		for _, i := range []int{0, 2} {
			if headers[i][1] != "PH stays" {
				t.Errorf("header %q = %q, want it byte for byte", headers[i][0], headers[i][1])
			}
		}
	})

	t.Run("the kind's header names are matched without case", func(t *testing.T) {
		headers := rewritten(t, Kind{Headers: []string{"X-TOKEN"}}, [][2]string{
			{"x-token", "PH"},
			{"Authorization", "Bearer PH"},
		})
		if headers[0][1] != decideValue {
			t.Errorf("x-token = %q, want %q", headers[0][1], decideValue)
		}
		if headers[1][1] != "Bearer PH" {
			t.Errorf("authorization = %q, want it untouched: the kind names x-token alone", headers[1][1])
		}
	})

	t.Run("a kind that names no header swaps nothing", func(t *testing.T) {
		headers := rewritten(t, Kind{Headers: []string{}}, [][2]string{{"Authorization", "Bearer PH"}})
		if headers[0][1] != "Bearer PH" {
			t.Errorf("authorization = %q, want it untouched", headers[0][1])
		}
	})

	t.Run("several occurrences in one value", func(t *testing.T) {
		headers := rewritten(t, Kind{Headers: []string{"x-token"}}, [][2]string{
			{"x-token", "PH, PH and PH"},
		})
		want := decideValue + ", " + decideValue + " and " + decideValue
		if headers[0][1] != want {
			t.Errorf("x-token = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("inside a Basic credential, as the user", func(t *testing.T) {
		headers := rewritten(t, Kind{}, [][2]string{{"Authorization", basic("PH:x-oauth-basic")}})
		if want := basic(decideValue + ":x-oauth-basic"); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("inside a Basic credential, as the password", func(t *testing.T) {
		headers := rewritten(t, Kind{}, [][2]string{{"Authorization", basic("user:PH")}})
		if want := basic("user:" + decideValue); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("spaces and tabs around a Basic credential are trimmed", func(t *testing.T) {
		headers := rewritten(t, Kind{}, [][2]string{
			{"Authorization", "Basic \t " + base64.StdEncoding.EncodeToString([]byte("user:PH")) + " \t"},
		})
		if want := basic("user:" + decideValue); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("no other space is trimmed", func(t *testing.T) {
		// strings.TrimSpace would take U+00A0 and the credential would decode; the model
		// trims a space and a horizontal tab alone, so this value is no Basic credential
		// and takes the plain replacement, which finds no placeholder in the base64.
		in := "Basic \u00a0" + base64.StdEncoding.EncodeToString([]byte("user:PH"))
		headers := rewritten(t, Kind{}, [][2]string{{"Authorization", in}})
		if headers[0][1] != in {
			t.Errorf("authorization = %q, want it untouched: %q", headers[0][1], in)
		}
	})

	t.Run("a Basic payload that is not valid UTF-8 takes the plain replacement", func(t *testing.T) {
		// The payload holds the placeholder, but it is not valid UTF-8, so it is no Basic
		// credential: the plain replacement runs over the header value itself, which holds
		// no placeholder, and the value is left as it came.
		in := "Basic " + base64.StdEncoding.EncodeToString([]byte{0xff, 'P', 'H'})
		headers := rewritten(t, Kind{}, [][2]string{{"Authorization", in}})
		if headers[0][1] != in {
			t.Errorf("authorization = %q, want it untouched: %q", headers[0][1], in)
		}
	})

	t.Run("a Basic credential without the placeholder takes the plain replacement", func(t *testing.T) {
		in := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:other")) + " PH"
		headers := rewritten(t, Kind{}, [][2]string{{"Authorization", in}})
		if want := strings.Replace(in, "PH", decideValue, 1); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("Cipher-Repository is removed in every case spelling, and nothing else", func(t *testing.T) {
		in := [][2]string{
			{"X-First", "1"},
			{"Cipher-Repository", "example-org/x"},
			{"Authorization", "Bearer PH"},
			{"cipher-repository", "o/y"},
			{"Cipher-RepositoryX", "kept: a longer name"},
			{"CIPHER-REPOSITORY", "PH"},
			{"Cipher-Repo\u017fitory", "kept: strings.EqualFold folds U+017F onto s, the ASCII fold does not"},
			{"X-Last", "PH stays"},
		}
		headers := rewritten(t, Kind{}, in)
		want := [][2]string{
			{"X-First", "1"},
			{"Authorization", "Bearer " + decideValue},
			{"Cipher-RepositoryX", "kept: a longer name"},
			{"Cipher-Repo\u017fitory", "kept: strings.EqualFold folds U+017F onto s, the ASCII fold does not"},
			{"X-Last", "PH stays"},
		}
		if !reflect.DeepEqual(headers, want) {
			t.Errorf("headers = %q, want %q", headers, want)
		}
	})

	t.Run("Cipher-Repository is removed when the kind names it", func(t *testing.T) {
		headers := rewritten(t, Kind{Headers: []string{"Cipher-Repository", "Authorization"}}, [][2]string{
			{"Cipher-Repository", "PH"},
			{"Authorization", "Bearer PH"},
		})
		if want := [][2]string{{"Authorization", "Bearer " + decideValue}}; !reflect.DeepEqual(headers, want) {
			t.Errorf("headers = %q, want %q", headers, want)
		}
	})

	t.Run("a request of Cipher-Repository alone keeps no header", func(t *testing.T) {
		headers := rewritten(t, Kind{}, [][2]string{{"Cipher-Repository", "o/x"}})
		if headers == nil || len(headers) != 0 {
			t.Errorf("headers = %#v, want an empty list", headers)
		}
	})

	t.Run("a refused request is no request", func(t *testing.T) {
		r := decideRequest()
		r.SNI = "other.example.com"
		if out := Rewrite(decideRules(), decideGrant(), decideValue, r); out != nil {
			t.Errorf("Rewrite of a refused request = %+v, want nil", out)
		}
	})
}

// TestCoversAndOutside holds the conservative cover rule the policy load and `profiles check
// --against` read: a missing old list covers every row; otherwise a row of old covers a row of
// the same method whose prefix is the old prefix or continues it at a "/" boundary, when the old
// row names no host or the row's host.
func TestCoversAndOutside(t *testing.T) {
	row := func(method, prefix, host string) Allow { return Allow{Method: method, PathPrefix: prefix, Host: host} }
	for _, c := range []struct {
		name string
		old  []Allow
		row  Allow
		want bool
	}{
		{"a missing old list covers every row", nil, row("DELETE", "/", "elsewhere.example"), true},
		{"an empty old list covers none", []Allow{}, row("GET", "/v1/", ""), false},
		{"the same row", []Allow{row("GET", "/v1/organizations/", "")}, row("GET", "/v1/organizations/", ""), true},
		{"another method", []Allow{row("GET", "/v1/organizations/", "")}, row("POST", "/v1/organizations/", ""), false},
		{"the method is compared as written", []Allow{row("GET", "/v1/", "")}, row("get", "/v1/", ""), false},
		{"a prefix past the old one at a slash", []Allow{row("GET", "/storage-limit", "")}, row("GET", "/storage-limit/7", ""), true},
		{"a prefix beside the old one", []Allow{row("GET", "/storage-limit", "")}, row("GET", "/storage-limitX", ""), false},
		{"a prefix short of the old one", []Allow{row("GET", "/storage-limit/", "")}, row("GET", "/storage-limit", ""), false},
		{"past an old prefix that ends with a slash", []Allow{row("GET", "/v1/", "")}, row("GET", "/v1/organizations/", ""), true},
		{"an old row with no host covers a row with one", []Allow{row("GET", "/v1/", "")}, row("GET", "/v1/", "api.github.com"), true},
		{"an old row's host covers a row of that host", []Allow{row("GET", "/v1/", "api.github.com")}, row("GET", "/v1/", "API.GitHub.com"), true},
		{"an old row's host does not cover a row with no host", []Allow{row("GET", "/v1/", "api.github.com")}, row("GET", "/v1/", ""), false},
		{"an old row's host does not cover a row of another host", []Allow{row("GET", "/v1/", "api.github.com")}, row("GET", "/v1/", "uploads.github.com"), false},
		{"any row of old may cover it", []Allow{row("PUT", "/a/", ""), row("GET", "/b/", "")}, row("GET", "/b/c", ""), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Covers(c.old, c.row); got != c.want {
				t.Errorf("Covers(%v, %v) = %v, want %v", c.old, c.row, got, c.want)
			}
		})
	}

	// The anthropic kind's profile rows held to a kind that allows GET alone: the rows outside
	// it are POST and DELETE, in the order the profile names them.
	getOnly := []Allow{{Method: "GET", PathPrefix: "/v1/organizations/"}}
	if got, want := Outside(getOnly, anthropicRows()), anthropicRows()[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("Outside(GET-only, the three rows) = %v, want %v", got, want)
	}
	if got := Outside(anthropicRows(), anthropicRows()); got != nil {
		t.Errorf("Outside(rows, rows) = %v, want nil", got)
	}
	if got := Outside(nil, anthropicRows()); got != nil {
		t.Errorf("Outside(nil, rows) = %v, want nil", got)
	}
}

// anthropicCases are check k47's calls of the anthropic kind, for its two secrets: K, with no
// profile and so no allow of its own, and N, whose profile's allow is the kind's three rows.
// Each call carries the placeholder as a Bearer token in Authorization. The comparison fuzz test
// sends each to the Lean model too.
func anthropicCases() []wireCase {
	var cases []wireCase
	for _, b := range []Binding{
		{Name: "K", Kind: "anthropic", Way: WayMint},
		{Name: "N", Kind: "anthropic", Way: WayMint, Allow: anthropicRows()},
	} {
		for _, call := range anthropicCalls {
			r := &Request{ConnectHost: "api.anthropic.com", ConnectPort: 443, SNI: "api.anthropic.com", HostHeader: "api.anthropic.com",
				Method: call.method, Path: call.path, RawPath: call.path, Headers: [][2]string{{"Authorization", "Bearer " + anthropicPlaceholder}}, Body: "{}"}
			cases = append(cases, wireCase{
				Rules:   rulesOf([]Kind{anthropicKind()}, b),
				Grant:   Grant{Secret: b.Name, Placeholder: anthropicPlaceholder},
				Value:   anthropicToken,
				Request: r,
			})
		}
	}
	return cases
}

const (
	anthropicPlaceholder = "agent-secrets-placeholder-k47aaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	anthropicToken       = "sk-ant-oat01-the-exchange-token"
)

// anthropicCalls are k47's (a), (b), (c) and (d): the three Admin API calls the kind allows, and
// the token exchange, the Messages API and a method the kind does not allow, which it refuses.
var anthropicCalls = []struct {
	method, path string
	want         string
}{
	{"GET", "/v1/organizations/workspaces", ""},
	{"POST", "/v1/organizations/workspaces", ""},
	{"DELETE", "/v1/organizations/workspaces/w1", ""},
	{"POST", "/v1/oauth/token", "not allowed by rule"},
	{"POST", "/v1/messages", "not allowed by rule"},
	{"PUT", "/v1/organizations/workspaces/w1", "not allowed by rule"},
}

// TestTheAnthropicKind is check k47's decision: the anthropic kind allows GET, POST and DELETE
// under /v1/organizations/ on api.anthropic.com alone, and its token goes into Authorization in
// place of the placeholder. For K, which has no profile, each refusal is the kind's allow alone:
// with that allow taken away, the same calls are carried.
func TestTheAnthropicKind(t *testing.T) {
	k := anthropicKind()
	if err := CheckKind(k); err != nil {
		t.Fatalf("CheckKind: %v", err)
	}
	for i, c := range anthropicCases() {
		call := anthropicCalls[i%len(anthropicCalls)]
		t.Run(c.Grant.Secret+" "+call.method+" "+call.path, func(t *testing.T) {
			if err := Validate(c.Rules); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			why := decided(t, c.Rules, c.Grant, c.Value, c.Request)
			if why != call.want {
				t.Fatalf("Decide = %q, want %q", why, call.want)
			}
			if call.want != "" {
				return
			}
			out := Rewrite(c.Rules, c.Grant, c.Value, c.Request)
			if want := [][2]string{{"Authorization", "Bearer " + anthropicToken}}; !reflect.DeepEqual(out.Headers, want) {
				t.Errorf("headers = %q, want %q", out.Headers, want)
			}
		})
	}
	t.Run("K's refusals are the kind's allow alone", func(t *testing.T) {
		for i, c := range anthropicCases()[:len(anthropicCalls)] {
			call := anthropicCalls[i]
			if call.want == "" {
				continue
			}
			c.Rules.Kinds[0].Allow = nil
			if why := decided(t, c.Rules, c.Grant, c.Value, c.Request); why != "" {
				t.Errorf("%s %s with no allow on the kind: Decide = %q, want it carried, so the refusal above was the kind's allow", call.method, call.path, why)
			}
		}
	})
	t.Run("another host of Anthropic's is not bound", func(t *testing.T) {
		c := anthropicCases()[0]
		c.Request.ConnectHost, c.Request.SNI, c.Request.HostHeader = "console.anthropic.com", "console.anthropic.com", "console.anthropic.com"
		if why := decided(t, c.Rules, c.Grant, c.Value, c.Request); why != "host not bound" {
			t.Errorf("Decide = %q, want %q", why, "host not bound")
		}
	})
}
