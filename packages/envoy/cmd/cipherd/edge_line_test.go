package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The edge's start line names BROKER_EDGE_URL through redact.URL and the page
// BROKER_EDGE_MACHINE_LOGIN_URL names by that setting alone: a made-up user name and password, and a
// made-up token in the query, print in neither. BROKER_EDGE_URL with no credential is named as
// written.
func TestTheEdgeStartLineNamesNoCredentialOfItsURLs(t *testing.T) {
	const user, secret = "madeUpEdgeUser0123", "madeUpEdgeSecret0123"
	userinfo := user + ":" + secret + "@"
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	logEdgeListening("127.0.0.1:13390", &config.Config{
		EdgeURL:             "https://" + userinfo + "edge.example",
		EdgeMachineLoginURL: "https://edge.example/login?token=" + secret,
	})
	if !strings.Contains(logged.String(), "broker edge listening") {
		t.Fatalf("the log %q holds no edge line", logged.String())
	}
	for _, part := range []string{user[:10], secret[:10]} {
		if strings.Contains(logged.String(), part) {
			t.Errorf("the edge line %q holds %q of a credential", logged.String(), part)
		}
	}
	logged.Reset()
	logEdgeListening("127.0.0.1:13390", &config.Config{EdgeURL: "https://edge.example", EdgeMachineLoginURL: "https://edge.example/login"})
	if !strings.Contains(logged.String(), "url=https://edge.example ") || !strings.Contains(logged.String(), `verification_uri="(the page BROKER_EDGE_MACHINE_LOGIN_URL names; not printed)"`) {
		t.Errorf("the edge line %q does not name a plain BROKER_EDGE_URL as written and the page by its setting", logged.String())
	}
}
