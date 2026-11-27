package core_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// CheckMachineID refuses the ids a hub grant entry or the watchdog's files cannot carry, the ids
// bus.CheckListenerMachineID refuses (one holding '%', which starts the escape of every character
// NATS refuses in a durable name, so its names could be another id's, and one whose durable is over
// NATS's 255-byte limit), and accepts the rest: an interior ordinary space, because both generated
// names replace it with a hyphen, and a dot, a wildcard or a path separator, which both names
// escape. A refused character is never echoed unless it is a printable delimiter.
func TestCheckMachineID(t *testing.T) {
	const carry = ", which a hub's grant entries or the watchdog's files cannot carry"
	const escape = " holds '%', which starts an escape in the listener's NATS names, so another machine id can give the same names"
	for _, row := range []struct {
		name, id, want string
	}{
		{name: "letters, digits, hyphens and underscores", id: "example-host_1"},
		{name: "an interior ordinary space", id: "example host 1"},
		{name: "a fully qualified host name", id: "host.example.internal"},
		{name: "a star, a full wildcard, a slash and a backslash", id: `example*host>a/b\c`},
		{name: "empty", want: "users[0].machineId is empty"},
		{name: "only ordinary spaces", id: "   ", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "leading ordinary space", id: " example-host", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "trailing ordinary space", id: "example-host ", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "leading non-breaking space", id: "\u00a0example-host", want: "users[0].machineId has leading or trailing whitespace"},
		{name: "the escape character", id: "host%2Eexample%2Einternal", want: "users[0].machineId" + escape},
		{name: "an interior tab", id: "example\thost", want: "users[0].machineId contains a control character" + carry},
		{name: "an interior newline", id: "example\nhost", want: "users[0].machineId contains a control character" + carry},
		{name: "an interior non-breaking space", id: "example\u00a0host", want: "users[0].machineId contains whitespace other than a space" + carry},
		{name: "the longest id the durable name allows", id: strings.Repeat("a", 246)},
		{name: "an id one byte too long", id: strings.Repeat("a", 247), want: "users[0].machineId makes the durable name listener-<id> 256 bytes long, over NATS's 255-byte limit for a consumer name"},
		// Each dot is three bytes of the durable, %2E, so these ids of 164 and 165 bytes are at the limit and one byte over it.
		{name: "the longest dotted id the durable name allows", id: strings.Repeat("a.", 41) + strings.Repeat("a", 82)},
		{name: "a dotted id whose escapes make the durable too long", id: strings.Repeat("a.", 41) + strings.Repeat("a", 83), want: "users[0].machineId makes the durable name listener-<id> 256 bytes long, over NATS's 255-byte limit for a consumer name"},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := core.CheckMachineID("users[0].machineId", row.id)
			if row.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, row.want)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "users[0].machineId", refused.Input)
		})
	}
}
