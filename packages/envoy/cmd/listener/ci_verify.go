package main

import "fmt"

// ciVerifyEnv turns on the CI store's verify-before-announce (cistore.WithVerify): the summary loop
// publishes a head's settlement only after Dispatch's CI reconcile has read the head from GitHub
// after the listener asked. It is a deployment's to set, on every listener of it at once, where a
// Dispatch with the GitHub App key serves the bucket: with it on and no reconcile answering, no
// settlement is published at all. Unset is off, which is how a development stack,
// scripts/e2e-local.sh and the start-up tests run.
const ciVerifyEnv = "ENVOY_CI_VERIFY"

// ciVerifySetting reads ciVerifyEnv's value: "on" or "off", and off when unset. Anything else
// refuses the start, naming the variable: a misspelt value would otherwise turn the check off
// without a word.
func ciVerifySetting(value string) (bool, error) {
	switch value {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, fmt.Errorf("%s=%q: the setting is on or off", ciVerifyEnv, value)
}
