package api

// authorities_v2.go holds a /api/v2 request to the authority each state, field and query value
// needs (R1.8 of dispatch://INFRA-2946/spec; R1 of dispatch://INFRA-4071/spec). A route's own
// scopes, which every request to it needs, stay in its OAuthScopes column and gateRouteScopes; a
// scope only one value of a request needs is a rule of its Authorities column, and
// gateV2Authorities refuses a request whose credential lacks it before the handler runs, so the
// table, not a handler, says who may do what.
//
// Each route declares its rules on its own row, each one authorityRule. A state is a value of the
// body member `state`, a field a body member's presence, a query value a value of a query key:
//
//	Authorities: []authorityRule{
//		{Where: authorityWhere{In: partBody, Name: "state"}, Value: "answered", Scopes: routeScopeSet{model.ScopePerson}},
//		{Where: authorityWhere{In: partBody, Name: "asked_of"}, Scopes: routeScopeSet{model.ScopePerson}},
//		{Where: authorityWhere{In: partQuery, Name: "include"}, Value: "mark", Scopes: routeScopeSet{model.ScopePerson}},
//	},
//
// A check that depends on the data stays in the handler, and the row's description names it
// (dispatch://INFRA-2946/artifact/decisions-md, "The authority of each state, field and query
// value is in the table"): the `approve` scope of an answer to an approval ask (recordAnswer),
// the author-or-person rule of an ask's reword (rewordAsk), and the holder's `force` of a claim.
//
// A write to the rows of one session the path names is held to that session by
// withinSessionScope (the row's SessionPath). Where the path names a session only for one kind
// of object, or the body names several, the body's type names them (sessionAddressing), and this
// gate holds each to the session the caller's credential proved whatever OAuth scopes the row
// declares (refusesSessionOtherThanProved). Two rows declare such a body: PATCH
// /api/v2/me/marks/{kind}/{id}, whose {id} is a session when {kind} is `agent`, and PATCH
// /api/v2/me/marks, whose `agent` items each name one. Both need the person scope, and a broker
// token of one session that holds it marks, through that path or those items, that session's
// agent conversation alone: another session's is 403 SESSION_SCOPE (marksPatchV2Input). The
// filter form of PATCH /api/v2/me/marks (`{"filter": {"kind": ["agent"]}, "set": {...}}`,
// patchMarksByFilter) names no session, and sessionsAddressed names only the items, so that form
// marks the agent replies of every session, as /api/v1's POST /me/inbox/read and POST
// /me/inbox/dismiss do.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

// sessionAddressing is a decoded body, or one arm of a union, that names the sessions its write
// goes into where the row's path does not name one alone: the `agent` arm of PATCH
// /api/v2/me/marks/{kind}/{id}, whose {id} is a session only for that kind, answers the path's
// {id}, and the body of PATCH /api/v2/me/marks answers the session of each of its `agent` items.
// gateV2Authorities holds each session it names to the session the caller's credential proved,
// whatever OAuth scopes the row declares (refusesSessionOtherThanProved), so a credential that
// proved one session writes into no other's rows by naming it in a body. The decoder decodes a
// body as a value of its type, so the type implements it with a value receiver.
type sessionAddressing interface {
	sessionsAddressed(r *http.Request) []string
}

var sessionAddressingType = reflect.TypeFor[sessionAddressing]()

// bodyTypes is every type a body of this declaration is decoded into: its Type, or each arm of a
// union.
func bodyTypes(body requestBody) []reflect.Type {
	var types []reflect.Type
	for _, value := range append(slices.Collect(maps.Values(body.Arms)), body.Type) {
		if value != nil {
			types = append(types, reflect.TypeOf(value))
		}
	}
	return types
}

// addressesSessions reports whether a body of this declaration, or of one of its arms, names the
// sessions its write goes into (sessionAddressing).
func addressesSessions(body requestBody) bool {
	return slices.ContainsFunc(bodyTypes(body), func(typ reflect.Type) bool { return typ.Implements(sessionAddressingType) })
}

// gateV2Authorities refuses a request to a /api/v2 route whose credential lacks a scope one of its
// values needs (the row's Authorities), and a write whose body names a session the credential may
// not write into (sessionAddressing). register wraps it after the decoder has read the query and
// the body, and outside the keyed layer, so a refused write claims no idempotency key: the person
// who ticks the scope back is not answered the stored refusal under the same key. A missing scope
// is 403 OAUTH_SCOPE_REQUIRED naming the scope, under the challenge refuseMissingScope gives it; a
// session other than the one the credential proved is 403 SESSION_SCOPE naming both; and a caller
// that does not resolve at all is answered through writeAuthenticationError, as at the route's own
// gate. A route that declares no rule and whose body names no session is returned unwrapped and
// pays nothing.
func (s *server) gateV2Authorities(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	rules, sessions := route.Authorities, addressesSessions(route.Body)
	if len(rules) == 0 && !sessions {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		decoded, ok := decodedV2(r)
		if !ok {
			// register wraps this gate inside the decoder's layers alone, so a request that
			// reaches it undecoded is the server's own fault, and it is refused rather than let
			// past rules nothing could judge.
			s.writeHandlerError(w, r, fmt.Errorf("%s %s: the authority gate holds no decoded request", route.Method, route.Pattern))
			return
		}
		// The caller is resolved once a request, at the first rule that holds, and carried on the
		// request as the route's scope gate carries it (gateRouteScopes), so a second rule, every
		// layer behind this one and the handler read it rather than look a token up again.
		resolved := false
		for _, rule := range rules {
			if !rule.holdsFor(decoded) {
				continue
			}
			actor, human, err := s.optionalActor(r)
			if err != nil {
				s.writeAuthenticationError(w, r, err)
				return
			}
			if !resolved {
				r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
				resolved = true
			}
			for _, scope := range rule.Scopes {
				if !actor.Credential.Holds(scope) {
					refuseMissingScope(w, r, scope, fmt.Sprintf(
						"this request's credential does not hold the %s scope, which %s requires", scope, rule))
					return
				}
			}
		}
		if addressing, ok := decoded.body.(sessionAddressing); ok {
			for _, session := range addressing.sessionsAddressed(r) {
				if s.refusesSessionOtherThanProved(w, r, route, session) {
					return
				}
			}
		}
		handler(w, r)
	}
}

// holdsFor reports whether the rule's value is one decoded holds: a body member that has the
// rule's Value, a body member that is present at all where Value is empty, or a query key one of
// whose values (a member of a comma list among them) is Value, or that is present at all where
// Value is empty. A body member is matched as encoding/json matches it to a field, ignoring case,
// and every member that matches is read, so no spelling of a member reaches the handler past the
// rule that holds it.
func (rule authorityRule) holdsFor(decoded *v2Request) bool {
	if rule.Where.In == partQuery {
		values, present := decoded.values[rule.Where.Name]
		return present && (rule.Value == "" || slices.Contains(values, rule.Value))
	}
	for name, raw := range decoded.members {
		if !strings.EqualFold(name, rule.Where.Name) {
			continue
		}
		if rule.Value == "" || memberValue(raw) == rule.Value {
			return true
		}
	}
	return false
}

// memberValue is a body member's value as a rule names it: a string's text, and any other value's
// JSON.
func memberValue(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return strings.TrimSpace(string(raw))
}

// String names what the rule holds a request to, for its refusal: `state answered`, `the member
// asked_of`, `waiting_on=me`.
func (rule authorityRule) String() string {
	switch {
	case rule.Where.In == partQuery && rule.Value != "":
		return rule.Where.Name + "=" + rule.Value
	case rule.Where.In == partQuery:
		return "the query key " + rule.Where.Name
	case rule.Value != "":
		return rule.Where.Name + " " + rule.Value
	}
	return "the member " + rule.Where.Name
}

// authorityFaults names every rule of the decoder's route that could never hold, or that names
// what the route does not read, so a misspelt rule fails the server's start rather than leaving a
// value unguarded: a rule with no scope; a query rule on a key the route's query does not declare,
// or on a value its enum does not list; a body rule on a route with no JSON body, on a member no
// arm and no type declares, or naming a state or kind no arm of the union is.
func (d *v2Decoder) authorityFaults() []string {
	var faults []string
	body := d.route.Body
	for _, rule := range d.route.Authorities {
		fault := ""
		switch {
		case rule.Where.Name == "" || len(rule.Scopes) == 0:
			fault = "names no member or key, or no scope"
		case rule.Where.In == partQuery:
			field, declared := d.query.field(rule.Where.Name)
			switch {
			case !declared:
				fault = "reads a query key the route's Query does not declare"
			case rule.Value != "" && field.enum != nil && !slices.Contains(field.enum, rule.Value):
				fault = "names a value the key's enum does not list"
			}
		case rule.Where.In != partBody:
			fault = fmt.Sprintf("reads %q, which is no part of a request a rule reads (body, query)", rule.Where.In)
		case body.Kind != bodyJSON && body.Kind != bodyStateUnion && body.Kind != bodyKindUnion:
			fault = "reads a body member of a route whose body is no JSON body the gate sees decoded"
		case !bodyDeclaresMember(body, rule.Where.Name):
			fault = "reads a body member no type of the route's Body declares"
		case rule.Value != "" && unionMember(body) == rule.Where.Name && body.Arms[rule.Value] == nil:
			fault = "names a " + rule.Where.Name + " no arm of the union is"
		}
		if fault != "" {
			faults = append(faults, fmt.Sprintf("its authority rule %+v %s", rule, fault))
		}
	}
	return faults
}

// bodyDeclaresMember reports whether the body's type, or one of its arms, declares member.
func bodyDeclaresMember(body requestBody, member string) bool {
	return slices.ContainsFunc(bodyTypes(body), func(typ reflect.Type) bool {
		return declaresMember(membersDeclaredBy(typ), member)
	})
}

// unionMember is the member a union's arms are picked by, and "" for a body that is no union.
func unionMember(body requestBody) string {
	switch body.Kind {
	case bodyStateUnion:
		return "state"
	case bodyKindUnion:
		return "kind"
	}
	return ""
}
