package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/routes"
)

// A deployment that signs people in with Google for a Workspace domain hands the router its sign-in
// issuer, the issuer of every account people records; one that signs them in through a Cognito
// pool's group hands it none, since that sign-in records no account, and a `sub` a Google sign-in
// left before the switch must name no account a pool-made credential acts as. So does a deployment
// that signs nobody in.
//
// Control: appContextOptions handing the router boot.SignInIssuer whatever the rule; the pool's
// case answers the pool's issuer.
func TestTheRouterNamesAccountsUnderTheSignInIssuerOnlyWhenTheSignInRecordsThem(t *testing.T) {
	for _, test := range []struct {
		name string
		boot bootConfig
		want string
	}{
		{"Google for a Workspace domain", bootConfig{SignInIssuer: googleIssuer, SignInDomain: "example.com"}, googleIssuer},
		{"a Cognito pool's group", bootConfig{SignInIssuer: "https://issuer.example/pool", SignInGroup: "dispatch"}, ""},
		{"no sign-in", bootConfig{}, ""},
	} {
		if got := appContextOptions(test.boot, nil, routes.AppContextOptions{}).SignInIssuer; got != test.want {
			t.Errorf("%s: the router is handed SignInIssuer %q, want %q", test.name, got, test.want)
		}
	}
}
