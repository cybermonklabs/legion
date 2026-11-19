package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/fold"
)

// FuzzRuleHostRefusalIsNoLooserThanHostShapeOK holds ruleHostRefusal to airlock.HostShapeOK, the
// Lean-proved shape airlock.Validate holds every host to: a host ruleHostRefusal accepts (an empty
// reason) is one HostShapeOK accepts, so validateRulesFile, which runs ruleHostRefusal first, never
// passes a host for Validate to refuse in words that quote it. The seeds are the hosts the airlock
// package's model test drives (TestModelAgreesOnHostShapes, airlock_fuzz_test.go) and the shapes of
// this program's own host rows; go test runs each seed, and go test -fuzz searches beyond them.
func FuzzRuleHostRefusalIsNoLooserThanHostShapeOK(f *testing.F) {
	for _, host := range []string{
		"slack.com", "*.googleapis.com", "API.GitHub.com", "a-b.example.com", "localhost",
		"1e100.net", "*.0.example.com", "xn--p1ai", "93.184.216.34", "*.216.34", "*.34", "8080",
		"https://slack.com", "*", "slack.com/", "slack.com:443",
		".slack.com", "slack..com", "slack.com.", "*.", "", "-slack.com", "slack-.com",
		"slack_com", "*.*.com", "sl\u212aack.com",
		"2001:db8:7::1", "[2001:db8:7::1]", "2001:db8:7::1%eth0", "::ffff:192.0.2.10", "0x1f", "*.0x1f",
		"a.0x7f000001", "093.184.216.34", "api.github.com.", "a_b.example.com.", "api github.com.",
	} {
		f.Add(host)
	}
	f.Fuzz(func(t *testing.T, host string) {
		if why := ruleHostRefusal(fold.ASCII(host)); why == "" && !airlock.HostShapeOK(host) {
			t.Fatalf("ruleHostRefusal accepts %q, which airlock.HostShapeOK refuses", host)
		}
	})
}
