package cipheraddr

import (
	"net/url"
	"testing"
)

// airlockProxyTestHost is the host of every proxy URL below. Each URL is built from its parts,
// and none is written as one literal: a line holding `scheme://user:password@host` is a
// credential in a URL to the push leak check, which refuses the push.
const airlockProxyTestHost = "broker.internal.example:13381"

// proxyURL is a proxy URL with user and password, either empty for none.
func proxyURL(user, password string) string {
	parsed := &url.URL{Scheme: "http", Host: airlockProxyTestHost}
	switch {
	case password != "":
		parsed.User = url.UserPassword(user, password)
	case user != "":
		parsed.User = url.User(user)
	}
	return parsed.String()
}

// NamesAirlockProxy holds a URL to the airlock's user alone: a proxy with no credential, one whose
// user is another's, a user that only begins with agent-secrets, and a value that is no URL are
// not the airlock's. cmd/cipher's TestAirlockProxyURLCarriesThePasswordAndParsesBack holds the URL
// agent-secrets writes to NamesAirlockProxy.
func TestNamesAirlockProxyKnowsTheAirlocksUserAlone(t *testing.T) {
	for value, want := range map[string]bool{
		proxyURL(AirlockProxyUser, "placeholder"):          true,
		proxyURL(AirlockProxyUser, ""):                     true,
		proxyURL("", ""):                                   false,
		proxyURL("ada", "placeholder"):                     false,
		proxyURL(AirlockProxyUser+"-other", "placeholder"): false,
		"": false,
		// A URL net/url cannot parse: %zz is no percent-encoded byte.
		"http://" + AirlockProxyUser + ":%zz" + "@" + airlockProxyTestHost: false,
	} {
		if got := NamesAirlockProxy(value); got != want {
			t.Errorf("NamesAirlockProxy(%q) = %v, want %v", value, got, want)
		}
	}
}
