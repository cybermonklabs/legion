package proxyrules

// FuzzAirlock drives Validate, Decide and Rewrite against the Lean model of exactly those
// functions (lean/Airlock/Model.lean, built as the airlock-model program AIRLOCK_MODEL
// names). Every generated case goes to both sides and the two answers must be the same
// value, so a change on one side that is not made on the other is a test failure here
// rather than a surprise in production. This file is in package proxyrules, not package
// proxyrules_test: the generator and the wire shapes sit beside the code they mirror, and
// the comparison reaches the three functions with nothing in between.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// ---- The Lean model as a long-lived subprocess ---------------------------------------

// modelBuildPath is where lake build writes the model, relative to this package directory,
// which is the working directory of a test in this package. It is what AIRLOCK_MODEL is
// pointed at, by hand and by scripts/airlock-mutants.sh, and what the skip below names.
const modelBuildPath = "lean/.lake/build/bin/airlock-model"

// modelPath names the built airlock-model binary: AIRLOCK_MODEL, and nothing else. With
// that variable unset, or naming no file, it answers "" and FuzzAirlock skips, so a plain
// `go test ./...` of this module needs no Lean toolchain and no built model, and a run
// that does compare the two sides says which model it compared against rather than
// picking up whatever a past lake build left in the tree.
func modelPath() string {
	bin := os.Getenv("AIRLOCK_MODEL")
	if bin == "" {
		return ""
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return ""
	}
	return bin
}

// model is one airlock-model process, fed one JSON case per line and read one JSON answer
// per line. Starting a process per case costs milliseconds each and would dominate the run,
// so the process is started once and kept alive. Go runs each fuzz worker as its own
// process of this test binary, so one process per binary is one model per worker; the mutex
// keeps the line protocol a single conversation when -parallel drives one worker's target
// from several goroutines.
type model struct {
	mu  sync.Mutex
	in  *bufio.Writer
	out *bufio.Reader
}

var (
	modelOnce sync.Once
	theModel  *model
	modelErr  error
)

func leanModel(t testing.TB) *model {
	modelOnce.Do(func() {
		cmd := exec.Command(modelPath())
		cmd.Stderr = os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			modelErr = err
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			modelErr = err
			return
		}
		if modelErr = cmd.Start(); modelErr != nil {
			return
		}
		theModel = &model{in: bufio.NewWriter(stdin), out: bufio.NewReaderSize(stdout, 1<<20)}
	})
	if modelErr != nil {
		t.Fatalf("start lean model %s: %v", modelPath(), modelErr)
	}
	return theModel
}

// wireCase is one line of the model's input protocol. The rules, the grant and the request
// are the package's own types, marshalled by encoding/json: the model's derived FromJson
// ignores a field's default, so every field has to be on the wire, which marshalling the
// real types gives for free and a hand-built JSON object would not.
type wireCase struct {
	Rules   Rules    `json:"rules"`
	Grant   Grant    `json:"grant"`
	Value   string   `json:"value"`
	Request *Request `json:"request"`
}

// answer is one line of the model's output protocol, and the same shape Go's side is read
// into, so the two are compared as values. Error carries the model's parse refusal.
type answer struct {
	Valid   bool     `json:"valid"`
	Allow   bool     `json:"allow"`
	Reason  string   `json:"reason"`
	Rewrite *Request `json:"rewrite"`
	Error   string   `json:"error,omitempty"`
}

func (m *model) ask(t testing.TB, line []byte) answer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.in.Write(line)
	m.in.WriteByte('\n')
	if err := m.in.Flush(); err != nil {
		t.Fatalf("write to model: %v", err)
	}
	resp, err := m.out.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read from model: %v", err)
	}
	var a answer
	if err := json.Unmarshal(resp, &a); err != nil {
		t.Fatalf("model answer %q: %v", resp, err)
	}
	if a.Error != "" {
		t.Fatalf("model could not parse case %s: %s", line, a.Error)
	}
	return a
}

func goAnswer(c wireCase) answer {
	allow, reason := Decide(c.Rules, c.Grant, c.Value, c.Request)
	return answer{
		Valid:   Validate(c.Rules) == nil,
		Allow:   allow,
		Reason:  reason,
		Rewrite: Rewrite(c.Rules, c.Grant, c.Value, c.Request),
	}
}

// ---- Case generator: a byte string drives choices among tricky pools ------------------

type src struct{ b []byte }

func (s *src) u8() byte {
	if len(s.b) == 0 {
		return 0
	}
	c := s.b[0]
	s.b = s.b[1:]
	return c
}

// raw takes up to 15 arbitrary bytes (may be invalid UTF-8).
func (s *src) raw() string {
	n := int(s.u8() % 16)
	if n > len(s.b) {
		n = len(s.b)
	}
	x := string(s.b[:n])
	s.b = s.b[n:]
	return x
}

func (s *src) text() string { return strings.ToValidUTF8(s.raw(), "") }

// pick returns a pool entry, or (1 in 8) fresh valid-UTF-8 text.
func (s *src) pick(pool []string) string {
	i := int(s.u8())
	if i%8 == 7 {
		return s.text()
	}
	return pool[(i/8)%len(pool)] // i%8 picked the escape; i/8 reaches every entry
}

// port returns a portPool entry, or (1 in 8) a free 16-bit draw, so the comparison covers
// every port and not only the ones named here.
func (s *src) port() uint16 {
	i := int(s.u8())
	if i%8 == 7 {
		return uint16(s.u8())<<8 | uint16(s.u8())
	}
	return portPool[(i/8)%len(portPool)] // i%8 picked the escape; i/8 reaches every entry
}

var (
	// Every seed of the committed corpus decodes its draws through these lists by their
	// lengths, so adding an entry to one changes what every seed means: a seed that isolated
	// one refusal can start drawing an entry that masks it. The hosts validate refuses for
	// their shape are compared against the model in TestModelAgreesOnHostShapes instead.
	hostPool = []string{"api.github.com", "*.github.com", "github.com", "*.com", "*.", "API.GitHub.com", "*.GitHub.COM", "uploads.github.com", "a.b.github.com", ".github.com", "githu\u212a.com", "githuk.com", "\u0130.github.com", "", "api.anthropic.com"}
	// kindNamePool holds the names a kind and a binding draw alike, so a binding often names a
	// kind the rules declare: three names Validate takes, and three it refuses (a capital, a
	// hyphen and the empty name), so its "one lowercase word" refusal is reached.
	kindNamePool = []string{"github", "anthropic", "slack", "Anthropic", "git-hub", ""}
	namePool     = []string{"gh", "ANTHROPIC_ADMIN_HDO", "GH", ""}
	// hdrPool holds Cipher-Repository in three spellings the ASCII fold takes and a longer name
	// it does not, so the rewrite's removal of that header is compared on both sides.
	hdrPool    = []string{"authorization", "Authorization", "AUTHORIZATION", "x-api-key", "X-Api-Key", "x-api-\u212aey", "proxy-authorization", "cookie", "Cipher-Repository", "cipher-repository", "CIPHER-REPOSITORY", "cipher-repositoryx"}
	methodPool = []string{"GET", "POST", "get", "DELETE", "PUT"}
	// overridePool is the three headers check refuses under a kind or a profile with an allow
	// list, each in a case the ASCII fold has to work on, with two near misses: a longer name
	// that only starts like one of them, and the KELVIN SIGN spelling that the fold here
	// leaves alone where strings.EqualFold would not.
	overridePool = []string{"x-http-method-override", "X-HTTP-Method-Override", "x-http-method", "X-Http-Method", "x-method-override", "X-METHOD-OVERRIDE", "x-http-method-overridden", "x-http-met\u212aod"}
	// pathPool holds paths with no dot segment, paths that carry one, and two near misses -
	// "..foo" and "..." are segments no reader resolves; paths with an empty segment; a path
	// with a literal backslash; dot segments with a ";" parameter and two near misses, "...;"
	// and ";.."; decoded paths that still hold an escaped ".", "/", "\" or "%", and a near miss
	// that holds an escaped letter; and paths at, past and beside the "/" boundary of each prefix
	// of prefixPool ("/repos", "/repos/x" and "/reposX"; "/storage-limit", "/storage-limit/7" and
	// "/storage-limitX"), so the comparison covers where each side draws each of those lines.
	pathPool = []string{"/", "/repos/x", "/repos", "/admin", "", "/v1/../admin", "/a/./b", "/..", "/.", "/v1/things/../../admin", "/v1/..foo", "/v1/.../x",
		"/reposX", "/repos/", "/a//b", "//", "/repos//x", "/v1/organizations/workspaces", "/v1/organizations", "/v1/organizationsX", "/v1/messages",
		"/storage-limit", "/storage-limitX", "/storage-limit/7", `/repos\x`,
		"/repos/..;/admin", "/repos/..;x=1/admin", "/repos/.;/x", "/repos/...;/x", "/repos/;../x",
		"/repos/%2e%2e/admin", "/repos/%2Fx", "/repos/%5cx", "/repos/100%25", "/repos/%41"}
	// portPool is the CONNECT port: 443 five times in eight, so a case reaches the refusals
	// below the port, and otherwise a port the rules say nothing about.
	portPool   = []uint16{443, 443, 443, 443, 443, 80, 8443, 0}
	prefixPool = []string{"/", "/repos", "", "/r", "/admin/", "/repos/", "/v1/organizations/", "/storage-limit"}
	// separatorPool is what the raw path may hold in place of a "/": the two spellings of an
	// encoded "/" and the three of a "\" that check refuses under an allow list; spellings of a
	// double-encoded "/", "." or "\", which the raw path rules let through and which a decoded
	// path holding them is refused for; and three near misses, a cut-off escape, an encoded "%"
	// before another escape and a cut-off double escape.
	separatorPool = []string{"%2F", "%2f", "%252F", "%2", "%5C", "%5c", `\`, "%255C", "%252e", "%2541", "%252",
		"%25%32%46", "%252%46", "%25%32%65", "%25252f"}
	// methodParameterPool is a query parameter a request may carry: names check reads as
	// "_method" under an allow list - as sent, folded, escaped in part or whole, with a "." or a
	// "[" for its "_", with a "+", a space or an escaped space before it, with a NUL and more
	// after it, after a ";", with a value and without - names that do not decode, and near
	// misses: a name longer at either end, as sent or escaped, "_method" as a value, after a ";"
	// too, a "+" before "method", a NUL before "_method", a dot after "_method", and a value that
	// does not decode.
	methodParameterPool = []string{"_method=PUT", "_method", "_METHOD=delete", "%5Fmethod=PUT", "%5fmethod=",
		"_meth%6Fd=DELETE", "%5F%6Detho%64=PUT", ".method=PUT", "+_method=PUT", "%20_method=PUT", " _method=PUT", "[method=PUT",
		"_method%00=PUT", ".method%00x=PUT", "%5Fmethod%00=PUT", "x;_method=DELETE", "a=1;_method=PUT",
		"_method%zz=1", "q%=1",
		"x_method=PUT", "_methods=PUT", "x%5Fmethod=PUT", "a=_method", "a=1;b=_method", "+method=PUT", "%00_method=PUT", "_method%2e=PUT", "a=%zz"}
	phPool     = []string{"PH_gh_1", "ph", "", "a", "=", "user"}
	valuePool  = []string{"REAL", "ghp_secret", "", "PH_gh_1", "a:b", "\u00e9"}
	piecePool  = []string{"user", "token ", " ", "\t", "x"}
	schemePool = []string{"Basic ", "basic ", "BASIC ", "Basic  ", "Basic\t", "Basi "}
	tailPool   = []string{"\v", " ", "\t", "\n", "\u00a0", "\u0085"}
)

func (s *src) list(n int, f func() string) []string {
	out := []string{}
	for range n {
		out = append(out, f())
	}
	return out
}

// piece builds a fragment that often holds the placeholder (so the swap has work to do) or
// the value (so check's "request already carries the value" refusal is reached).
func (s *src) piece(g Grant, value string, allowRaw bool) string {
	switch s.u8() % 8 {
	case 0, 1:
		return g.Placeholder
	case 2, 3:
		return value
	case 4:
		return ":"
	case 5:
		return "Bearer "
	case 6:
		if allowRaw {
			return s.raw() // may be invalid UTF-8: only used inside Basic payloads
		}
		return s.text()
	default:
		return s.pick(piecePool)
	}
}

func (s *src) compose(g Grant, value string, allowRaw bool) string {
	var b strings.Builder
	for n := 1 + int(s.u8()%3); n > 0; n-- {
		b.WriteString(s.piece(g, value, allowRaw))
	}
	return b.String()
}

// headerValue builds one header value: two in three plain, one in three a Basic credential
// whose payload is composed the same way. Both shapes carry the placeholder and the value
// often, so the swap and the "already carries the value" refusal are each reached directly
// and through a decoded Basic credential.
func (s *src) headerValue(g Grant, value string) string {
	k := s.u8()
	if k%3 != 0 {
		return s.compose(g, value, false)
	}
	scheme := s.pick(schemePool)
	enc := base64.StdEncoding.EncodeToString([]byte(s.compose(g, value, true)))
	switch s.u8() % 8 {
	case 1:
		enc = strings.TrimRight(enc, "=")
	case 2:
		if len(enc) > 4 {
			enc = enc[:4] + "\r\n" + enc[4:]
		}
	case 3:
		enc += s.pick(tailPool)
	case 4:
		if i := strings.IndexByte(enc, '='); i > 0 {
			b := []byte(enc)
			b[i-1] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"[s.u8()%64]
			enc = string(b)
		}
	case 5:
		enc += s.text()
	}
	return scheme + enc
}

// allow draws an allow list: nil (any method and path), empty (nothing allowed), or one to
// three rows. One row in hostOdds names a host: a profile's rows draw it often, since a
// profile's row may name one of its kind's hosts, and a kind's rows rarely, since CheckKind
// refuses that at the policy load while Validate and the decision read it like any row.
func (s *src) allow(hostOdds byte) []Allow {
	switch s.u8() % 3 {
	case 0:
		return nil
	case 1:
		return []Allow{}
	}
	rows := []Allow{}
	for m := 1 + int(s.u8()%3); m > 0; m-- {
		a := Allow{Method: s.pick(methodPool), PathPrefix: s.pick(prefixPool)}
		if s.u8()%hostOdds == 0 {
			a.Host = s.pick(hostPool)
		}
		rows = append(rows, a)
	}
	return rows
}

// kind draws one kind: its name, its hosts, its headers and its allow. Hosts is never nil:
// the model's hosts is a List String, and JSON null is no list, so a nil slice would be a case
// the model refuses to parse rather than a case about a kind with no host. The empty list is
// that case, and list gives it. The fields no decision reads (ways, variables, the placeholder
// prefix and max_grant) are left unset.
func (s *src) kind() Kind {
	k := Kind{Name: s.pick(kindNamePool)}
	k.Hosts = s.list(int(s.u8()%3), func() string { return s.pick(hostPool) })
	switch s.u8() % 3 { // nil: the default ["authorization"]; empty: no header; or names
	case 1:
		k.Headers = []string{}
	case 2:
		k.Headers = s.list(1+int(s.u8()%3), func() string { return s.pick(hdrPool) })
	}
	k.Allow = s.allow(8)
	return k
}

// rawPath draws the path as the command sent it: the decoded path itself two times in five;
// the path with its last "/" written as a pick of separatorPool; the path with one appended; or
// the path with each "%" written "%25", which is how a request sends a decoded path that holds a
// "%", so check's "path has an encoded slash", "path has a backslash" and "path has a
// double-encoded character" refusals and their near misses are reached on paths every other
// rule allows.
func (s *src) rawPath(path string) string {
	switch s.u8() % 5 {
	case 2:
		if j := strings.LastIndexByte(path, '/'); j >= 0 {
			return path[:j] + s.pick(separatorPool) + path[j+1:]
		}
		return path
	case 3:
		return path + s.pick(separatorPool)
	case 4:
		return strings.ReplaceAll(path, "%", "%25")
	default:
		return path
	}
}

func genCase(data []byte) wireCase {
	s := &src{b: data}
	// Kinds and Names are never nil, for the reason a kind's Hosts is not.
	rules := Rules{Kinds: []Kind{}, Names: []Binding{}}
	for n := int(s.u8() % 4); n > 0; n-- {
		rules.Kinds = append(rules.Kinds, s.kind())
	}
	// A binding names a kind drawn from the pool the kinds' names are drawn from, carries its
	// profile's own allow (nil, empty or rows, which often name a host), and one time in six
	// holds an inline rule, which three times in four names no kind beside it and otherwise
	// does, a binding Validate refuses.
	for n := int(s.u8() % 4); n > 0; n-- {
		b := Binding{Name: s.pick(namePool), Kind: s.pick(kindNamePool), Allow: s.allow(2)}
		if s.u8()%6 == 0 {
			inline := s.kind()
			b.Inline = &inline
			if s.u8()%4 != 0 {
				b.Kind = ""
			}
		}
		rules.Names = append(rules.Names, b)
	}
	g := Grant{Secret: s.pick(namePool), Placeholder: s.pick(phPool)}
	value := s.pick(valuePool)
	r := &Request{ConnectHost: s.pick(hostPool), ConnectPort: s.port(), Method: s.pick(methodPool), Path: s.pick(pathPool), Headers: [][2]string{}}
	r.RawPath = s.rawPath(r.Path)
	r.SNI, r.HostHeader = r.ConnectHost, r.ConnectHost
	if s.u8()%4 == 0 {
		r.SNI = s.pick(hostPool)
	}
	if s.u8()%4 == 0 {
		r.HostHeader = s.pick(hostPool)
	}
	for n := int(s.u8() % 4); n > 0; n-- {
		r.Headers = append(r.Headers, [2]string{s.pick(hdrPool), s.headerValue(g, value)})
	}
	r.Query = s.compose(g, value, false)
	r.Body = s.compose(g, value, false)
	// One request in four carries a method override header, and one in four a method override
	// parameter in its query, before or after what the query held, so check's refusal of each
	// under a kind or a profile with an allow list is reached from both sides. These draws are
	// the last of the case: a byte string that has run out answers 0 here, and 0 adds nothing.
	if s.u8()%4 == 1 {
		r.Headers = append(r.Headers, [2]string{s.pick(overridePool), s.compose(g, value, false)})
	}
	switch s.u8() % 8 {
	case 1:
		r.Query = s.pick(methodParameterPool) + "&" + r.Query
	case 2:
		r.Query += "&" + s.pick(methodParameterPool)
	}
	return wireCase{Rules: rules, Grant: g, Value: value, Request: r}
}

// allValidUTF8: the wire (JSON) and the model (Lean String) carry only valid UTF-8, so a
// case with a byte string that is not is out of scope — Go would send it as U+FFFD and the
// two sides would be compared on different strings. Basic payloads may still decode to
// invalid UTF-8, which is a case both sides do handle, and basicPayload refuses alike.
func allValidUTF8(c wireCase) bool {
	b, _ := json.Marshal(c)
	return utf8.Valid(b) && !bytes.Contains(b, []byte(`\ufffd`))
}

// compare sends one case to both sides and fails t when the two answer differently. It answers
// the one answer, which a caller may hold to more. Compared as values, not as bytes: Lean's
// Json.compress prints an object's keys sorted and encoding/json prints a struct's fields in
// declaration order, so the same answer is two different lines.
func compare(t testing.TB, c wireCase) answer {
	t.Helper()
	line, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	want := leanModel(t).ask(t, line)
	got := goAnswer(c)
	if !reflect.DeepEqual(want, got) {
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		t.Fatalf("Go and the Lean model disagree\ncase: %s\nlean: %s\ngo:   %s", line, wj, gj)
	}
	return got
}

// FuzzAirlock compares the two sides on every case the byte string generates. The committed
// corpus under testdata/fuzz/FuzzAirlock reaches every refusal reason, both answers of Validate
// and each of its refusals, and each mutant of testdata/mutants, so `go test -run FuzzAirlock`
// is already the differential test; -fuzz goes on from those seeds. Before the generated cases,
// the anthropic kind's calls of TestTheAnthropicKind go to both sides as they are.
func FuzzAirlock(f *testing.F) {
	if modelPath() == "" {
		f.Skip("AIRLOCK_MODEL names no built airlock-model; lake build in lean/ writes one to " + modelBuildPath)
	}
	for _, c := range anthropicCases() {
		compare(f, c)
	}
	f.Add([]byte{}) // the empty byte string: no kind, no binding, no header, every pool at its head
	f.Fuzz(func(t *testing.T, data []byte) {
		c := genCase(data)
		if !allValidUTF8(c) {
			t.Skip()
		}
		compare(t, c)
	})
}

// TestTheModelRefusesAHostlessEntry: rules whose kind binds no host, or whose binding holds an
// inline rule that binds none, are refused on both sides, since a kind binds its credential to
// its hosts (theorem T4 of the model). A proxy request for a name of it is still refused as a
// secret not behind the airlock, on both sides. It needs the model, as
// TestModelAgreesOnHostShapes does, so scripts/database-skips.sh expects its skip and the
// airlock-lean job's seed step runs it by name.
func TestTheModelRefusesAHostlessEntry(t *testing.T) {
	if modelPath() == "" {
		t.Skip("AIRLOCK_MODEL names no built airlock-model; lake build in lean/ writes one to " + modelBuildPath)
	}
	for name, rules := range map[string]Rules{
		"a kind of no host": {
			Kinds: []Kind{{Name: "hostless", Hosts: []string{}}},
			Names: []Binding{{Name: "HOSTLESS", Kind: "hostless"}},
		},
		"an inline rule of no host": {
			Kinds: []Kind{},
			Names: []Binding{{Name: "HOSTLESS", Inline: &Kind{Name: "minted", Hosts: []string{}}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := compare(t, wireCase{
				Rules: rules,
				Grant: Grant{Secret: "HOSTLESS", Placeholder: "PH"},
				Value: "the-value-not-a-secret",
				Request: &Request{
					ConnectHost: "api.example.com", ConnectPort: 443, SNI: "api.example.com", HostHeader: "api.example.com",
					Method: "GET", Path: "/", RawPath: "/", Headers: [][2]string{{"Authorization", "Bearer PH"}},
				},
			})
			if got.Valid || got.Allow || got.Reason != "secret not behind airlock" {
				t.Fatalf("the host-less rule answers %+v; want invalid rules and a refusal, secret not behind airlock", got)
			}
		})
	}
}

// TestModelAgreesOnHostShapes compares Go's Validate with the Lean model's validate over the
// host shapes the rules may hold, the ones no request can ever match among them, as a kind's
// host and as an inline rule's. They are read here rather than added to hostPool, because every
// seed of the committed corpus decodes its hosts through that list by its length.
func TestModelAgreesOnHostShapes(t *testing.T) {
	if modelPath() == "" {
		t.Skip("AIRLOCK_MODEL names no built airlock-model; lake build in lean/ writes one to " + modelBuildPath)
	}
	hosts := []string{
		"slack.com", "*.googleapis.com", "API.GitHub.com", "a-b.example.com", "localhost",
		"1e100.net", "*.0.example.com", "xn--p1ai", "93.184.216.34", "*.216.34", "*.34", "8080",
		"https://slack.com", "*", "slack.com/", "slack.com:443",
		".slack.com", "slack..com", "slack.com.", "*.", "", "-slack.com", "slack-.com",
		"slack_com", "*.*.com", "sl\u212aack.com",
	}
	for _, host := range hosts {
		for _, as := range []string{"kind", "inline"} {
			t.Run(as+" "+host, func(t *testing.T) {
				k := Kind{Name: "slack", Hosts: []string{host}, Headers: []string{"authorization"}}
				rules := Rules{Kinds: []Kind{k}, Names: []Binding{{Name: "gh", Kind: "slack"}}}
				if as == "inline" {
					rules = Rules{Kinds: []Kind{}, Names: []Binding{{Name: "gh", Inline: &k}}}
				}
				c := wireCase{
					Rules:   rules,
					Grant:   Grant{Secret: "gh", Placeholder: "agent-secrets-placeholder-gh"},
					Value:   "the-value",
					Request: &Request{ConnectHost: "slack.com", ConnectPort: 443, Method: "GET", Path: "/", RawPath: "/", SNI: "slack.com", HostHeader: "slack.com", Headers: [][2]string{}},
				}
				if !allValidUTF8(c) {
					t.Skip("the case does not go over the wire as valid UTF-8")
				}
				if got := compare(t, c); got.Valid != HostShapeOK(host) {
					t.Errorf("both sides call the rules valid=%v for host %q, and HostShapeOK says %v", got.Valid, host, HostShapeOK(host))
				}
			})
		}
	}
}
