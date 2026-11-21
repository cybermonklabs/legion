package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"

	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
)

// A listener starts and serves while an identity token issuer cannot be read, and answers each
// token of that issuer 503 until it has read the issuer's discovery document (oidc.LazyVerifier),
// so a roll completes while an issuer is unreadable: a wrong ENVOY_OIDC_ISSUER, an issuer that is
// down, or a pod with no egress to it. Nothing in the pod's readiness shows that state, since a
// listener in it serves everything else. The two reports below are what show it: /healthz's "oidc"
// and "google" fields, and the envoy_listener_oidc_issuer_discovered gauge, which an alert reads.
// Neither decides /healthz's status, for the reason Dispatch's service_tokens field does not: a
// probe that failed on it would take every other service away for an outage the listener serves
// through.

// The issuer label of envoy_listener_oidc_issuer_discovered: cluster is the ENVOY_OIDC_ISSUER
// pair's issuer, google the Google pair's.
const (
	issuerCluster = "cluster"
	issuerGoogle  = "google"
)

// registerIssuerGauge registers envoy_listener_oidc_issuer_discovered: for each identity token
// issuer the listener is configured for, 1 once it has read the issuer's discovery document
// (oidc.LazyVerifier.Discovered) and 0 until then, read at each scrape. A listener configured for
// neither writes no sample.
func registerIssuerGauge(met *metrics.Registry, auth listenerAPIAuth) {
	met.NewGaugeSetFunc("envoy_listener_oidc_issuer_discovered",
		"1 once the listener has read the identity token issuer's discovery document, 0 while it has not and answers that issuer's tokens 503, by issuer (cluster, google)",
		func() []metrics.GaugeSample {
			var samples []metrics.GaugeSample
			add := func(issuer string, verifier *oidc.LazyVerifier) {
				var value int64
				if verifier.Discovered() {
					value = 1
				}
				samples = append(samples, metrics.GaugeSample{Labels: [][2]string{{"issuer", issuer}}, Value: value})
			}
			if auth.verifier != nil {
				add(issuerCluster, auth.verifier)
			}
			if auth.google != nil {
				add(issuerGoogle, auth.google.verifier)
			}
			return samples
		})
}

// issuerHealthFields is the "oidc" and "google" fields of a /healthz answer: whether the listener
// has read the discovery document of the ENVOY_OIDC_ISSUER pair's issuer and of Google's, each
// null when that pair is not configured.
func issuerHealthFields(auth listenerAPIAuth) map[string]any {
	fields := map[string]any{"oidc": nil, "google": nil}
	if auth.verifier != nil {
		fields["oidc"] = auth.verifier.Discovered()
	}
	if auth.google != nil {
		fields["google"] = auth.google.verifier.Discovered()
	}
	return fields
}

// withIssuerHealth is next, the listener's /healthz (healthzHandler), with issuerHealthFields added
// to every answer it gives: starting, healthy, degraded and unhealthy alike, with its status code
// and every other field unchanged. The issuers are not among the bus dependencies healthzHandler
// is built over, which open later; they are known from the start. An answer that is not a JSON
// object passes through as it is.
func withIssuerHealth(next http.Handler, auth listenerAPIAuth) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := &bufferedAnswer{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(answer, r)
		maps.Copy(w.Header(), answer.header)
		decoder := json.NewDecoder(bytes.NewReader(answer.body.Bytes()))
		decoder.UseNumber()
		var body map[string]any
		if err := decoder.Decode(&body); err != nil || body == nil {
			w.WriteHeader(answer.status)
			_, _ = w.Write(answer.body.Bytes())
			return
		}
		maps.Copy(body, issuerHealthFields(auth))
		w.WriteHeader(answer.status)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// bufferedAnswer holds an answer a handler writes, for withIssuerHealth to add to before it is
// sent.
type bufferedAnswer struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (a *bufferedAnswer) Header() http.Header { return a.header }

func (a *bufferedAnswer) WriteHeader(status int) {
	if !a.wrote {
		a.status, a.wrote = status, true
	}
}

func (a *bufferedAnswer) Write(p []byte) (int, error) {
	a.wrote = true
	return a.body.Write(p)
}
