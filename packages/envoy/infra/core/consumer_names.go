package core

import "github.com/sjawhar/envoy/internal/bus"

// EnvConsumerNames is the setting that tells a consumer creator, envoy-listener or
// envoy-dispatch, how it names the JetStream consumers it creates: "fixed", one name for each
// consumer for ever, or "fresh", a new physical name at each creation behind a selection record. A
// creator reads it once, at its start, so its mode changes only by a restart with a new value.
// Unset, it runs fixed. A component renders it only where its consumer-name input is set, so a
// deployment that sets none renders what it always rendered.
const EnvConsumerNames = "ENVOY_CONSUMER_NAMES"

// The two consumer-name modes a creator starts on.
const (
	ConsumerNamesFixed = "fixed"
	ConsumerNamesFresh = "fresh"
)

// CheckConsumerNames refuses a consumer-name setting a creator would refuse at its start, so a
// component that renders one is refused before it registers anything rather than deploying a
// creator that will not start. input is the caller's own path for the setting, as CheckAllowedHosts
// takes it: "consumerNames" for a listener's config, and a component's spelling, such as
// "listener.consumerNames", for its own input. "" (unset), "fixed" and "fresh" are taken, exactly
// as written: case and surrounding whitespace count, and a list is no mode. A value holding an
// nkey seed is refused first, naming input and never the value, since the refusal below quotes
// it.
//
// The rule is the creators' own, ParseMode in packages/envoy/consumernames, copied by hand: core
// may not import that package, since a non-test import would put it in `go list -deps ./cmd/...`,
// the path list a plugin release is cut from (packages/envoy/infra/README.md, "Changing the
// module"), so an edit to it would cut a plugin release. A creator built before that package reads
// no such setting and runs fixed whatever it holds.
func CheckConsumerNames(input, value string) error {
	if bus.HoldsSeed(value) {
		return InputErrorf(input, "%w", SeedRefusal(input))
	}
	switch value {
	case "", ConsumerNamesFixed, ConsumerNamesFresh:
		return nil
	}
	return InputErrorf(input, "%s %q must be %q or %q (%s); leave it unset for %s",
		input, value, ConsumerNamesFixed, ConsumerNamesFresh, EnvConsumerNames, ConsumerNamesFixed)
}
