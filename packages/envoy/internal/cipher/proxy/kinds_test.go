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
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
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

// TestAPathTwoReadersResolveApartIsRefused: an allow prefix bounds the path the service reads,
// and a service that merges "//" or decodes "%2F" reads another path than the one the prefix
// holds, so the proxy refuses a path with an empty segment and a raw path with an encoded slash,
// 403 in the decision's own words, before anything is sent. Each path is under the kind's /v1/,
// so the rule that refuses it is the path rule and not the allow list. The control: the same
// path with one slash is carried.
func TestAPathTwoReadersResolveApartIsRefused(t *testing.T) {
	for _, row := range []struct{ path, reason string }{
		{"/v1//things", "path has an empty segment"},
		{"/v1/things//", "path has an empty segment"},
		{"/v1/ada%2Fproofs", "path has an encoded slash"},
		{"/v1/ada%2fproofs", "path has an encoded slash"},
		{"/v1/things", ""},
	} {
		t.Run(row.path, func(t *testing.T) {
			r := newRig(t, Options{})
			answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+row.path, "Authorization", "Bearer "+workerPlaceholder)
			if row.reason == "" {
				if answer.StatusCode != http.StatusOK || len(r.requests()) != 1 {
					t.Fatalf("the control %s answered %d %q with %d calls sent; want 200 and one", row.path, answer.StatusCode, body, len(r.requests()))
				}
				return
			}
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, row.reason) {
				t.Errorf("GET %s answered %d %q; want 403 naming %q", row.path, answer.StatusCode, body, row.reason)
			}
			if lines := r.logs.refusals(); len(lines) != 1 || lines[0].fields["code"] != codeNotAllowed || lines[0].fields["reason"] != row.reason {
				t.Errorf("the refusal logged %+v; want one line, %s, %q", lines, codeNotAllowed, row.reason)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Errorf("the service saw %d calls; want none", len(seen))
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
