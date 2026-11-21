package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
	"github.com/sjawhar/envoy/internal/oidc"
)

// privateReadiness is GET /private-readiness: the go-live report of the private-hub admission
// proof (busprojects.Report), every hub of DISPATCH_PRIVATE_HUBS and every listener replica of
// DISPATCH_PRIVATE_LISTENERS with the last proof's result, failed or passing, which an operator
// reads before any project is private. It is mounted in every deployment (dispatchHandler) and
// answers only Dispatch's own service account, account (requireOwnAccount). admission is the proof
// main runs, nil for a deployment that runs none, which answers the empty report.
type privateReadiness struct {
	admission hubAdmission
	account   string
}

// newPrivateReadiness is the route over the proof main runs and boot's DISPATCH_SERVICE_ACCOUNT.
func newPrivateReadiness(boot bootConfig, admission hubAdmission) privateReadiness {
	return privateReadiness{admission: admission, account: boot.ServiceAccount}
}

// report serves the go-live report: the proof's own (busprojects.HubAdmission.ReportHandler),
// whose status is 200 whatever it holds, so no probe reads a failed proof as a pod out of service;
// with no proof, the empty report, calling nothing on the nil value a deployment with no private
// hub holds.
func (p privateReadiness) report() http.Handler {
	if noHubAdmission(p.admission) {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			api.WriteJSON(w, http.StatusOK, busprojects.Report{Hubs: []busprojects.HubReport{}, Listeners: []busprojects.ReplicaReport{}})
		})
	}
	return p.admission.ReportHandler()
}

// requireOwnAccount serves next only to a request presenting a projected service-account token
// that verifier verifies and whose subject is account, Dispatch's own: the go-live report names
// every hub and listener replica of the deployment, which only Dispatch's operators read, from
// inside Dispatch's own pod with its own token. A request with no credential at all, no
// Authorization header and no cookie, is 401 NO_IDENTITY, the refusal every route gives a caller
// that carries none. A token of the service-account form that does not verify is 401
// OIDC_TOKEN_INVALID, and one its issuer cannot judge yet 503 OIDC_ISSUER_UNAVAILABLE, as the API
// answers them. Every other caller, a verified token of another account, a person's cookie, a
// personal or shared token and a broker token's DPoP presentation among them, and every caller
// while the deployment names no account of its own or verifies no service-account token, is 403
// DISPATCH_ACCOUNT_REQUIRED, naming the subject when one was verified.
func requireOwnAccount(verifier *oidc.LazyVerifier, account string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, present := auth.BearerToken(r)
		if !present && len(r.Cookies()) == 0 {
			api.WriteJSON(w, http.StatusUnauthorized, readinessRefusal{Code: "NO_IDENTITY",
				Error: "GET /private-readiness answers only Dispatch's own service account, presenting its projected token"})
			return
		}
		if verifier == nil || account == "" || token == "" || !oidc.LooksLikeJWT(token) {
			refuseNotOwnAccount(w, "")
			return
		}
		claims, err := verifier.Verify(r.Context(), token)
		if errors.Is(err, oidc.ErrIssuerUnavailable) {
			api.WriteJSON(w, http.StatusServiceUnavailable, readinessRefusal{Code: "OIDC_ISSUER_UNAVAILABLE",
				Error: "the service-account token issuer cannot be reached; try again in a minute"})
			return
		}
		if err != nil {
			// The class only: the error carries the token's own unverified claims, which an
			// unauthenticated caller chooses.
			reason := oidc.Reason(err)
			slog.Warn("dispatch: service-account token rejected at GET /private-readiness", "reason", reason)
			api.WriteJSON(w, http.StatusUnauthorized, readinessRefusal{Code: "OIDC_TOKEN_INVALID", Error: "service-account token rejected (" + reason + ")"})
			return
		}
		if claims.Subject != account {
			refuseNotOwnAccount(w, claims.Subject)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readinessRefusal is the JSON error GET /private-readiness answers, in the shape every Dispatch
// route answers one.
type readinessRefusal struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// refuseNotOwnAccount answers 403 DISPATCH_ACCOUNT_REQUIRED, naming the verified subject when
// there is one.
func refuseNotOwnAccount(w http.ResponseWriter, subject string) {
	message := "GET /private-readiness answers only Dispatch's own service account (DISPATCH_SERVICE_ACCOUNT), presenting its projected token"
	if subject != "" {
		message += "; the token presented is " + subject + "'s"
	}
	api.WriteJSON(w, http.StatusForbidden, readinessRefusal{Code: "DISPATCH_ACCOUNT_REQUIRED", Error: message})
}

// giveListeners hands the proof main runs the listener workloads that serve its hubs
// (DISPATCH_PRIVATE_LISTENERS) and the client that reads their replicas, before its loop starts,
// so its first proof reads them. A deployment with no proof, a nil admission or the nil pointer
// busprojects.NewHubAdmission answers for no hub, gets nothing: it has no listener to read, and a
// call on the nil proof would end the start.
func giveListeners(admission hubAdmission, listeners []busprojects.ListenerTarget, client *http.Client) {
	if noHubAdmission(admission) {
		return
	}
	admission.SetListeners(listeners, client)
}

// listenerTargets is the workloads of listeners, as the proof reads them.
func listenerTargets(listeners []privateListener) []busprojects.ListenerTarget {
	targets := make([]busprojects.ListenerTarget, len(listeners))
	for index, listener := range listeners {
		targets[index] = listener.ListenerTarget
	}
	return targets
}

// listenerHTTP is the client each proof reads the listener replicas' GET /v1/privacy-readiness
// with: each read presents Dispatch's own projected token for the audience of the listener its
// Host names, from that entry's token_file, read through brokertoken.AccountToken, which reads the
// file again once its copy is AccountTokenCacheTTL old, since the kubelet rewrites it in place.
// Each listener verifies a token for its own audience alone, so one token for every listener would
// be refused by all but one. A read whose Host names no entry, or whose token cannot be read, sends
// no request. It sets no timeout of its own: each proof bounds its reads by its own context.
func listenerHTTP(listeners []privateListener) *http.Client {
	tokens := make(map[string]*brokertoken.AccountToken, len(listeners))
	for _, listener := range listeners {
		tokens[listener.Host] = brokertoken.NewAccountToken(listener.TokenFile)
	}
	return &http.Client{Transport: listenerTokenTransport{base: http.DefaultTransport, tokens: tokens}}
}

// listenerTokenTransport sets each request's Authorization to the token of the listener its Host
// names.
type listenerTokenTransport struct {
	base   http.RoundTripper
	tokens map[string]*brokertoken.AccountToken
}

func (t listenerTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	account, ok := t.tokens[r.Host]
	if !ok {
		return nil, fmt.Errorf("no entry of DISPATCH_PRIVATE_LISTENERS names the host %q, so no token is presented and nothing is sent", r.Host)
	}
	token, err := account.Read()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(r)
}
