// packages/envoy/internal/cipher/proxy/kinds_test.go
//
// The rows of the airlock's rules read from kinds, on the rig's fake grants: the call line names
// the way the name's credential went in, an exchange writes the one line of the token it got and
// never the token, the proxy removes Cipher-Repository from what it forwards whatever way the
// call takes, a path that two readers would resolve differently is refused, and a profile's own
// allow narrows its kind's. The rows of a real broker behind the proxy, the anthropic kind's, are
// anthropic_kind_test.go's.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// holdsText reports whether v, as encoding/json writes it, holds text. It is the one search the
// rows below make, both over what the broker logged and over what a stand-in recorded, so the
// search that finds a token in the stand-in's record is the one that finds none in the log.
func holdsText(t *testing.T, v any, text string) bool {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T for the search: %v", v, err)
	}
	return strings.Contains(string(encoded), text)
}

// sha256Hex is the hex SHA-256 of text, as requests.MintMessage's sha256 field writes it.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// TestTheCallLineNamesTheWay: each call to a bound host logs the way its name's credential went
// into it, the binding's way: swap for a stored value, mint for a minted token, sign for an AWS
// key pair signed again, exchange for the access token of an azure-login exchange, and dpop for a
// capability the broker minted and holds. A reader of the log tells from the line alone which of
// the five paths carried the call.
//
// The named revert: drop the way from callFields, and every row fails.
func TestTheCallLineNamesTheWay(t *testing.T) {
	for _, row := range []struct {
		way string
		// call sends the one call the row is about, through the rig it builds, and answers the
		// rig and the path of that call.
		call func(t *testing.T) (*rig, string)
	}{
		{proxyrules.WaySwap, func(t *testing.T) (*rig, string) {
			r := newRig(t, Options{})
			if answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder); answer.StatusCode != http.StatusOK {
				t.Fatalf("the swapped call answered %d %q; want 200", answer.StatusCode, body)
			}
			return r, "/v1/things"
		}},
		{proxyrules.WayMint, func(t *testing.T) (*rig, string) {
			rules := testRules()
			rules.Kinds[0].Ways = []string{proxyrules.WayMint}
			rules.Names[0].Way = proxyrules.WayMint
			r := newRuledRig(t, rules, Options{})
			r.useGitHubAppGrant()
			if answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder); answer.StatusCode != http.StatusOK {
				t.Fatalf("the minted call answered %d %q; want 200", answer.StatusCode, body)
			}
			if got := r.requests()[0].Header.Get("Authorization"); got != "Bearer "+githubAcmeToken {
				t.Fatalf("the minted call reached the service with Authorization %q; want acme's minted token", got)
			}
			return r, "/v1/things"
		}},
		{proxyrules.WaySign, func(t *testing.T) (*rig, string) {
			r := newAWSRig(t, false)
			verifyingStandIn(r, false)
			answer, body := awsCall(t, r, r.client(awsKeyPlaceholder), http.MethodGet, "https://"+boundHost+"/v1/things?Action=GetCallerIdentity&Version=2011-06-15", "", "sts", false)
			if answer.StatusCode != http.StatusOK || body != awsStandInAnswer {
				t.Fatalf("the signed call answered %d %q; want 200 %q", answer.StatusCode, body, awsStandInAnswer)
			}
			return r, "/v1/things"
		}},
		{proxyrules.WayExchange, func(t *testing.T) (*rig, string) {
			r, _ := newAzureRig(t, Options{})
			client := r.client(azureFederatedPlaceholder)
			_, body := tokenRequest(t, r, client, azureFederatedPlaceholder)
			access := placeholderTokenOf(t, body)
			if answer, body := r.get(client, "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+access); answer.StatusCode != http.StatusOK {
				t.Fatalf("the call with the access token answered %d %q; want 200", answer.StatusCode, body)
			}
			return r, "/v1/subscriptions"
		}},
		{proxyrules.WayDPoP, func(t *testing.T) (*rig, string) {
			r, _ := newDPoPRig(t)
			if answer, body := r.postMinted("Authorization", "DPoP "+mintedPlaceholder); answer.StatusCode != http.StatusOK {
				t.Fatalf("the dpop write answered %d %q; want 200", answer.StatusCode, body)
			}
			return r, mintedPath
		}},
	} {
		t.Run(row.way, func(t *testing.T) {
			r, path := row.call(t)
			line := callLineOf(t, r, map[string]any{"path": path})
			if line.fields["allowed"] != true || line.fields["way"] != row.way {
				t.Errorf("the call to %s logged allowed %v, way %v; want an allowed line naming the way %s", path, line.fields["allowed"], line.fields["way"], row.way)
			}
		})
	}
}

// callLineOf is the call line of the one call whose fields hold every field of match, once the
// proxy has written it: a call's line is written after its answer reaches the client
// (callsAtLeast says why that is a race to read straight away), and a row whose call follows
// others of its own (an exchange's token request) waits for that call's line and not for the
// first. It fails t unless exactly one line matches.
func callLineOf(t *testing.T, r *rig, match map[string]any) logLine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var lines []logLine
		for _, line := range r.logs.calls() {
			matched := true
			for field, value := range match {
				matched = matched && line.fields[field] == value
			}
			if matched {
				lines = append(lines, line)
			}
		}
		switch {
		case len(lines) == 1:
			return lines[0]
		case len(lines) > 1:
			t.Fatalf("the proxy logged %d call lines of %v: %v; want one", len(lines), match, lines)
		case time.Now().After(deadline):
			t.Fatalf("the proxy logged no call line of %v: %v", match, r.logs.calls())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAnExchangeLogsTheSHA256OfItsTokenAndNeverTheToken: the access token an azure-login exchange
// gets is minted for the grant as a release's tokens are, so the proxy writes the one line of it
// (requests.MintMessage): the grant, its enrollment, the secret, the way exchange, the type, the
// slot the token fills and its SHA-256, which a scan of a pod's files compares against. No line
// the broker writes holds the token. The control: the same search finds the token in the
// stand-in's record of the call it was swapped into, so a search that finds nothing in the log is
// one that could have.
//
// The named revert: drop logExchanged from exchangeToken, and the line is missing.
func TestAnExchangeLogsTheSHA256OfItsTokenAndNeverTheToken(t *testing.T) {
	r, stand := newAzureRig(t, Options{})
	client := r.client(azureFederatedPlaceholder)
	_, body := tokenRequest(t, r, client, azureFederatedPlaceholder)
	access := placeholderTokenOf(t, body)
	if answer, body := r.get(client, "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+access); answer.StatusCode != http.StatusOK {
		t.Fatalf("the call with the access token answered %d %q; want 200", answer.StatusCode, body)
	}
	if exchanges := stand.Exchanges(); len(exchanges) != 1 {
		t.Fatalf("the token endpoint saw %d exchanges; want one", len(exchanges))
	}

	minted := r.logs.lines(requests.MintMessage)
	if len(minted) != 1 {
		t.Fatalf("the proxy wrote %d %q lines; want one, for the one exchange", len(minted), requests.MintMessage)
	}
	fields := minted[0].fields
	want := map[string]any{
		"enrollment": testEnrollID, "grant": testGrantID, "secret": azureName, "way": proxyrules.WayExchange,
		"type": "azure-login", "sha256": sha256Hex(azureRealAccess),
	}
	for field, value := range want {
		if fields[field] != value {
			t.Errorf("the mint line's %s is %v; want %v", field, fields[field], value)
		}
	}
	if slot, _ := fields["slot"].(string); !strings.HasPrefix(slot, requests.SlotAzureAccessPrefix) {
		t.Errorf("the mint line's slot is %v; want the access slot the token fills", fields["slot"])
	}

	seen := r.requests()
	if len(seen) == 0 || !holdsText(t, seen[len(seen)-1], azureRealAccess) {
		t.Fatal("the control failed: the search finds no access token in the stand-in's record of the call it was swapped into")
	}
	if holdsText(t, r.logs.writtenLines(t), azureRealAccess) {
		t.Error("a line the broker wrote holds the access token itself")
	}
}

// TestCipherRepositoryNeverReachesTheService: the proxy reads Cipher-Repository to say which
// repository a GitHub request is for, and removes it from every request it forwards, so no
// service sees what the broker read: on the swap path (proxyrules.Rewrite) and on the sign path,
// where the header is removed before the call is signed again, so the signature does not cover
// it and the service still takes the call. The control: a header of the command's own that no
// rule touches reaches the stand-in, so the stand-in records what it is sent.
//
// The named reverts: keep the header in Rewrite (proxyrules' go-cipher-repository-forwarded
// mutant), or drop the removal before signAWS, and the header reaches the service.
func TestCipherRepositoryNeverReachesTheService(t *testing.T) {
	const repository, note = "ada/proofs", "the command's own header"
	for _, row := range []struct {
		name string
		call func(t *testing.T) *rig
	}{
		{"the swap path", func(t *testing.T) *rig {
			r := newRig(t, Options{})
			answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
				"Authorization", "Bearer "+workerPlaceholder, "Cipher-Repository", repository, "X-Command-Note", note)
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("the swapped call answered %d %q; want 200", answer.StatusCode, body)
			}
			return r
		}},
		{"the sign path", func(t *testing.T) *rig {
			r := newAWSRig(t, false)
			verifyingStandIn(r, false)
			answer, body := awsCall(t, r, r.client(awsKeyPlaceholder), http.MethodGet, "https://"+boundHost+"/v1/things", "", "execute-api", false,
				"Cipher-Repository", repository, "X-Command-Note", note)
			if answer.StatusCode != http.StatusOK || body != awsStandInAnswer {
				t.Fatalf("the signed call answered %d %q; want 200 %q, a signature the service takes", answer.StatusCode, body, awsStandInAnswer)
			}
			return r
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := row.call(t)
			seen := r.requests()
			if len(seen) != 1 {
				t.Fatalf("the service saw %d calls; want one", len(seen))
			}
			if got := seen[0].Header.Get("X-Command-Note"); got != note {
				t.Fatalf("the control failed: the service saw X-Command-Note %q; want %q as the command sent it", got, note)
			}
			if got := seen[0].Header.Values("Cipher-Repository"); len(got) != 0 {
				t.Errorf("the service saw Cipher-Repository %q; want it removed", got)
			}
		})
	}
}

// TestAPathTwoReadersResolveApartIsRefused: an allow prefix bounds the path and the method the
// service reads, and a service that merges "//", decodes "%2F", reads "\" or "%5C" as "/", drops a
// ";" parameter off a ".." segment (a Java servlet container reads "/..;/" as "/../"), decodes a
// path twice in any spelling ("%25%32%46" is "%2F" once decoded, and "/" twice), or takes its
// method from a "_method" parameter reads another request than the one the allow list holds. A
// "_method" name counts as such a framework reads it: percent-decoded, cut at its first NUL as
// PHP and Symfony cut it, a "+" or a leading space dropped, "." or "[" read as "_" (Express's
// querystring and PHP's $_GET), split off the query at a ";" as well as a "&" (PHP's
// arg_separator.input), and a name that does not decode counts too. So where the kind or the
// profile has an allow, the proxy refuses each of these, 403 in the decision's own words, before
// anything is sent: each refused row's path is under /v1/, which the list allows, so the rule that
// refuses it is the path or query rule and not the list. Where neither has one there is no prefix
// to bound, so the same forms go to the service as the command sent them, an object name's "%2F"
// through the gcp kind included. The controls: the path with one slash, and parameters whose name
// is no "_method" however it is read, are carried under the allow.
//
// The named revert: read the decoded path as the raw one (proxy-raw-path-decoded), and the
// encoded slash is carried.
func TestAPathTwoReadersResolveApartIsRefused(t *testing.T) {
	v1 := []proxyrules.Allow{{Method: http.MethodGet, PathPrefix: "/v1/"}, {Method: http.MethodPost, PathPrefix: "/v1/"}}
	// rules is testRules with kind the kind's allow and profile the binding's own, nil for none.
	rules := func(kind, profile []proxyrules.Allow) proxyrules.Rules {
		r := testRules()
		r.Kinds[0].Allow, r.Names[0].Allow = kind, profile
		return r
	}
	underKind, underProfile, underNone := rules(v1, nil), rules(nil, v1), rules(nil, nil)
	for _, row := range []struct {
		name  string
		rules proxyrules.Rules
		// target is the path and query as the command sends them. literal sends it as the
		// request line verbatim (URL.Opaque): a backslash no URL writes, since net/url escapes it.
		target  string
		literal bool
		// reason is the refusal, "" for a request the service is sent as the command wrote it.
		reason string
	}{
		{"an empty segment", underKind, "/v1//things", false, "path has an empty segment"},
		{"a trailing empty segment", underKind, "/v1/things//", false, "path has an empty segment"},
		{"an encoded slash", underKind, "/v1/ada%2Fproofs", false, "path has an encoded slash"},
		{"an encoded slash in lower case", underKind, "/v1/ada%2fproofs", false, "path has an encoded slash"},
		{"an encoded backslash", underKind, "/v1/ada%5Cproofs", false, "path has a backslash"},
		{"an encoded backslash in lower case", underKind, "/v1/ada%5cproofs", false, "path has a backslash"},
		{"a backslash as the command wrote it", underKind, `/v1/ada\proofs`, true, "path has a backslash"},
		{"a _method parameter", underKind, "/v1/things?_method=DELETE", false, "request carries a method override parameter"},
		{"a _METHOD parameter after another", underKind, "/v1/things?a=1&_METHOD=PUT", false, "request carries a method override parameter"},
		{"an encoded _method parameter", underKind, "/v1/things?%5Fmethod=DELETE", false, "request carries a method override parameter"},
		{"an empty segment under the profile's allow alone", underProfile, "/v1//things", false, "path has an empty segment"},
		{"a _method parameter under the profile's allow alone", underProfile, "/v1/things?_method=DELETE", false, "request carries a method override parameter"},
		{"a _method parameter with an escaped letter", underKind, "/v1/things?_meth%6Fd=DELETE", false, "request carries a method override parameter"},
		{"a _method parameter escaped whole", underKind, "/v1/things?%5F%6Detho%64=DELETE", false, "request carries a method override parameter"},
		{"a .method parameter, PHP's _method", underKind, "/v1/things?.method=DELETE", false, "request carries a method override parameter"},
		{"a [method parameter, PHP's _method", underKind, "/v1/things?[method=DELETE", false, "request carries a method override parameter"},
		{"a +_method parameter, a space before _method", underKind, "/v1/things?+_method=DELETE", false, "request carries a method override parameter"},
		{"a %20_method parameter", underKind, "/v1/things?%20_method=DELETE", false, "request carries a method override parameter"},
		{"a parameter name that does not decode", underKind, "/v1/things?_method%zz=1", false, "request carries a method override parameter"},
		{"a _method parameter cut at a NUL, PHP's _method", underKind, "/v1/things?_method%00=PUT", false, "request carries a method override parameter"},
		{"a .method parameter cut at a NUL, PHP's _method", underKind, "/v1/things?.method%00x=PUT", false, "request carries a method override parameter"},
		{"a _method parameter after a ;, PHP's arg_separator.input", underKind, "/v1/things?x;_method=DELETE", false, "request carries a method override parameter"},
		{"a parameter on a .. segment", underKind, "/v1/..;/admin", false, "path has a parameter on a dot segment"},
		{"a parameter after a .. segment's text", underKind, "/v1/..;foo/x", false, "path has a parameter on a dot segment"},
		{"a parameter on a . segment", underKind, "/v1/.;/x", false, "path has a parameter on a dot segment"},
		{"an encoded ; on a .. segment", underKind, "/v1/..%3B/admin", false, "path has a parameter on a dot segment"},
		{"an encoded .. segment with a parameter", underKind, "/v1/%2e%2e;/admin", false, "path has a parameter on a dot segment"},
		{"a double-encoded slash", underKind, "/v1/ada%252Fproofs", false, "path has a double-encoded character"},
		{"a double-encoded dot", underKind, "/v1/%252e%252e/admin", false, "path has a double-encoded character"},
		{"a double-encoded backslash", underKind, "/v1/ada%255cproofs", false, "path has a double-encoded character"},
		{"a dot encoded twice, each character escaped", underKind, "/v1/%25%32%45%25%32%45/admin", false, "path has a double-encoded character"},
		{"a slash encoded twice, each character escaped", underKind, "/v1/%25%32%46x", false, "path has a double-encoded character"},
		{"a slash encoded twice, its letter escaped", underKind, "/v1/%252%46x", false, "path has a double-encoded character"},
		{"a slash encoded twice, its digit escaped", underKind, "/v1/%25%32fx", false, "path has a double-encoded character"},
		{"a slash encoded three times", underKind, "/v1/%25252fx", false, "path has a double-encoded character"},
		{"a parameter on a .. segment under the profile's allow alone", underProfile, "/v1/..;/admin", false, "path has a parameter on a dot segment"},
		{"the control: one slash", underKind, "/v1/things", false, ""},
		{"the control: a parameter whose name ends in _method", underKind, "/v1/things?x_method=PUT", false, ""},
		{"the control: a parameter whose name starts with _method", underKind, "/v1/things?_methods=PUT", false, ""},
		{"the control: a _method value", underKind, "/v1/things?a=_method", false, ""},
		{"the control: a +method parameter, a space before method", underKind, "/v1/things?+method=PUT", false, ""},
		{"the control: a ; in a segment that is no dot segment", underKind, "/v1/things;v=1", false, ""},
		{"no allow: an object name's encoded slash", underNone, "/storage/v1/b/b/o/dir%2Fobj", false, ""},
		{"no allow: an empty segment", underNone, "/a//b", false, ""},
		{"no allow: an encoded backslash", underNone, "/a%5Cb", false, ""},
		{"no allow: a _method parameter", underNone, "/v1/things?_method=PUT", false, ""},
		{"no allow: a _method parameter with an escaped letter", underNone, "/v1/things?_meth%6Fd=DELETE", false, ""},
		{"no allow: a _method parameter escaped whole", underNone, "/v1/things?%5F%6Detho%64=DELETE", false, ""},
		{"no allow: a .method parameter", underNone, "/v1/things?.method=DELETE", false, ""},
		{"no allow: a +_method parameter", underNone, "/v1/things?+_method=DELETE", false, ""},
		{"no allow: a %20_method parameter", underNone, "/v1/things?%20_method=DELETE", false, ""},
		{"no allow: a parameter name that does not decode", underNone, "/v1/things?_method%zz=1", false, ""},
		{"no allow: a _method parameter cut at a NUL", underNone, "/v1/things?_method%00=PUT", false, ""},
		{"no allow: a .method parameter cut at a NUL", underNone, "/v1/things?.method%00x=PUT", false, ""},
		{"no allow: a _method parameter after a ;", underNone, "/v1/things?x;_method=DELETE", false, ""},
		{"no allow: a parameter on a .. segment", underNone, "/v1/..;/admin", false, ""},
		{"no allow: an encoded ; on a .. segment", underNone, "/v1/..%3B/admin", false, ""},
		{"no allow: a double-encoded slash", underNone, "/v1/ada%252Fproofs", false, ""},
		{"no allow: a double-encoded backslash", underNone, "/v1/ada%255cproofs", false, ""},
		{"no allow: a dot encoded twice, each character escaped", underNone, "/v1/%25%32%45%25%32%45/admin", false, ""},
		{"no allow: a slash encoded twice, each character escaped", underNone, "/v1/%25%32%46x", false, ""},
		{"no allow: a slash encoded twice, its letter escaped", underNone, "/v1/%252%46x", false, ""},
		{"no allow: a slash encoded twice, its digit escaped", underNone, "/v1/%25%32fx", false, ""},
		{"no allow: a slash encoded three times", underNone, "/v1/%25252fx", false, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRuledRig(t, row.rules, Options{})
			// What the service was sent, as it read the request line: the path as escaped on the
			// wire, and the query.
			var forwarded []string
			var mu sync.Mutex
			r.answerWith(func(_ http.ResponseWriter, request *http.Request, _ proxyrulestest.Request) bool {
				mu.Lock()
				defer mu.Unlock()
				target := request.URL.EscapedPath()
				if request.URL.RawQuery != "" {
					target += "?" + request.URL.RawQuery
				}
				forwarded = append(forwarded, target)
				return false
			})
			request, err := http.NewRequest(http.MethodGet, "https://"+boundHost+row.target, nil)
			if row.literal {
				request, err = http.NewRequest(http.MethodGet, "https://"+boundHost+"/", nil)
				if err == nil {
					request.URL.Opaque = row.target
				}
			}
			if err != nil {
				t.Fatalf("build GET %s: %v", row.target, err)
			}
			request.Header.Set("Authorization", "Bearer "+workerPlaceholder)
			answer, err := r.client(workerPlaceholder).Do(request)
			if err != nil {
				t.Fatalf("GET %s through the proxy: %v", row.target, err)
			}
			body := bodyOf(t, answer)
			mu.Lock()
			defer mu.Unlock()
			if row.reason == "" {
				if answer.StatusCode != http.StatusOK || len(forwarded) != 1 || forwarded[0] != row.target {
					t.Fatalf("GET %s answered %d %q and the service was sent %q; want 200 and the request as the command wrote it", row.target, answer.StatusCode, body, forwarded)
				}
				if got := r.requests()[0].Header.Get("Authorization"); got != "Bearer "+workerValue {
					t.Errorf("the service was sent Authorization %q; want the value swapped in", got)
				}
				return
			}
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, row.reason) {
				t.Errorf("GET %s answered %d %q; want 403 naming %q", row.target, answer.StatusCode, body, row.reason)
			}
			if lines := r.logs.refusals(); len(lines) != 1 || lines[0].fields["code"] != codeNotAllowed || lines[0].fields["reason"] != row.reason {
				t.Errorf("the refusal logged %+v; want one line, %s, %q", lines, codeNotAllowed, row.reason)
			}
			if len(forwarded) != 0 {
				t.Errorf("the service was sent %q; want nothing", forwarded)
			}
		})
	}
}

// TestAProfilesAllowNarrowsItsKind: a name's binding carries its profile's own allow, and the proxy
// forwards a request only when the kind's allow and the profile's both allow it: a row the kind
// allows and the profile does not is refused "not allowed by the profile's rule", a row the kind
// does not allow is refused by the kind's rule first, a present empty list allows nothing, and a
// method override header is refused where the profile alone has an allow. Nothing refused reaches
// the service.
//
// The named revert: drop the profile's allow from the decision (proxyrules.check's second
// allowOK), and the narrowed rows are carried.
func TestAProfilesAllowNarrowsItsKind(t *testing.T) {
	reports := []proxyrules.Allow{{Method: http.MethodGet, PathPrefix: "/v1/reports/"}}
	for _, row := range []struct {
		name string
		// anyKind takes the kind's allow away, where testRules' kind allows GET and POST under
		// /v1/; profile is the binding's own allow.
		anyKind        bool
		profile        []proxyrules.Allow
		method, path   string
		headers        []string
		reason         string
		carriedAsValue bool
	}{
		{name: "a row both allow", profile: reports, method: http.MethodGet, path: "/v1/reports/7", carriedAsValue: true},
		{name: "a path the kind allows and the profile does not", profile: reports, method: http.MethodGet, path: "/v1/things", reason: "not allowed by the profile's rule"},
		{name: "a method the kind allows and the profile does not", profile: reports, method: http.MethodPost, path: "/v1/reports/7", reason: "not allowed by the profile's rule"},
		{name: "a path the kind does not allow", profile: reports, method: http.MethodGet, path: "/v2/reports/7", reason: "not allowed by rule"},
		{name: "a present empty list", profile: []proxyrules.Allow{}, method: http.MethodGet, path: "/v1/reports/7", reason: "not allowed by the profile's rule"},
		{name: "a method override under the profile's allow alone", anyKind: true, profile: reports, method: http.MethodGet, path: "/v1/reports/7",
			headers: []string{"X-HTTP-Method-Override", http.MethodDelete}, reason: "request carries a method override header"},
		{name: "the control: no profile allow, the kind's rows alone", method: http.MethodPost, path: "/v1/things", carriedAsValue: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			rules := testRules()
			if row.anyKind {
				rules.Kinds[0].Allow = nil
			}
			rules.Names[0].Allow = row.profile
			r := newRuledRig(t, rules, Options{})
			answer, body := r.do(r.client(workerPlaceholder), row.method, "https://"+boundHost+row.path, "",
				append([]string{"Authorization", "Bearer " + workerPlaceholder}, row.headers...)...)
			if row.carriedAsValue {
				seen := r.requests()
				if answer.StatusCode != http.StatusOK || len(seen) != 1 || seen[0].Header.Get("Authorization") != "Bearer "+workerValue {
					t.Fatalf("%s %s answered %d %q; want 200 with the value swapped in", row.method, row.path, answer.StatusCode, body)
				}
				return
			}
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, row.reason) {
				t.Errorf("%s %s answered %d %q; want 403 naming %q", row.method, row.path, answer.StatusCode, body, row.reason)
			}
			if lines := r.logs.refusals(); len(lines) != 1 || lines[0].fields["reason"] != row.reason {
				t.Errorf("the refusal logged %+v; want one line naming %q", lines, row.reason)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Errorf("the service saw %d calls; want none", len(seen))
			}
		})
	}
}

// TestAKindsOwnPlaceholderPrefixIsReadBeforeTheSwap: SC14 reads a kind's own placeholder_prefix
// in the request the command sent, outside the header the swap replaces. A whole tml- placeholder
// in X-Other is refused and reaches no service; the boundary control, html- with the same 32
// characters, reaches the service because the tml- occurrence has a letter before it. The named
// revert: remove the kind-prefix left-boundary check, which refuses the control.
func TestAKindsOwnPlaceholderPrefixIsReadBeforeTheSwap(t *testing.T) {
	const (
		prefix   = "tml-"
		suffix   = "0123456789abcdef0123456789abcdef"
		leftover = prefix + suffix
		real     = prefix + "realrealrealrealrealrealrealreal"
	)
	rules := testRules()
	rules.Kinds[0].PlaceholderPrefix = prefix
	for _, row := range []struct {
		name        string
		leftover    string
		wantRefusal bool
	}{
		{"a whole kind placeholder", leftover, true},
		{"the left-boundary control", "h" + leftover, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRuledRig(t, rules, Options{})
			r.grants.setValues(testGrantID, storedValues(workerName, real))
			answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things",
				"Authorization", "Bearer "+workerPlaceholder, "X-Other", row.leftover)
			if row.wantRefusal {
				if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "placeholder") {
					t.Fatalf("the request carrying %q answered %d %q; want 403 naming the leftover placeholder", row.leftover, answer.StatusCode, body)
				}
				if seen := r.requests(); len(seen) != 0 {
					t.Fatalf("the service saw %d requests; want none", len(seen))
				}
				return
			}
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("the request carrying %q answered %d %q; want 200", row.leftover, answer.StatusCode, body)
			}
			if seen := r.requests(); len(seen) != 1 || seen[0].Header.Get("Authorization") != "Bearer "+real || seen[0].Header.Get("X-Other") != row.leftover {
				t.Fatalf("the service saw %+v; want one swapped Authorization and X-Other %q, carried through", seen, row.leftover)
			}
		})
	}
}
