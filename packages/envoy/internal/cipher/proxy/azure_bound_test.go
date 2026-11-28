// packages/envoy/internal/cipher/proxy/azure_bound_test.go
//
// The bound on the exchange way: the federated token is the broker pod's own, and
// Entra takes it as the assertion of every application whose federated credential trusts the
// broker, so the proxy exchanges it only at the token authority, only as the application and
// only in the tenant the grant's own secret document names, in a form of its own, and sends it
// in nothing but that exchange. Every row counts what reached the stand-in token endpoint,
// because a refusal that still made the exchange would be no bound at all.
package proxy

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// The application, the tenant and the scope a row names that the document does not: another
// application federated with the broker's identity, the tenant it lives in, and another resource
// the document's own application asks a token for. azureAuthorityHost is a token authority that
// is not the rig's bound host: a row that names it makes that bound host the kind's second
// one, the service an access token is spent at, where no exchange may be made.
const (
	azureOtherClientID = "99999999-8888-7777-6666-000000000000"
	azureOtherTenantID = "another-tenant-id"
	azureOtherScope    = "https://graph.example.com/.default"
	azureResource      = "https://management.example.com/"
	azureAuthorityHost = "login.example.net"
)

// azureRulesBindingAnAuthority is azureRules with the azure kind bound to two hosts:
// azureAuthorityHost, the token authority, and the rig's bound host beside it.
func azureRulesBindingAnAuthority() proxyrules.Rules {
	rules := azureRules()
	rules.Kinds[0].Hosts = []string{azureAuthorityHost, boundHost}
	return rules
}

// boundRefusal fails t unless answer is a 403 whose words name the entry and field and carry
// neither the real federated token, nor stored, the value the bound holds that field to (the
// document's id, or the authority's host), nor sent, the one the request carried instead, and
// unless the one refusal record the proxy wrote for the call carries code and gives the same
// reason. Neither ids nor hosts are secrets, but a refusal is written to the command and to the
// broker's log, and neither is a place an id - or whatever a command put in that field - is to
// be copied to. A failure names what leaked and never prints it, and prints words only when they
// carry none of the three.
func boundRefusal(t *testing.T, r *rig, answer *http.Response, body, code, field, stored, sent string) {
	t.Helper()
	// refuse writes its record before the answer, so a row that has its answer reads it at once.
	var reason string
	if records := r.logs.refusals(); len(records) != 1 {
		t.Errorf("the proxy wrote %d refusal records; want one", len(records))
	} else {
		reason, _ = records[0].fields["reason"].(string)
		if got := records[0].fields["code"]; got != code {
			t.Errorf("the refusal record's code is %v; want %s", got, code)
		}
	}
	never := []struct{ what, value string }{
		{"the real federated token", azureRealFederated},
		{"the bound's " + field, stored},
		{"the request's " + field, sent},
	}
	for _, place := range []struct{ what, words string }{{"the answer", body}, {"the logged reason", reason}} {
		clean := true
		for _, n := range never {
			if n.value != "" && strings.Contains(place.words, n.value) {
				clean = false
				t.Errorf("%s carries %s; want neither the federated token nor either value of %s", place.what, n.what, field)
			}
		}
		if clean && (!strings.Contains(place.words, azureName) || !strings.Contains(place.words, field)) {
			t.Errorf("%s %q does not name the entry %s and the field %s", place.what, place.words, azureName, field)
		}
	}
	// Not fatal: the row's noExchange then says what reached the endpoint as well.
	if answer.StatusCode != http.StatusForbidden {
		t.Errorf("the request answered %d; want 403", answer.StatusCode)
	}
}

// noExchange fails t unless nothing reached the token endpoint or any other part of the bound
// host, and the broker was asked for no access placeholder.
func noExchange(t *testing.T, r *rig, stand *proxyrulestest.MicrosoftStand) {
	t.Helper()
	if sent := stand.Exchanges(); len(sent) != 0 {
		t.Errorf("the token endpoint was sent %d exchanges; want none", len(sent))
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Errorf("the bound host was sent %d requests; want none", len(seen))
	}
	if mints := r.grants.accessMints(); mints != 0 {
		t.Errorf("the broker was asked for %d access placeholders; want none", mints)
	}
}

// sentForm fails t unless sent, the form the token endpoint was sent, is want field for field
// with the real federated token as its assertion, and holds no field want does not. A failure
// names the fields and prints no assertion.
func sentForm(t *testing.T, sent, want url.Values) {
	t.Helper()
	wanted := append(slices.Collect(maps.Keys(want)), "client_assertion")
	slices.Sort(wanted)
	if got := slices.Sorted(maps.Keys(sent)); !slices.Equal(got, wanted) {
		t.Errorf("the endpoint was sent the fields %v; want exactly %v", got, wanted)
	}
	if !slices.Equal(sent["client_assertion"], []string{azureRealFederated}) {
		t.Errorf("the endpoint was not sent the real federated token, and it alone, as the assertion")
	}
	for field, values := range want {
		if !slices.Equal(sent[field], values) {
			t.Errorf("the endpoint was sent %s = %q; want %q", field, sent[field], values)
		}
	}
}

// TestAzureExchangeIsBoundToItsDocument: a command's token request is exchanged when it is
// posted to the token authority and its client_id and the tenant of the endpoint it posts to are
// the document's, whatever scope it asks for, since one application asks for tokens of several
// resources. One that names another application, posts to another tenant's endpoint, or posts
// to another host the entry binds, is refused before the broker puts its federated token in the
// form, and nothing reaches the endpoint.
func TestAzureExchangeIsBoundToItsDocument(t *testing.T) {
	for _, row := range []struct {
		name string
		path string
		// authority is the token authority the proxy is given, "" for the bound host the
		// request goes to; a row that names another binds both (azureRulesBindingAnAuthority).
		authority string
		// form writes the row's own fields over tokenForm's.
		form func(url.Values)
		// scope is the scope the exchange the endpoint is sent carries, for a row that is
		// exchanged; field is the field a refused row's refusal names, and stored and sent
		// the value the bound holds it to and the request's, which that refusal must carry
		// neither of.
		scope, field, stored, sent string
	}{
		{
			name:  "the document's own application and tenant",
			path:  azureTokenPath,
			scope: azureScope,
		},
		{
			name:  "another scope, as the document's application and in its tenant",
			path:  azureTokenPath,
			form:  func(f url.Values) { f.Set("scope", azureOtherScope) },
			scope: azureOtherScope,
		},
		{
			name:  "another application",
			path:  azureTokenPath,
			form:  func(f url.Values) { f.Set("client_id", azureOtherClientID) },
			field: "client_id", stored: azureClientID, sent: azureOtherClientID,
		},
		{
			// Of two client_ids the endpoint and not the proxy would choose the one that
			// counts, so the document's beside another is no bound.
			name:  "the document's application and another one",
			path:  azureTokenPath,
			form:  func(f url.Values) { f.Add("client_id", azureOtherClientID) },
			field: "client_id", stored: azureClientID, sent: azureOtherClientID,
		},
		{
			name:  "no application at all",
			path:  azureTokenPath,
			form:  func(f url.Values) { f.Del("client_id") },
			field: "client_id", stored: azureClientID,
		},
		{
			name:  "another tenant",
			path:  "/" + azureOtherTenantID + "/oauth2/v2.0/token",
			field: "tenant", stored: azureTenantID, sent: azureOtherTenantID,
		},
		{
			name:  "another tenant on the v1 endpoint",
			path:  "/" + azureOtherTenantID + "/oauth2/token",
			field: "tenant", stored: azureTenantID, sent: azureOtherTenantID,
		},
		{
			// A tenant that only opens with the document's is another tenant: the path is read
			// whole, not by its prefix.
			name:  "a tenant that opens with the document's own",
			path:  "/" + azureTenantID + "-and-more/oauth2/v2.0/token",
			field: "tenant", stored: azureTenantID,
		},
		{
			// Every id is the document's and the host is one the entry binds, and the exchange
			// would still hand that host the federated token: it is not the token authority.
			name:      "another host the kind binds",
			path:      azureTokenPath,
			authority: azureAuthorityHost,
			field:     "host", stored: azureAuthorityHost, sent: boundHost,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rules, o := azureRules(), Options{}
			if row.authority != "" {
				rules, o = azureRulesBindingAnAuthority(), Options{AzureAuthority: row.authority}
			}
			r, stand := newAzureRuledRig(t, rules, o)
			form := tokenForm(azureFederatedPlaceholder)
			if row.form != nil {
				row.form(form)
			}
			answer, body := tokenRequestAt(t, r, r.client(azureFederatedPlaceholder), row.path, form)

			if row.field != "" {
				boundRefusal(t, r, answer, body, codeTokenApplication, row.field, row.stored, row.sent)
				noExchange(t, r, stand)
				return
			}
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("the exchange answered %d %q; want 200", answer.StatusCode, body)
			}
			placeholderTokenOf(t, body)
			exchanges := stand.Exchanges()
			if len(exchanges) != 1 {
				t.Fatalf("the endpoint was sent %d exchanges; want one", len(exchanges))
			}
			if seen := r.requests(); len(seen) != 1 || seen[0].Method != http.MethodPost || seen[0].Path != row.path {
				t.Fatalf("the bound host saw %d requests; want the one exchange, a POST to the document's tenant's endpoint %s", len(seen), row.path)
			}
			sent := exchanges[0]
			if sent.Get("client_assertion") != azureRealFederated {
				t.Errorf("the endpoint was not sent the real federated token as the assertion")
			}
			if got := sent.Get("client_id"); got != azureClientID {
				t.Errorf("the endpoint was sent the client_id %q; want the document's %q", got, azureClientID)
			}
			if got := sent.Get("scope"); got != row.scope {
				t.Errorf("the endpoint was sent the scope %q; want the command's own %q", got, row.scope)
			}
			if mints := r.grants.accessMints(); mints != 1 {
				t.Errorf("the broker was asked for %d access placeholders; want one", mints)
			}
		})
	}
}

// TestAnExchangeSendsTheEndpointAFormOfItsOwn: the form the token endpoint is sent is the
// exchange's own - the federated flow's two constants, the document's client_id, the command's
// scope (and its resource at the v1 endpoint, which reads one in place of a scope) and the real
// federated token - and nothing else the command wrote reaches it: not a second client_id in
// another case, which the bound does not read and an endpoint that reads keys without regard to
// case might, not a field the proxy does not know, and not constants of the command's own.
func TestAnExchangeSendsTheEndpointAFormOfItsOwn(t *testing.T) {
	flow := func(fields ...string) url.Values {
		want := url.Values{
			"grant_type":            {"client_credentials"},
			"client_id":             {azureClientID},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		}
		for i := 0; i+1 < len(fields); i += 2 {
			want.Set(fields[i], fields[i+1])
		}
		return want
	}
	for _, row := range []struct {
		name string
		path string
		// form writes the row's own fields over tokenForm's.
		form func(url.Values)
		// want is every field the endpoint must be sent but the assertion, which is the real
		// federated token on every row.
		want url.Values
	}{
		{
			name: "a second client_id in another case and a field the exchange does not know",
			path: azureTokenPath,
			form: func(f url.Values) {
				f.Set("CLIENT_ID", azureOtherClientID)
				f.Set("foo", "bar")
			},
			want: flow("scope", azureScope),
		},
		{
			name: "the flow's two constants written as the command's own",
			path: azureTokenPath,
			form: func(f url.Values) {
				f.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
				f.Set("client_assertion_type", "urn:the-command:assertion")
			},
			want: flow("scope", azureScope),
		},
		{
			// The v2.0 endpoint reads a scope and no resource.
			name: "a resource at the v2.0 endpoint",
			path: azureTokenPath,
			form: func(f url.Values) { f.Set("resource", azureResource) },
			want: flow("scope", azureScope),
		},
		{
			name: "a resource at the v1 endpoint, beside a field the exchange does not know",
			path: "/" + azureTenantID + "/oauth2/token",
			form: func(f url.Values) {
				f.Del("scope")
				f.Set("resource", azureResource)
				f.Set("foo", "bar")
			},
			want: flow("resource", azureResource),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, stand := newAzureRig(t, Options{})
			form := tokenForm(azureFederatedPlaceholder)
			row.form(form)
			answer, body := tokenRequestAt(t, r, r.client(azureFederatedPlaceholder), row.path, form)
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("the exchange answered %d %q; want 200", answer.StatusCode, body)
			}
			exchanges := stand.Exchanges()
			if len(exchanges) != 1 {
				t.Fatalf("the endpoint was sent %d exchanges; want one", len(exchanges))
			}
			sentForm(t, exchanges[0], row.want)
		})
	}
}

// TestAnAccessSlotOfAnotherApplicationOrTenantIsNotExchanged: a pod that holds no token of an
// access slot exchanges for itself from what the slot carries, so the slot is held to the token
// authority and the document as the command's own request is. A slot is a row of the broker's
// own, which only an exchange the bound let through writes - but a row written before the bound
// existed lives as long as its grant, and is refused all the same, with nothing reaching the
// endpoint or the service. A slot of the v1 endpoint carries the resource that endpoint reads,
// and the pod's own exchange asks for it.
func TestAnAccessSlotOfAnotherApplicationOrTenantIsNotExchanged(t *testing.T) {
	for _, row := range []struct {
		name     string
		exchange azureExchange
		// authority is the token authority the proxy is given, "" for the rig's bound host; a
		// row that names another binds both (azureRulesBindingAnAuthority).
		authority           string
		field, stored, sent string
		// want is every field the pod's own exchange must send but the assertion, for a row
		// that is exchanged.
		want url.Values
	}{
		{
			name:     "the document's own application and tenant",
			exchange: azureExchange{Endpoint: "https://" + boundHost + azureTokenPath, Scope: azureScope, ClientID: azureClientID},
			want: url.Values{
				"grant_type": {"client_credentials"}, "client_id": {azureClientID}, "scope": {azureScope},
				"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			},
		},
		{
			name:     "the document's own application and tenant at the v1 endpoint",
			exchange: azureExchange{Endpoint: "https://" + boundHost + "/" + azureTenantID + "/oauth2/token", Resource: azureResource, ClientID: azureClientID},
			want: url.Values{
				"grant_type": {"client_credentials"}, "client_id": {azureClientID}, "resource": {azureResource},
				"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			},
		},
		{
			name:     "another application",
			exchange: azureExchange{Endpoint: "https://" + boundHost + azureTokenPath, Scope: azureScope, ClientID: azureOtherClientID},
			field:    "client_id", stored: azureClientID, sent: azureOtherClientID,
		},
		{
			name:     "another tenant",
			exchange: azureExchange{Endpoint: "https://" + boundHost + "/" + azureOtherTenantID + "/oauth2/v2.0/token", Scope: azureScope, ClientID: azureClientID},
			field:    "tenant", stored: azureTenantID, sent: azureOtherTenantID,
		},
		{
			name:      "another host the kind binds",
			exchange:  azureExchange{Endpoint: "https://" + boundHost + azureTokenPath, Scope: azureScope, ClientID: azureClientID},
			authority: azureAuthorityHost,
			field:     "host", stored: azureAuthorityHost, sent: boundHost,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rules, o := azureRules(), Options{}
			if row.authority != "" {
				rules, o = azureRulesBindingAnAuthority(), Options{AzureAuthority: row.authority}
			}
			r, stand := newAzureRuledRig(t, rules, o)
			access := placeholder.Prefix + "slotAccessSlotAccessSlotAccess01"
			r.grants.connections[azureFederatedPlaceholder].Names[azureName].Slots[azureAccessSlot(row.exchange)] = access
			answer, body := r.get(r.client(azureFederatedPlaceholder), "https://"+boundHost+"/v1/subscriptions", "Authorization", "Bearer "+access)

			if row.field != "" {
				boundRefusal(t, r, answer, body, codeAccessToken, row.field, row.stored, row.sent)
				noExchange(t, r, stand)
				return
			}
			seen := r.requests()
			if len(seen) != 2 {
				t.Fatalf("the host saw %d calls; want the pod's own exchange and the API call", len(seen))
			}
			if seen[1].Header.Get("Authorization") != "Bearer "+azureRealAccess {
				t.Fatalf("the service was not sent the real access token the pod exchanged for itself")
			}
			exchanges := stand.Exchanges()
			if len(exchanges) != 1 {
				t.Fatalf("the endpoint was sent %d exchanges; want one", len(exchanges))
			}
			sentForm(t, exchanges[0], row.want)
		})
	}
}

// TestTheFederatedTokenLeavesOnlyInAnExchange: Entra takes the federated token as the assertion
// of every application federated with the broker, so the token leaves the broker in an exchange
// at the token authority and in nothing else. A request that is no token request and carries
// its placeholder is refused before anything is sent, whether it goes to the authority or to
// another host its kind binds, whether or not the grant holds an access slot beside it, and
// whatever way the binding of the name takes - swap, which a stored value's binding takes, or
// swap read under Options.NoKind - since the azure-login value fills the federated slot under
// every way; a request that carries an access token's placeholder is swapped as every other
// way's is; and one that carries no placeholder, which is how az reads a tenant's OpenID
// configuration before its first token request, goes on with nothing swapped into it.
func TestTheFederatedTokenLeavesOnlyInAnExchange(t *testing.T) {
	access := placeholder.Prefix + "slotAccessSlotAccessSlotAccess01"
	own := azureExchange{Endpoint: "https://" + boundHost + azureTokenPath, Scope: azureScope, ClientID: azureClientID}
	federated := []string{"Authorization", "Bearer " + azureFederatedPlaceholder}
	for _, row := range []struct {
		name string
		// authority is the token authority the proxy is given, "" for the rig's bound host; a
		// row that names another binds both (azureRulesBindingAnAuthority) and sends its
		// request to the bound host, the kind's other one.
		authority string
		// slot is whether the grant holds an access slot of the document's own exchange, whose
		// placeholder is access.
		slot bool
		// swapped is whether the rig's binding takes the way swap rather than exchange, and
		// noKind whether the proxy reads every way as a swap (Options.NoKind, check k6's
		// negative control).
		swapped, noKind bool
		path            string
		headers         []string
		// refused is whether the call is refused with nothing sent. For one that is not, calls
		// is how many calls reach the bound host, the pod's own exchange among them, and
		// authorization the Authorization the last of them carries.
		refused       bool
		calls         int
		authorization string
	}{
		{name: "the federated placeholder, to the token authority", path: "/v1/subscriptions", headers: federated, refused: true},
		{name: "the federated placeholder, to the kind's other host", authority: azureAuthorityHost, path: "/v1/subscriptions", headers: federated, refused: true},
		{name: "the federated placeholder, beside an access slot", slot: true, path: "/v1/subscriptions", headers: federated, refused: true},
		{name: "the federated placeholder, to a token endpoint's path but no POST", path: azureTokenPath, headers: federated, refused: true},
		{name: "the federated placeholder, under a binding of way swap", swapped: true, path: "/v1/subscriptions", headers: federated, refused: true},
		{name: "the federated placeholder, under a binding of way swap, to the kind's other host", swapped: true, authority: azureAuthorityHost, path: "/v1/subscriptions", headers: federated, refused: true},
		{name: "the federated placeholder, with every way read as a swap", noKind: true, path: "/v1/subscriptions", headers: federated, refused: true},
		{
			name: "an access token's placeholder", slot: true, path: "/v1/subscriptions",
			headers: []string{"Authorization", "Bearer " + access}, calls: 2, authorization: "Bearer " + azureRealAccess,
		},
		{name: "no placeholder, as az's read of the tenant's OpenID configuration", path: "/" + azureTenantID + "/v2.0/.well-known/openid-configuration", calls: 1},
		{name: "no placeholder, under a binding of way swap", swapped: true, path: "/" + azureTenantID + "/v2.0/.well-known/openid-configuration", calls: 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			rules, o := azureRules(), Options{NoKind: row.noKind}
			if row.authority != "" {
				rules, o = azureRulesBindingAnAuthority(), Options{AzureAuthority: row.authority}
			}
			if row.swapped {
				rules.Names[0].Way = proxyrules.WaySwap
			}
			r, stand := newAzureRuledRig(t, rules, o)
			if row.slot {
				r.grants.connections[azureFederatedPlaceholder].Names[azureName].Slots[azureAccessSlot(own)] = access
			}
			answer, body := r.get(r.client(azureFederatedPlaceholder), "https://"+boundHost+row.path, row.headers...)

			if row.refused {
				if answer.StatusCode != http.StatusForbidden {
					t.Errorf("the call answered %d; want 403", answer.StatusCode)
				}
				var reason string
				if records := r.logs.refusals(); len(records) != 1 {
					t.Errorf("the proxy wrote %d refusal records; want one", len(records))
				} else {
					reason, _ = records[0].fields["reason"].(string)
					if got := records[0].fields["code"]; got != codeFederatedSwap {
						t.Errorf("the refusal record's code is %v; want %s", got, codeFederatedSwap)
					}
				}
				for _, place := range []struct{ what, words string }{{"the answer", body}, {"the logged reason", reason}} {
					if strings.Contains(place.words, azureRealFederated) || strings.Contains(place.words, azureFederatedPlaceholder) {
						t.Errorf("%s carries the federated token or its placeholder", place.what)
					} else if !strings.Contains(place.words, azureName) || !strings.Contains(place.words, "federated token") {
						t.Errorf("%s %q does not name the secret %s and its federated token", place.what, place.words, azureName)
					}
				}
				noExchange(t, r, stand)
				return
			}
			seen := r.requests()
			if len(seen) != row.calls {
				t.Fatalf("the bound host saw %d calls; want %d", len(seen), row.calls)
			}
			for _, call := range seen {
				for name, values := range call.Header {
					for _, value := range values {
						if strings.Contains(value, azureRealFederated) {
							t.Errorf("the bound host was sent the real federated token in %s", name)
						}
					}
				}
			}
			if got := seen[len(seen)-1].Header.Get("Authorization"); got != row.authorization {
				t.Errorf("the service was sent Authorization %q; want %q", got, row.authorization)
			}
		})
	}
}

// TestAzureBoundReadsTheWholePathAndTheDocumentsOwnIDs: the bound itself, on what no request
// through the proxy reaches it with. A dot segment the decision refuses first in a command's own
// request, but a slot's endpoint never passes the decision; a document that names no tenant or
// no application the release refuses to split, and the bound holds an exchange to no empty id
// all the same; and a host is the authority's only when it is that host exactly, with no port.
func TestAzureBoundReadsTheWholePathAndTheDocumentsOwnIDs(t *testing.T) {
	app := secrets.AzureLoginValue{TenantID: azureTenantID, ClientID: azureClientID}
	one := []string{azureClientID}
	for _, row := range []struct {
		name      string
		authority string
		app       secrets.AzureLoginValue
		host      string
		clientIDs []string
		path      string
		// field is the field the refusal names, "" for an exchange the bound lets through.
		field string
	}{
		{"the v2.0 endpoint of the document's tenant", boundHost, app, boundHost, one, azureTokenPath, ""},
		{"the v1 endpoint of the document's tenant", boundHost, app, boundHost, one, "/" + azureTenantID + "/oauth2/token", ""},
		{"another tenant behind the document's own and a dot segment", boundHost, app, boundHost, one, "/" + azureTenantID + "/../" + azureOtherTenantID + "/oauth2/v2.0/token", "tenant"},
		{"a segment before the document's tenant", boundHost, app, boundHost, one, "/" + azureOtherTenantID + "/" + azureTenantID + "/oauth2/v2.0/token", "tenant"},
		{"the document's tenant in another case", boundHost, app, boundHost, one, "/" + strings.ToUpper(azureTenantID) + "/oauth2/v2.0/token", "tenant"},
		{"another endpoint of the document's tenant", boundHost, app, boundHost, one, "/" + azureTenantID + "/oauth2/v2.0/devicecode", "tenant"},
		{"no client_id", boundHost, app, boundHost, nil, azureTokenPath, "client_id"},
		{"a document that names no application", boundHost, secrets.AzureLoginValue{TenantID: azureTenantID}, boundHost, []string{""}, azureTokenPath, "client_id"},
		{"a document that names no tenant", boundHost, secrets.AzureLoginValue{ClientID: azureClientID}, boundHost, one, "//oauth2/v2.0/token", "tenant"},
		{"another host", azureAuthorityHost, app, boundHost, one, azureTokenPath, "host"},
		{"the authority's host on another port", boundHost, app, boundHost + ":8443", one, azureTokenPath, "host"},
		{"a host that ends with the authority's", boundHost, app, "login." + boundHost, one, azureTokenPath, "host"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := azureBound(azureName, row.authority, row.app, row.host, row.clientIDs, row.path)
			if row.field == "" {
				if err != nil {
					t.Fatalf("azureBound = %v; want the exchange let through", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("azureBound let the exchange through; want a refusal naming %s", row.field)
			}
			if words := err.Error(); !strings.Contains(words, azureName) || !strings.Contains(words, row.field) ||
				strings.Contains(words, azureTenantID) || strings.Contains(words, azureClientID) || strings.Contains(words, azureOtherTenantID) ||
				strings.Contains(words, boundHost) || strings.Contains(words, azureAuthorityHost) {
				t.Errorf("azureBound = %q; want the entry and the field named and no id or host of the document's, the authority's or the request's", words)
			}
		})
	}
}
