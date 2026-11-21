package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestTheBootLogSaysWhetherTheHoldIsOff: the real binary started with no
// BROKER_PROJECT_ACCESS_URL builds no hold gate and holds nobody, and its boot log says so and
// why; one started with the setting logs no such line.
func TestTheBootLogSaysWhetherTheHoldIsOff(t *testing.T) {
	off := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
	}})
	off.AwaitRecord(t, "hold of refused people", map[string]any{"state": "off", "reason": "BROKER_PROJECT_ACCESS_URL is not set"})

	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")
	on := startBroker(t, brokerOptions{Env: projectAccessEnv(t, storetest.URL(t), cluster, github, clusterKey, projectAccessStandIn(t))})
	on.AwaitRecord(t, "project access check", map[string]any{"state": "on"})
	// The token exchange's line comes after the hold's in the boot, so a log that holds it holds
	// every line the hold could have written.
	on.AwaitRecord(t, "token exchange", nil)
	if lines := recordsWith(on.Log(), "hold of refused people", nil); len(lines) != 0 {
		t.Fatalf("the broker started with BROKER_PROJECT_ACCESS_URL logged %v, want no line saying the hold is off", lines)
	}
}
