package core

import (
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/bus"
)

const machineIDCarryRefusal = ", which a hub's grant entries or the watchdog's files cannot carry"

// CheckMachineID refuses a machine id that cannot safely name a listener's NATS identities or
// ride in a hub grant entry or the watchdog's files. bus.ListenerDurable and bus.ListenerQueue
// produce listener-<id> and envoy-listener-<id>: each ordinary space is a hyphen, and each other
// character NATS refuses in a consumer name (a dot, '*', '>', '/', '\', a tab, a line break, a form
// feed) is '%' and two hex digits, so an id with a dot, such as a fully qualified host name, is
// accepted, and the grant names the durable and the queue group the listener creates. The listener
// refuses at start what bus.CheckListenerMachineID refuses, and so does this rule: an id holding
// '%', which could give the names of another id, and an id whose durable is over NATS's 255-byte
// limit for a consumer name. The rule is stricter than the listener where a hub grant entry
// (core/nats_auth.go) or the watchdog's heredoc and environment file could not carry the
// character: a control character or whitespace other than an ordinary space. The listener's own
// names carry each such character: they escape a tab, a line break and a form feed, and NATS takes
// the others in a consumer name. The listener trims ENVOY_MACHINE_ID while loading it, so leading
// or trailing whitespace is refused rather than allowed to change either generated name. Each
// refusal is an InputError naming input. Only a printable delimiter is quoted; control and
// whitespace characters are described without being echoed.
func CheckMachineID(input, id string) error {
	if id == "" {
		return InputErrorf(input, "%s is empty", input)
	}
	if strings.TrimSpace(id) != id {
		return InputErrorf(input, "%s has leading or trailing whitespace", input)
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return InputErrorf(input, "%s contains a control character%s", input, machineIDCarryRefusal)
		}
		if r != ' ' && unicode.IsSpace(r) {
			return InputErrorf(input, "%s contains whitespace other than a space%s", input, machineIDCarryRefusal)
		}
	}
	if err := bus.CheckListenerMachineID(id); err != nil {
		return InputErrorf(input, "%s %w", input, err)
	}
	return nil
}
