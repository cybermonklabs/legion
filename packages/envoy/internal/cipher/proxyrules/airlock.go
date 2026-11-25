// Package proxyrules holds the airlock's rules and the two answers the broker's proxy port
// takes from them: whether a request may carry a secret, and the request with the grant's
// placeholder swapped for the secret's real value in the headers its kind names. The rules are
// the profiles file's kinds - for each, the hosts a credential of the kind may go to, the
// headers it goes into and the methods and paths it may travel on - and one binding for each
// name the policy serves behind the proxy: the kind its secret's proxy tag names, or a
// transitional inline rule, and its profile's own allow, which narrows that one name and
// never widens its kind.
//
// The package holds no HTTP and no database code, because a Lean model of exactly these
// functions is proved and a differential fuzz test drives the Go functions against the
// model. The refusal strings, the order they are reported in, and the string handling are
// therefore the contract with that model, not style: a change here is a change there. Each
// string rule below is narrower than the standard library's nearest function, and says why.
//
// Its one case fold, of a host name, a header name and the Basic scheme alike, is fold.ASCII,
// the fold the model makes: A-Z lowered and nothing else. The Unicode folding of
// strings.ToLower and strings.EqualFold would fold U+212A KELVIN SIGN onto "k", so
// "githu\u212a.com" would reach the rule for "githuk.com". The proxy port
// (internal/cipher/proxy) folds the CONNECT host, the SNI and the Host header with the
// same function before the decision compares them, since host matching is sound only while the
// two folds are one.
package proxyrules

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/fold"
)

// Allow is one method and path prefix a credential may travel on. A kind's row holds no host,
// and binds the method and prefix on every host of its kind. A profile's row may hold Host, one
// of its kind's hosts, and then allows the request on that host alone.
type Allow struct {
	Method     string `json:"method"`
	PathPrefix string `json:"pathPrefix"`
	Host       string `json:"host,omitempty"`
}

// Kind is one kind of the profiles file's kinds part: where a credential of the kind may go and
// in which headers, for every secret whose proxy tag names it. Name is one lowercase word.
// Variables, Ways, PlaceholderPrefix and MaxGrant are no part of the decision: the policy load
// reads Ways and MaxGrant (CheckKind), the release reads Variables and PlaceholderPrefix.
type Kind struct {
	Name      string   `json:"name"`
	Hosts     []string `json:"hosts"`
	Headers   []string `json:"headers"` // nil: the default ["authorization"]; empty: no header
	Variables []string `json:"variables"`
	Ways      []string `json:"ways"`
	Allow     []Allow  `json:"allow"` // nil: any method and path; empty: nothing allowed
	// PlaceholderPrefix opens each placeholder of the kind in place of the default prefix, for a
	// client that checks a key's shape before it sends it.
	PlaceholderPrefix string `json:"placeholder_prefix,omitempty"`
	// MaxGrant is a Go duration that caps the max-grant tag of each secret of the kind.
	MaxGrant string `json:"max_grant,omitempty"`
}

// HeaderNames is the headers a credential of this kind travels in: the kind's own list, or the
// one default header when the kind names none ("headers" absent). A kind whose list is empty
// names no header at all, and this answers that empty list.
//
// It is the one place the default lives. Rewrite swaps in these headers and nothing else, and
// `envoy-broker profiles check` reports what a file adds and removes against them, so the report
// says what the swap does rather than what a second copy of the default says it does. The slice
// a kind names points into that kind; the default is a fresh slice.
func (k Kind) HeaderNames() []string {
	if k.Headers == nil {
		return []string{"authorization"}
	}
	return k.Headers
}

// The ways a credential of a kind can go into a call (Kind.Ways, Binding.Way). The secret's
// type picks its way (policy's WayOf): no type swaps a stored value in, and an AWS key pair of
// the aws kind is signed with again; github-app, github-installations, google-login,
// google-service-account and anthropic-federation mint a token and swap it in; azure-login
// exchanges at Microsoft's token endpoint; and a capability the broker mints for a grant of its
// own and holds is swapped in with a DPoP proof. A way joins these when a path of the proxy takes
// it, and not before: a kind that lists a way no path takes would say its credential goes in a
// way it never goes, so CheckKind refuses one.
const (
	WaySwap     = "swap"
	WayMint     = "mint"
	WaySign     = "sign"
	WayExchange = "exchange"
	WayDPoP     = "dpop"
)

// Ways is every way a kind may list, in the order a refusal names them.
var Ways = []string{WaySwap, WayMint, WaySign, WayExchange, WayDPoP}

// Binding is one name behind the proxy: the kind its proxy tag (or its profile's transitional
// proxy string) names, the way its secret's type takes into a call, and its profile's own allow,
// which narrows the kind's and never widens it. Inline is the rule of a transitional proxy block
// of kind dpop, a capability the broker mints and holds whose host no kind declares; a binding
// names a kind or holds an inline rule, never both. Inline goes when a kind with the dpop way
// replaces the profiles file's dpop blocks: each such capability's grant then names that kind,
// and no binding holds a rule of its own.
type Binding struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Way    string  `json:"way"`
	Allow  []Allow `json:"allow"` // nil: no allow of the profile's own; empty: nothing allowed
	Inline *Kind   `json:"inline,omitempty"`
}

// Rules is what the airlock decides from: the profiles file's kinds, and one binding for each
// name the policy serves behind the proxy. The kinds are the file's; the bindings are the
// policy load's, since a secret is behind the proxy when its proxy tag names a kind.
type Rules struct {
	Kinds []Kind    `json:"kinds"`
	Names []Binding `json:"names"`
}

// Grant names the secret a connection carries and the placeholder the command holds for it.
type Grant struct {
	Secret      string `json:"secret"`
	Placeholder string `json:"placeholder"`
}

// Request is one request on the proxy port, as much of it as the decision and the swap read.
type Request struct {
	ConnectHost string `json:"connectHost"`
	// ConnectPort is the port the CONNECT named. Only 443 may carry a value: the three
	// names the decision compares carry no port, so 443 is the only port the rules speak
	// about, and another port of one name is another service.
	ConnectPort uint16 `json:"connectPort"`
	SNI         string `json:"sni"`
	HostHeader  string `json:"hostHeader"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	// RawPath is the path as the command sent it, percent-encoding kept (URL.EscapedPath):
	// the one form that tells an encoded "/" or "\" from a "/".
	RawPath string      `json:"rawPath"`
	Headers [][2]string `json:"headers"`
	Query   string      `json:"query"`
	Body    string      `json:"body"`
}

// CipherRepositoryHeader is the request header a command names a repository in, for the
// broker to mint a GitHub token for that one repository. The broker reads it nowhere yet, and
// a command may send it all the same, so the proxy removes it from every request it reads
// (Rewrite, and the sign path of internal/cipher/proxy, which does not go through Rewrite)
// before it forwards it: no service ever sees it. It is held ASCII-lower-cased, which is how
// every header name in this package is compared.
const CipherRepositoryHeader = "cipher-repository"

// kindNameOK reports whether a kind's name is one lowercase word: a-z alone, never empty.
func kindNameOK(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		if name[i] < 'a' || name[i] > 'z' {
			return false
		}
	}
	return true
}

// Validate reports why the rules are not valid, naming the kind or the binding by its name and
// its index. It reads the kinds in order, then the bindings, and reports the first failure: a
// repeated kind name, a kind name that is not one lowercase word, a kind that names no host, or
// a host that binds the kind nowhere (HostShapeOK); a repeated binding name, a binding that names
// both a kind and an inline rule, a kind the rules do not declare, or an inline rule that names no
// host or a host that binds nowhere. A kind binds its credential to at least one host, and a kind
// of no host would hold every value of the kind back and carry it nowhere, so it is refused here
// (theorem T4 of the model) rather than left to Decide, which still refuses every proxy request
// for one ("secret not behind airlock"). Whether it answers nil is the same answer as the Lean
// model's validate, so a new refusal here is a new refusal there.
func Validate(rules Rules) error {
	kinds := make(map[string]bool, len(rules.Kinds))
	for i, k := range rules.Kinds {
		at := fmt.Sprintf("kinds[%d] %q", i, k.Name)
		switch {
		case kinds[k.Name]:
			return fmt.Errorf("%s: the name is repeated", at)
		case !kindNameOK(k.Name):
			return fmt.Errorf("%s: the name is not one lowercase word", at)
		}
		if err := hostsOK(at, k.Hosts); err != nil {
			return err
		}
		kinds[k.Name] = true
	}
	names := make(map[string]bool, len(rules.Names))
	for i, b := range rules.Names {
		at := fmt.Sprintf("names[%d] %q", i, b.Name)
		switch {
		case names[b.Name]:
			return fmt.Errorf("%s: the name is repeated", at)
		case b.Inline != nil && b.Kind != "":
			return fmt.Errorf("%s: the binding names the kind %q and holds an inline rule", at, b.Kind)
		case b.Inline == nil && !kinds[b.Kind]:
			return fmt.Errorf("%s: the kind %q is no kind of the rules", at, b.Kind)
		}
		if b.Inline != nil {
			if err := hostsOK(at, b.Inline.Hosts); err != nil {
				return err
			}
		}
		names[b.Name] = true
	}
	return nil
}

// hostsOK holds one kind's hosts, or one inline rule's, to Validate's rule: at least one, each a
// host HostShapeOK accepts. A host it refuses binds the credential nowhere although the kind
// names it to bind one: the command gets its placeholder and no call of it is ever swapped.
func hostsOK(at string, hosts []string) error {
	if len(hosts) == 0 {
		return fmt.Errorf("%s: names no host", at)
	}
	for _, h := range hosts {
		if !HostShapeOK(h) {
			return fmt.Errorf("%s: host %q is not a DNS name or a %q suffix, so the airlock binds the credential nowhere", at, h, "*.")
		}
	}
	return nil
}

// CheckKind holds the fields of one kind that the decision does not read to the policy load's
// rules: each way one of Ways, at least one, none twice; a kind row that names no host, since a
// kind's rows bind every host of the kind; a max_grant that is a Go duration of at least a
// second, where it is set; and each variable a shell name. It answers nil or the first refusal,
// without the kind's name, which its caller adds.
func CheckKind(k Kind) error {
	if len(k.Ways) == 0 {
		return fmt.Errorf("ways names no way; a kind takes at least one of %s", strings.Join(Ways, ", "))
	}
	seen := map[string]bool{}
	for _, w := range k.Ways {
		switch {
		case !slices.Contains(Ways, w):
			return fmt.Errorf("ways names %q, which is not one of %s", w, strings.Join(Ways, ", "))
		case seen[w]:
			return fmt.Errorf("ways names %q twice", w)
		}
		seen[w] = true
	}
	for _, a := range k.Allow {
		if a.Host != "" {
			return fmt.Errorf("allow row %s %s names the host %q: a kind's row binds every host of its kind, and only a profile's row names one", a.Method, a.PathPrefix, a.Host)
		}
	}
	if k.MaxGrant != "" {
		d, err := time.ParseDuration(k.MaxGrant)
		if err != nil || d < time.Second {
			return fmt.Errorf("max_grant %q is not a Go duration of at least a second", k.MaxGrant)
		}
	}
	for _, v := range k.Variables {
		if !shellName(v) {
			return fmt.Errorf("variables names %q, which is no shell variable name", v)
		}
	}
	return nil
}

// shellName reports whether v is a POSIX shell variable name: a letter or underscore, then
// letters, digits and underscores.
func shellName(v string) bool {
	if v == "" {
		return false
	}
	for i := range len(v) {
		c := v[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Lookup finds a name's binding, or nil. The binding points into r, so a caller that keeps it
// keeps the rules it came from.
func (r Rules) Lookup(name string) *Binding {
	for i := range r.Names {
		if r.Names[i].Name == name {
			return &r.Names[i]
		}
	}
	return nil
}

// Kind finds a kind of the rules by its name, or nil.
func (r Rules) Kind(name string) *Kind {
	for i := range r.Kinds {
		if r.Kinds[i].Name == name {
			return &r.Kinds[i]
		}
	}
	return nil
}

// KindOf is the kind a binding is decided under: its inline rule, or the kind it names; nil for
// a nil binding and for a kind the rules do not declare, which a valid file never holds.
func (r Rules) KindOf(b *Binding) *Kind {
	switch {
	case b == nil:
		return nil
	case b.Inline != nil:
		return b.Inline
	default:
		return r.Kind(b.Kind)
	}
}

// HostsOf is the hosts a name is bound to (its kind's), nil for a name the rules do not bind.
func (r Rules) HostsOf(name string) []string {
	if k := r.KindOf(r.Lookup(name)); k != nil {
		return k.Hosts
	}
	return nil
}

// HostBound reports whether host matches one of hosts: the same name, or a "*.suffix"
// pattern, which matches one or more labels before the suffix ("*.example.com" matches
// "a.example.com" and "a.b.example.com", not "example.com"). Both sides are compared after
// fold.ASCII, never strings.EqualFold.
func HostBound(hosts []string, host string) bool {
	bound := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		bound[fold.ASCII(h)] = true
	}
	h := fold.ASCII(host)
	if bound[h] {
		return true
	}
	for {
		i := strings.IndexByte(h, '.')
		if i < 0 {
			return false
		}
		h = h[i+1:]
		if bound["*."+h] {
			return true
		}
	}
}

// HostShapeOK reports whether a host of a kind or of an inline rule is one the airlock can bind a
// request to: a host name, which HostBound holds exactly, or "*." and one, a suffix it looks up.
// A label is letters, digits and hyphens, never empty and never starting or ending with a
// hyphen, and the last label is not all digits. Validate holds every host of the rules to it,
// and config holds the local stack's stand-in token authority
// (BROKER_AIRLOCK_TEST_AZURE_AUTHORITY) to it, since a token request reaches the proxy only at a
// host a kind or an inline rule binds.
//
// The host a request carries is a name alone: the CONNECT target's host, with its port taken
// off and compared against 443 on its own. So a host written with a scheme, a path, a port, or
// "*" without the "*." that HostBound looks up, matches no request ever.
//
// An IPv4 address matches the CONNECT host of a request to it, but check also holds the TLS
// name to that host, and a TLS client names no address there: RFC 6066 section 3 keeps a
// literal address out of server_name, and Go's crypto/tls and curl send none for one. So check
// refuses every request to such a host "sni mismatch", on a connection the proxy decrypts
// because the kind binds its host, where without the kind it would be a tunnel. A host name
// is told from a dotted-decimal address by its last label, which in a host name is never all
// digits (RFC 1123 section 2.1), and that is the test here: it refuses an IPv4 literal, "*."
// over a suffix only an address ends in, and any other host whose last label is all digits,
// which is no host name either. An IPv6 literal holds a ':' that no label takes.
func HostShapeOK(host string) bool {
	name := strings.TrimPrefix(host, "*.")
	if name == "" {
		return false
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != ""
}

// allowOK reports whether allow covers the request. A nil list allows any method and path;
// an empty list allows nothing. A row allows a request of its method whose path its prefix
// holds (prefixOK) and, for a row that names a host, whose CONNECT host is that host, compared
// after fold.ASCII as every host here is.
func allowOK(allow []Allow, r *Request) bool {
	if allow == nil {
		return true
	}
	for _, a := range allow {
		if a.Method == r.Method && (a.Host == "" || fold.ASCII(a.Host) == r.ConnectHost) && prefixOK(a.PathPrefix, r.Path) {
			return true
		}
	}
	return false
}

// prefixOK reports whether a path prefix holds path: the path is the prefix, or continues it at
// a "/" boundary, which is a prefix that ends with "/" or a path whose next character is "/".
// So "/storage-limit" holds "/storage-limit" and "/storage-limit/7" and not "/storage-limitX",
// which a plain starts-with would let through to another route.
func prefixOK(prefix, path string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || strings.HasSuffix(prefix, "/") || path[len(prefix)] == '/'
}

// Covers reports whether old covers row by the conservative cover rule, which may refuse a row
// old does cover and never accepts one it does not: an old list that is missing (nil) covers
// every row; otherwise a row of old of the same method covers it when the row's prefix equals the
// old prefix or continues it at a "/" boundary (prefixOK), and the old row names no host or the
// row's host. The policy load holds a profile's rows to its kind's allow by it, and `envoy-broker
// profiles check --against` holds each served name's rows under the new file to its rows under the
// old one.
func Covers(old []Allow, row Allow) bool {
	if old == nil {
		return true
	}
	for _, o := range old {
		if o.Method == row.Method && (o.Host == "" || fold.ASCII(o.Host) == fold.ASCII(row.Host)) && prefixOK(o.PathPrefix, row.PathPrefix) {
			return true
		}
	}
	return false
}

// Outside is each row of rows that old does not cover (Covers), in the order rows names them,
// nil when old covers every one.
func Outside(old, rows []Allow) []Allow {
	var out []Allow
	for _, row := range rows {
		if !Covers(old, row) {
			out = append(out, row)
		}
	}
	return out
}

// methodOverrideHeaders are the header names a service may read the request's method from
// instead of the method on the request line: X-HTTP-Method-Override is the one Google's APIs
// honour, and X-HTTP-Method and X-Method-Override are the two other spellings in the wild.
// They are held ASCII-lower-cased, which is how every header name in this package is compared.
var methodOverrideHeaders = []string{"x-http-method-override", "x-http-method", "x-method-override"}

// hasMethodOverride reports whether the request carries one of those headers, whatever its
// value: the value is the service's to read, so the airlock refuses the header rather than
// deciding which methods one of them could have named.
func hasMethodOverride(r *Request) bool {
	for _, h := range r.Headers {
		for _, n := range methodOverrideHeaders {
			if fold.ASCII(h[0]) == n {
				return true
			}
		}
	}
	return false
}

// methodOverrideParameter is the query parameter name a service may read the request's method
// from instead of the method on the request line: Laravel, Symfony and Express's method-override
// middleware read "_method".
const methodOverrideParameter = "_method"

// hasMethodOverrideParameter reports whether the query, as the command sent it, holds a parameter
// that a service reads as "_method", whatever its value or none. The query is split on "&", and a
// part's name is its text before its first "=", or the whole part when it has none. Each name is
// read as those services read it, since a name compared as it was sent would let
// "_meth%6Fd=DELETE" through to one that decodes it: it is decoded with url.QueryUnescape, which
// reads "+" as a space, as Express's querystring and PHP's $_GET do; its leading spaces are
// dropped, and ".", " " and "[" are read as "_", as PHP reads a variable name, so ".method" and
// "+_method" are "_method" too; and it is compared after fold.ASCII. A name that does not decode,
// a "%" with no two hex digits after it, is one no reader of the airlock's can say the service
// does not read as "_method", so it counts as one. As with the header, the value is the service's
// to read, so the airlock refuses the parameter rather than deciding which method it names.
//
// A "_method" field of a form body is not refused, and Rails, Laravel and Symfony honour one:
// the decision reads no body, which internal/cipher/proxy streams to the service unread.
func hasMethodOverrideParameter(query string) bool {
	for _, part := range strings.Split(query, "&") {
		raw, _, _ := strings.Cut(part, "=")
		name, err := url.QueryUnescape(raw)
		if err != nil {
			return true
		}
		read := []byte(strings.TrimLeft(name, " "))
		for i, c := range read {
			if c == '.' || c == ' ' || c == '[' {
				read[i] = '_'
			}
		}
		if fold.ASCII(string(read)) == methodOverrideParameter {
			return true
		}
	}
	return false
}

// hasDotSegment reports whether the path holds a segment that is "." or "..". The path is
// split on "/" and each segment is compared whole, so "/v1/../admin" and "/a/./b" hold one
// while "/v1/..foo" and "/v1/.../x" hold none.
//
// strings.Split is narrower than path.Clean or net/url's resolution, and deliberately: it
// decides nothing about what the path means, it only says whether it holds a segment a
// reader would resolve. It is also the one split the model can make the same way, because
// Go's strings.Split and Lean's String.splitOn answer alike on every input - the empty path
// is one empty segment on both sides, and a leading or trailing slash is an empty segment at
// that end on both - so the two sides refuse the same paths.
func hasDotSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// hasEmptySegment reports whether the path holds an empty segment: two slashes in a row. A
// reader that merges them reads "/a//b" as "/a/b" and one that does not reads another route, so
// under an allow list the airlock carries neither (check). A trailing slash is no empty segment
// here.
func hasEmptySegment(path string) bool {
	return strings.Contains(path, "//")
}

// hasEncodedSlash reports whether the path as the command sent it holds a percent-encoded "/",
// "%2F" or "%2f". The decoded path reads it as a separator and a service may read it as a byte of
// one segment, so an allow prefix would bound one path and the service read another.
func hasEncodedSlash(rawPath string) bool {
	return strings.Contains(fold.ASCII(rawPath), "%2f")
}

// hasBackslash reports whether the path as the command sent it holds a "\", literal or
// percent-encoded ("%5C" or "%5c"). A reader that takes "\" for a "/" (the WHATWG URL standard
// does, for an https URL) reads "/v1/x\..\..\admin" as "/admin", outside the "/v1/" prefix the
// literal path is inside, so an allow prefix would bound one path and the service read another.
// The proxy fills RawPath from URL.EscapedPath, which writes a literal "\" as "%5C"; both
// spellings are read here so that the rule does not rest on that.
func hasBackslash(rawPath string) bool {
	return strings.Contains(rawPath, `\`) || strings.Contains(fold.ASCII(rawPath), "%5c")
}

// hasDotSegmentParameter reports whether the path holds a segment whose text before its first
// ";" is "." or "..": "/v1/..;/admin", "/v1/..;x/admin" or "/a/.;/b". Tomcat, Jetty and Spring
// cut a ";" parameter off a segment before they resolve dot segments, so they read
// "/v1/..;/admin" as "/admin", where the whole-segment dot rule (hasDotSegment) sees no dot
// segment. It reads the decoded path, which holds a ";" for a ";" the command sent as "%3B" and a
// "." for a "%2E", so each of those spellings is read too.
func hasDotSegmentParameter(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if before, _, _ := strings.Cut(segment, ";"); before == "." || before == ".." {
			return true
		}
	}
	return false
}

// doubleEncoded are the escapes of a "%" followed by the escape of a "/", a "." or a "\", held
// ASCII-lower-cased as the raw path is compared.
var doubleEncoded = []string{"%252f", "%252e", "%255c"}

// hasDoubleEncoding reports whether the path as the command sent it holds "%25" followed by
// "2F", "2E" or "5C", in either case. One decoding reads it as "%2F", "%2E" or "%5C", which the
// other path rules do not refuse, and a server that decodes a second time reads a "/", a "." or a
// "\" there, so an allow prefix would bound one path and that server read another.
func hasDoubleEncoding(rawPath string) bool {
	folded := fold.ASCII(rawPath)
	for _, e := range doubleEncoded {
		if strings.Contains(folded, e) {
			return true
		}
	}
	return false
}

// check is the one decision both Decide and Rewrite take: the kind the request is carried under,
// or the reason it may not be. The reasons, and the order they are reported in, are the Lean
// model's.
//
// The name's binding gives its kind, whose hosts and allow bind every secret of the kind, and
// its profile's own allow, which narrows that one name: a request goes through only when both
// allow it, so a profile never widens its kind.
//
// Two of the refusals are about the request's port and names. The CONNECT port must be 443:
// every name the decision compares - the CONNECT host, the TLS name and the Host header - carries
// no port, so 443 is the only port the rules speak about, and another port of one name is another
// service. It is read above the two name comparisons because the port is part of what the
// CONNECT named.
//
// A path that holds a dot segment is refused for every kind: no client means one, and a reader
// that resolves it (RFC 3986 section 5.2.4) reads another path than the literal one. A path that
// holds an empty segment or a dot segment with a ";" parameter, or a raw path that holds a
// percent-encoded "/", a "\" in any spelling, or a double-encoded "/", "." or "\", is refused
// when, and only when, the kind or the profile has an allow list: an allow prefix bounds the path
// the service will read, and a reader that merges slashes, cuts ";" parameters before it resolves
// dot segments, decodes "%2F", takes "\" for "/" or decodes twice first reads another path than
// the literal one the prefix holds. With neither list every path of the kind's hosts is allowed
// already, so such a path escapes no prefix, and real clients send them: Cloud Storage's JSON API
// names the object dir/obj as "dir%2Fobj", and an S3 key may hold "//". The path rules are read
// directly above the allow lists because they are what makes those rules mean what they say.
//
// A request carrying a method override header, or a method override parameter in its query, is
// refused when, and only when, the kind or the profile has an allow list. allowOK compares the
// method on the request line, while a service that reads X-HTTP-Method-Override (Google's APIs
// do), one of its two other spellings, or a "_method" parameter acts on the method that names,
// so a list allowing {POST, /prefix} would carry the value on a PATCH or a DELETE under that
// prefix. The header and the parameter are refused rather than the method they name, because
// which services read them is not something the rules say. With neither list every method is
// allowed already, so they take nothing away and are not refused. They are read directly below
// the allow lists, for the same reason the path rules are read directly above them.
func check(rules Rules, g Grant, value string, r *Request) (*Kind, string) {
	if g.Placeholder == "" {
		return nil, "empty placeholder"
	}
	b := rules.Lookup(g.Secret)
	if b == nil {
		return nil, "unknown secret"
	}
	k := rules.KindOf(b)
	if k == nil || len(k.Hosts) == 0 {
		return nil, "secret not behind airlock"
	}
	// listed is whether the kind or the profile has an allow list, which the rules that keep a
	// list meaning what it says are read under.
	listed := k.Allow != nil || b.Allow != nil
	switch {
	case !HostBound(k.Hosts, r.ConnectHost):
		return nil, "host not bound"
	case r.ConnectPort != 443:
		return nil, "connect port is not 443"
	case r.SNI != r.ConnectHost:
		return nil, "sni mismatch"
	case r.HostHeader != r.ConnectHost:
		return nil, "host header mismatch"
	case hasDotSegment(r.Path):
		return nil, "path has a dot segment"
	case listed && hasEmptySegment(r.Path):
		return nil, "path has an empty segment"
	case listed && hasEncodedSlash(r.RawPath):
		return nil, "path has an encoded slash"
	case listed && hasBackslash(r.RawPath):
		return nil, "path has a backslash"
	case listed && hasDotSegmentParameter(r.Path):
		return nil, "path has a parameter on a dot segment"
	case listed && hasDoubleEncoding(r.RawPath):
		return nil, "path has a double-encoded character"
	case !allowOK(k.Allow, r):
		return nil, "not allowed by rule"
	case !allowOK(b.Allow, r):
		return nil, "not allowed by the profile's rule"
	case listed && hasMethodOverride(r):
		return nil, "request carries a method override header"
	case listed && hasMethodOverrideParameter(r.Query):
		return nil, "request carries a method override parameter"
	}
	// A request that already carries the real value learnt it somewhere other than this
	// swap, so the airlock refuses it rather than forwarding it. The scan reads every
	// header, not only the ones the kind names, and no way is exempt.
	for _, h := range r.Headers {
		if Carries(value, h[1]) {
			return nil, "request already carries the value"
		}
	}
	return k, ""
}

// Decide reports whether the request may carry the secret, and the reason it may not.
func Decide(rules Rules, g Grant, value string, r *Request) (bool, string) {
	_, why := check(rules, g, value, r)
	return why == "", why
}

// Rewrite returns the request with the placeholder swapped for value in the headers the kind
// names, and with every Cipher-Repository header removed, or nil when Decide refuses. It changes
// no other header's name, keeps the order of the headers it keeps, and leaves the query and the
// body as they came.
func Rewrite(rules Rules, g Grant, value string, r *Request) *Request {
	k, _ := check(rules, g, value, r)
	if k == nil {
		return nil
	}
	names := k.HeaderNames()
	out := *r
	out.Headers = make([][2]string, 0, len(r.Headers))
	for _, h := range r.Headers {
		if fold.ASCII(h[0]) == CipherRepositoryHeader {
			continue
		}
		for _, n := range names {
			if fold.ASCII(h[0]) == fold.ASCII(n) {
				h[1] = substitute(g.Placeholder, value, h[1])
				break
			}
		}
		out.Headers = append(out.Headers, h)
	}
	return &out
}

// mentions reports whether a header value carries value, directly or inside a Basic
// credential it decodes to.
func mentions(value, v string) bool {
	if strings.Contains(v, value) {
		return true
	}
	payload, ok := basicPayload(v)
	return ok && strings.Contains(payload, value)
}

// Carries reports whether a request header already carries the real value. The empty
// value carries nowhere, because every string contains it.
//
// It is exported for the proxy port (internal/cipher/proxy), which reads it to tell
// which of a grant's names one request is for: the name whose placeholder a header already
// holds. It is the same function the Lean model calls carries, unchanged by being exported,
// so the contract with that model is the contract of check and Rewrite as before.
func Carries(value, v string) bool {
	return value != "" && mentions(value, v)
}

// substitute swaps placeholder p for value in one header value, including inside a Basic
// credential (as the user or as the password), which it re-encodes. A value whose Basic
// payload does not carry the placeholder, and a value that is no Basic credential, take the
// plain replacement of every occurrence.
func substitute(p, value, v string) string {
	if payload, ok := basicPayload(v); ok && strings.Contains(payload, p) {
		return v[:6] + base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(payload, p, value)))
	}
	return strings.ReplaceAll(v, p, value)
}

// basicPayload decodes the "user:pass" behind a header value of the form "Basic <base64>",
// the scheme compared with fold.ASCII. Three narrow choices, each a place where the handy
// standard-library call would part from the model:
//   - the credential is trimmed of spaces and horizontal tabs alone (HTTP's optional
//     whitespace), not of every Unicode space that strings.TrimSpace takes;
//   - base64.StdEncoding: the standard alphabet, padding required;
//   - a payload counts only when it is valid UTF-8, because the model's strings are.
func basicPayload(v string) (string, bool) {
	if len(v) <= 6 || fold.ASCII(v[:6]) != "basic " {
		return "", false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.Trim(v[6:], " \t"))
	if err != nil {
		return "", false
	}
	payload := string(dec)
	if !utf8.ValidString(payload) {
		return "", false
	}
	return payload, true
}
