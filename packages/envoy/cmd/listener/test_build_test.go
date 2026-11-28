package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

const promptListenerBuildFlags = "-X main.promptReconnectBuild=1 -X main.durableBindIntervalBuild=100ms"

// promptListenerBinary builds the listener with promptReconnectBuild=1 and
// durableBindIntervalBuild=100ms. Tests must not decide their verdict by either value.
func promptListenerBinary(t testing.TB) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{
		Name:  "envoy-listener",
		Flags: []string{"-ldflags", promptListenerBuildFlags},
	})
}

// jetStreamWaitListenerBinary builds the listener with promptReconnectBuild=1,
// durableBindIntervalBuild=100ms and jetStreamWaitBuild=650ms. Tests must not decide their
// verdict by these values.
func jetStreamWaitListenerBinary(t testing.TB) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{
		Name:  "envoy-listener",
		Flags: []string{"-ldflags", promptListenerBuildFlags + " -X main.jetStreamWaitBuild=650ms"},
	})
}

// shortBudgetListenerBinary is only for rows whose request must run out of route budget.
// Its reconnect is also prompt; no verdict depends on either shortened duration.
func shortBudgetListenerBinary(t testing.TB) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{
		Name:  "envoy-listener",
		Flags: []string{"-ldflags", promptListenerBuildFlags + " -X main.sendRequestBudgetBuild=350ms"},
	})
}
