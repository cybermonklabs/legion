package proxyrules

import (
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
)

// validRules sets every field of an entry, leaves every optional field out of the second entry,
// and gives the third empty lists where the second has none, so the nil defaults are checked
// beside the empty lists. The profiles file is what the broker reads rules from, and its tests
// (policy's) hold how a file is read into these entries.
var validRules = Rules{Secrets: []SecretRule{
	{
		Name:    "github",
		Hosts:   []string{"api.github.com", "*.githubusercontent.com"},
		Headers: []string{"authorization", "x-github-token"},
		Allow:   []Allow{{Method: "GET", PathPrefix: "/repos/"}},
		Kind:    "azure",
		Judge:   new(uint32(900)),
	},
	{Name: "plain", Hosts: []string{"example.com"}},
	{Name: "sealed", Hosts: []string{"sealed.example.com"}, Headers: []string{}, Allow: []Allow{}},
}}

func TestValidate(t *testing.T) {
	if err := Validate(validRules); err != nil {
		t.Fatalf("Validate of valid rules: %v", err)
	}

	cases := []struct {
		name  string
		rules Rules
		want  []string // every piece the error names
	}{
		{
			name: "repeated name",
			rules: Rules{Secrets: []SecretRule{
				{Name: "github", Hosts: []string{"a.example.com"}},
				{Name: "github", Hosts: []string{"b.example.com"}},
			}},
			want: []string{`secrets[1]`, `"github"`, "repeated"},
		},
		{
			name: "judge above 900 seconds",
			rules: Rules{Secrets: []SecretRule{
				{Name: "github", Hosts: []string{"a.example.com"}, Judge: new(uint32(901))},
			}},
			want: []string{`secrets[0]`, `"github"`, "901", "900"},
		},
		{
			name: "unknown kind",
			rules: Rules{Secrets: []SecretRule{
				{Name: "github", Hosts: []string{"a.example.com"}, Kind: "gcp"},
			}},
			want: []string{`secrets[0]`, `"github"`, `"gcp"`},
		},
		{
			name: "the first failure of an entry wins",
			rules: Rules{Secrets: []SecretRule{
				{Name: "github", Kind: "gcp", Judge: new(uint32(901))},
			}},
			want: []string{`secrets[0]`, "901"},
		},
		{
			name: "a host written as a URL",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{"https://slack.com"}},
			}},
			want: []string{`secrets[0]`, `"slack"`, `"https://slack.com"`, "binds the secret nowhere"},
		},
		{
			name: "a bare star, which no lookup can ever reach",
			rules: Rules{Secrets: []SecretRule{
				{Name: "gcloud", Hosts: []string{"*"}},
			}},
			want: []string{`secrets[0]`, `"gcloud"`, `"*"`, "binds the secret nowhere"},
		},
		{
			name: "a host with a path",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{"slack.com/"}},
			}},
			want: []string{`secrets[0]`, `"slack.com/"`, "binds the secret nowhere"},
		},
		{
			name: "a host with a port, which the request's name never carries",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{"slack.com:443"}},
			}},
			want: []string{`secrets[0]`, `"slack.com:443"`, "binds the secret nowhere"},
		},
		{
			name: "an empty label",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{".slack.com"}},
			}},
			want: []string{`secrets[0]`, `".slack.com"`, "binds the secret nowhere"},
		},
		{
			name: "one bad host among good ones",
			rules: Rules{Secrets: []SecretRule{
				{Name: "gcloud", Hosts: []string{"oauth2.googleapis.com", "*"}},
			}},
			want: []string{`secrets[0]`, `"*"`, "binds the secret nowhere"},
		},
		{
			name: "an IPv4 address, which no TLS client names in its SNI",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{"93.184.216.34"}},
			}},
			want: []string{`secrets[0]`, `"93.184.216.34"`, "binds the secret nowhere"},
		},
		{
			name: "a suffix only an address ends in",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{"*.216.34"}},
			}},
			want: []string{`secrets[0]`, `"*.216.34"`, "binds the secret nowhere"},
		},
		{
			name: "a repeated name wins over the entry's own fault",
			rules: Rules{Secrets: []SecretRule{
				{Name: "github", Hosts: []string{"a.example.com"}},
				{Name: "github", Kind: "gcp"},
			}},
			want: []string{`secrets[1]`, "repeated"},
		},
		{
			// An entry is a proxy block, which binds its value to the hosts it names: one that
			// names none would hold a value back and carry it nowhere.
			name: "an entry with no host",
			rules: Rules{Secrets: []SecretRule{
				{Name: "slack", Hosts: []string{"slack.com"}},
				{Name: "release", Judge: new(uint32(600)), Guidance: "grant for releases"},
			}},
			want: []string{`secrets[1]`, `"release"`, "the entry names no host"},
		},
		{
			name: "an entry with an empty host list",
			rules: Rules{Secrets: []SecretRule{
				{Name: "a", Hosts: []string{}},
			}},
			want: []string{`secrets[0]`, `"a"`, "the entry names no host"},
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
		{"judge at the limit", Rules{Secrets: []SecretRule{{Name: "a", Hosts: []string{"h"}, Judge: new(uint32(900))}}}},
		{"kind aws", Rules{Secrets: []SecretRule{{Name: "a", Hosts: []string{"h"}, Kind: "aws"}}}},
		{"kind azure", Rules{Secrets: []SecretRule{{Name: "a", Hosts: []string{"h"}, Kind: "azure"}}}},
		{"no secret at all", Rules{}},
		{"a name", Rules{Secrets: []SecretRule{{Name: "a", Hosts: []string{"slack.com"}}}}},
		{"a suffix", Rules{Secrets: []SecretRule{{Name: "a", Hosts: []string{"*.googleapis.com"}}}}},
		{"a name with digits in all but its last label", Rules{Secrets: []SecretRule{{Name: "a", Hosts: []string{"1e100.net", "*.0.example.com"}}}}},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate(c.rules); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

// TestValidateTakesTheDPoPKind: an entry of kind dpop, a capability the broker mints for a grant
// of its own and holds, is a valid file, and a kind that is only near it is refused. The kind is
// compared as it is written, as the other three are, so "DPoP" is no kind; each refusal names
// the kind the file wrote and the four kinds there are, so its reader sees what to write.
func TestValidateTakesTheDPoPKind(t *testing.T) {
	entry := func(kind string) Rules {
		return Rules{Secrets: []SecretRule{{Name: "TEST_MINTED_TOKEN", Hosts: []string{"dispatch.test"}, Headers: []string{"Authorization"},
			Allow: []Allow{{Method: "POST", PathPrefix: "/api/v1/projects/"}}, Kind: kind}}}
	}
	if err := Validate(entry("dpop")); err != nil {
		t.Errorf("Validate of an entry of kind dpop: %v", err)
	}
	for _, kind := range []string{"DPoP", "dpo"} {
		t.Run(kind, func(t *testing.T) {
			err := Validate(entry(kind))
			if err == nil {
				t.Fatalf("Validate of an entry of kind %q: no error", kind)
			}
			for _, want := range []string{`secrets[0]`, `"TEST_MINTED_TOKEN"`, fmt.Sprintf("kind %q", kind), `"", "aws", "azure" or "dpop"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

// TestHostShapeOK holds HostShapeOK to what Decide carries a value on. Each row is read against
// the request a TLS client makes to one host: that host as the CONNECT target and the Host
// header, and the name the client sends in its SNI, which for an address is none (RFC 6066
// section 3; Go's crypto/tls decides it with net.ParseIP). A refused entry is read against the
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
		// The request host to read the entry against: one Decide carries the value to by it
		// when the shape is accepted, and the name the entry meant to bind when it is refused.
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
			rules := Rules{Secrets: []SecretRule{{Name: "slack", Hosts: []string{c.host}}}}
			grant := Grant{Secret: "slack", Placeholder: "agent-secrets-placeholder-slack"}
			r := &Request{
				ConnectHost: c.request, ConnectPort: 443, SNI: sni, HostHeader: c.request,
				Method: "GET", Path: "/", Headers: [][2]string{{"Authorization", "Bearer " + grant.Placeholder}},
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

// decideRules, decideGrant, decideValue and decideRequest are an allowed call; each case
// below breaks exactly one thing about it.
func decideRules() Rules {
	return Rules{Secrets: []SecretRule{{
		Name:  "s",
		Hosts: []string{"api.example.com"},
		Allow: []Allow{{Method: "GET", PathPrefix: "/v1/"}},
	}}}
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
		Headers:     [][2]string{{"Authorization", "Bearer PH"}},
		Query:       "page=2",
		Body:        "{}",
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Rules, *Grant, *string, *Request)
		want string // the refusal, or "" for an allowed request
	}{
		{"allowed", func(*Rules, *Grant, *string, *Request) {}, ""},
		{
			// The empty placeholder is read before the secret is looked up.
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
			"secret not behind airlock",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) { rules.Secrets[0].Hosts = nil },
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
			func(_ *Rules, _ *Grant, _ *string, r *Request) { r.Path = "/v2/things" },
			"not allowed by rule",
		},
		{
			// A nil allow list allows anything; an empty one allows nothing.
			"an empty allow list allows nothing",
			func(rules *Rules, _ *Grant, _ *string, _ *Request) { rules.Secrets[0].Allow = []Allow{} },
			"not allowed by rule",
		},
		{
			// The override header is read below the allow list: a method the list refuses
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
			// the header the entry's allow list cannot bound.
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
				rules.Secrets[0].Allow = nil
				r.Method, r.Path = "DELETE", "/anything"
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
			// The check reads every header, not only the ones the entry names.
			"the value in a header the entry does not name",
			func(_ *Rules, _ *Grant, _ *string, r *Request) {
				r.Headers = append(r.Headers, [2]string{"X-Other", decideValue})
			},
			"request already carries the value",
		},
		{
			// An entry with a kind is not exempt from that refusal.
			"the value carried for an entry with a kind",
			func(rules *Rules, _ *Grant, _ *string, r *Request) {
				rules.Secrets[0].Kind = "aws"
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules, g, value, r := decideRules(), decideGrant(), decideValue, decideRequest()
			c.edit(&rules, &g, &value, r)

			ok, why := Decide(rules, g, value, r)
			if why != c.want {
				t.Errorf("Decide reason = %q, want %q", why, c.want)
			}
			if ok != (c.want == "") {
				t.Errorf("Decide ok = %v, want %v", ok, c.want == "")
			}
			if got := Rewrite(rules, g, value, r); (got == nil) == ok {
				t.Errorf("Rewrite = %v, but Decide ok = %v", got, ok)
			}
		})
	}
}

// TestTheConnectPortAndTheDotSegmentRefusals covers the two refusals that are about the
// request rather than about the entry, and the near misses on each side of the line each of
// them draws: the ports a CONNECT may name beside 443, the segments a reader resolves
// against the ones that only look like them, and a path with no segment at all.
func TestTheConnectPortAndTheDotSegmentRefusals(t *testing.T) {
	// decide runs one request through Decide and Rewrite and answers the refusal, "" for an
	// allowed one. anyPath drops the entry's allow list, which is what isolates the
	// dot-segment refusal: with no allow list the only thing left to refuse a path is that.
	decide := func(t *testing.T, anyPath bool, port uint16, path string) string {
		t.Helper()
		rules, r := decideRules(), decideRequest()
		if anyPath {
			rules.Secrets[0].Allow = nil
		}
		r.ConnectPort, r.Path = port, path
		ok, why := Decide(rules, decideGrant(), decideValue, r)
		if ok != (why == "") {
			t.Errorf("Decide ok = %v with reason %q", ok, why)
		}
		if got := Rewrite(rules, decideGrant(), decideValue, r); (got == nil) == ok {
			t.Errorf("Rewrite = %v, but Decide ok = %v", got, ok)
		}
		return why
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

	t.Run("a dot segment, whatever the entry allows", func(t *testing.T) {
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
				// With no allow list the entry allows any path, so a refusal here is the
				// dot-segment one and nothing else: it is unconditional.
				if got := decide(t, true, 443, c.path); got != c.want {
					t.Errorf("Decide on %q with no allow list = %q, want %q", c.path, got, c.want)
				}
			})
		}
	})

	t.Run("a dot segment is read before the allow list", func(t *testing.T) {
		// The first path is inside the entry's "/v1/" prefix as it is written and outside it
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

// TestTheMethodOverrideRefusal covers the refusal of a header a service reads the method
// from: each of the three names, in the cases a client may send them in, the near misses
// the fold and the whole-name comparison draw a line at, and the one shape that is not
// refused at all, an entry with no allow list, which allows every method already.
func TestTheMethodOverrideRefusal(t *testing.T) {
	// decide runs one request through Decide and Rewrite and answers the refusal, "" for an
	// allowed one. anyMethod drops the entry's allow list, which is what the refusal is
	// conditional on.
	decide := func(t *testing.T, anyMethod bool, headers [][2]string) string {
		t.Helper()
		rules, r := decideRules(), decideRequest()
		if anyMethod {
			rules.Secrets[0].Allow = nil
		}
		r.Headers = append(r.Headers, headers...)
		ok, why := Decide(rules, decideGrant(), decideValue, r)
		if ok != (why == "") {
			t.Errorf("Decide ok = %v with reason %q", ok, why)
		}
		if got := Rewrite(rules, decideGrant(), decideValue, r); (got == nil) == ok {
			t.Errorf("Rewrite = %v, but Decide ok = %v", got, ok)
		}
		return why
	}

	t.Run("under an entry with an allow list", func(t *testing.T) {
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
			{"a header the entry has nothing to say about", [2]string{"X-Request-Id", "7"}, ""},
		} {
			t.Run(c.name, func(t *testing.T) {
				if got := decide(t, false, [][2]string{c.header}); got != c.want {
					t.Errorf("Decide with %q = %q, want %q", c.header, got, c.want)
				}
			})
		}
	})

	t.Run("a request carrying none of them passes under the same entry", func(t *testing.T) {
		if got := decide(t, false, nil); got != "" {
			t.Errorf("Decide = %q, want the request allowed", got)
		}
	})

	t.Run("an entry with no allow list is unaffected", func(t *testing.T) {
		// Such an entry allows every method already, so a header naming another takes
		// nothing away from it and is not refused.
		for _, h := range [][2]string{
			{"X-HTTP-Method-Override", "DELETE"},
			{"x-http-method", "DELETE"},
			{"x-method-override", "DELETE"},
		} {
			t.Run(h[0], func(t *testing.T) {
				if got := decide(t, true, [][2]string{h}); got != "" {
					t.Errorf("Decide with %q and no allow list = %q, want the request allowed", h, got)
				}
			})
		}
	})

	t.Run("an empty allow list is refused by the list itself", func(t *testing.T) {
		// An empty list allows nothing, which is read first, so the header is never the
		// reason such a request is refused.
		rules, r := decideRules(), decideRequest()
		rules.Secrets[0].Allow = []Allow{}
		r.Headers = append(r.Headers, [2]string{"X-HTTP-Method-Override", "DELETE"})
		if _, why := Decide(rules, decideGrant(), decideValue, r); why != "not allowed by rule" {
			t.Errorf("Decide = %q, want %q", why, "not allowed by rule")
		}
	})
}

// rewritten runs Rewrite over one entry and one set of headers, and answers the headers it
// gave back. It fails the test when Rewrite refuses, or when it changed the request it read.
func rewritten(t *testing.T, sr SecretRule, headers [][2]string) [][2]string {
	t.Helper()
	sr.Name = "s"
	if sr.Hosts == nil {
		sr.Hosts = []string{"api.example.com"}
	}
	rules := Rules{Secrets: []SecretRule{sr}}
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
		out.Method != r.Method || out.Path != r.Path {
		t.Errorf("Rewrite changed the request's route: %+v, want %+v", out, r)
	}
	if len(out.Headers) != len(headers) {
		t.Fatalf("Rewrite gave %d headers, want %d", len(out.Headers), len(headers))
	}
	for i, h := range out.Headers {
		if h[0] != headers[i][0] {
			t.Errorf("header %d is named %q, want %q", i, h[0], headers[i][0])
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

func TestRewrite(t *testing.T) {
	t.Run("the named header, and no other", func(t *testing.T) {
		headers := rewritten(t, SecretRule{}, [][2]string{
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

	t.Run("the entry's header names are matched without case", func(t *testing.T) {
		headers := rewritten(t, SecretRule{Headers: []string{"X-TOKEN"}}, [][2]string{
			{"x-token", "PH"},
			{"Authorization", "Bearer PH"},
		})
		if headers[0][1] != decideValue {
			t.Errorf("x-token = %q, want %q", headers[0][1], decideValue)
		}
		if headers[1][1] != "Bearer PH" {
			t.Errorf("authorization = %q, want it untouched: the entry names x-token alone", headers[1][1])
		}
	})

	t.Run("an entry that names no header swaps nothing", func(t *testing.T) {
		headers := rewritten(t, SecretRule{Headers: []string{}}, [][2]string{{"Authorization", "Bearer PH"}})
		if headers[0][1] != "Bearer PH" {
			t.Errorf("authorization = %q, want it untouched", headers[0][1])
		}
	})

	t.Run("several occurrences in one value", func(t *testing.T) {
		headers := rewritten(t, SecretRule{Headers: []string{"x-token"}}, [][2]string{
			{"x-token", "PH, PH and PH"},
		})
		want := decideValue + ", " + decideValue + " and " + decideValue
		if headers[0][1] != want {
			t.Errorf("x-token = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("inside a Basic credential, as the user", func(t *testing.T) {
		headers := rewritten(t, SecretRule{}, [][2]string{{"Authorization", basic("PH:x-oauth-basic")}})
		if want := basic(decideValue + ":x-oauth-basic"); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("inside a Basic credential, as the password", func(t *testing.T) {
		headers := rewritten(t, SecretRule{}, [][2]string{{"Authorization", basic("user:PH")}})
		if want := basic("user:" + decideValue); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
		}
	})

	t.Run("spaces and tabs around a Basic credential are trimmed", func(t *testing.T) {
		headers := rewritten(t, SecretRule{}, [][2]string{
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
		headers := rewritten(t, SecretRule{}, [][2]string{{"Authorization", in}})
		if headers[0][1] != in {
			t.Errorf("authorization = %q, want it untouched: %q", headers[0][1], in)
		}
	})

	t.Run("a Basic payload that is not valid UTF-8 takes the plain replacement", func(t *testing.T) {
		// The payload holds the placeholder, but it is not valid UTF-8, so it is no Basic
		// credential: the plain replacement runs over the header value itself, which holds
		// no placeholder, and the value is left as it came.
		in := "Basic " + base64.StdEncoding.EncodeToString([]byte{0xff, 'P', 'H'})
		headers := rewritten(t, SecretRule{}, [][2]string{{"Authorization", in}})
		if headers[0][1] != in {
			t.Errorf("authorization = %q, want it untouched: %q", headers[0][1], in)
		}
	})

	t.Run("a Basic credential without the placeholder takes the plain replacement", func(t *testing.T) {
		in := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:other")) + " PH"
		headers := rewritten(t, SecretRule{}, [][2]string{{"Authorization", in}})
		if want := strings.Replace(in, "PH", decideValue, 1); headers[0][1] != want {
			t.Errorf("authorization = %q, want %q", headers[0][1], want)
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

// TestGuidance: the judge's guidance is the entry's own text, "" for an entry that gives none and
// for a name the file does not hold.
func TestGuidance(t *testing.T) {
	rules := Rules{Secrets: []SecretRule{
		{Name: "guided", Hosts: []string{"api.example.com"}, Judge: new(uint32(300)), Guidance: "grant for releases"},
		{Name: "unguided", Hosts: []string{"api.example.com"}, Judge: new(uint32(300))},
	}}
	for name, want := range map[string]string{"guided": "grant for releases", "unguided": "", "absent": ""} {
		if got := rules.Guidance(name); got != want {
			t.Errorf("Guidance(%q) = %q; want %q", name, got, want)
		}
	}
}

// TestJudgeSeconds pins the one reader of the judge's bound: an entry that names a judge answers
// its seconds, an entry that names none answers no bound rather than zero seconds of one, and so
// does a name the file does not hold at all.
func TestJudgeSeconds(t *testing.T) {
	judge := uint32(300)
	rules := Rules{Secrets: []SecretRule{
		{Name: "judged", Hosts: []string{"api.example.com"}, Judge: &judge},
		{Name: "unjudged", Hosts: []string{"api.example.com"}},
	}}
	for _, c := range []struct {
		name    string
		seconds uint32
		ok      bool
	}{
		{"judged", 300, true},
		{"unjudged", 0, false},
		{"absent", 0, false},
	} {
		seconds, ok := rules.JudgeSeconds(c.name)
		if seconds != c.seconds || ok != c.ok {
			t.Errorf("JudgeSeconds(%q) = %d, %v; want %d, %v", c.name, seconds, ok, c.seconds, c.ok)
		}
	}
}
