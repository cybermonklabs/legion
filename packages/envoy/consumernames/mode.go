package consumernames

import (
	"fmt"
	"os"
)

// ModeEnvVar names the process setting that chooses how a process names its consumers. A process
// reads it once, at its start (ModeFromEnv), and changes mode only by a restart with a new value:
// a NATS reconnect never changes it, and nothing a store holds selects it.
const ModeEnvVar = "ENVOY_CONSUMER_NAMES"

// Mode is how a process names the consumers it creates.
type Mode string

const (
	// ModeFixed runs the family's code before this package, unchanged: today's fixed names, its
	// order of delete and create, its resume handling, its reads and its grants, with no
	// selection store read or required. It is the default.
	ModeFixed Mode = "fixed"
	// ModeFresh runs this package's protocol (Names): a fresh physical name at each creation,
	// behind a selection record, refusing every consumer call until the store's sentinel says the
	// cutover is done (Admit).
	ModeFresh Mode = "fresh"
)

// ParseMode is the mode value names: ModeFixed for "" (the setting unset) and for "fixed",
// ModeFresh for "fresh", and an error naming ModeEnvVar and the value for anything else, on which
// a process refuses to start.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", ModeFixed:
		return ModeFixed, nil
	case ModeFresh:
		return ModeFresh, nil
	}
	return "", fmt.Errorf("%s=%q is not a consumer naming mode: set %q or %q (unset means %q)", ModeEnvVar, value, ModeFixed, ModeFresh, ModeFixed)
}

// ModeFromEnv is ParseMode of the process's ModeEnvVar.
func ModeFromEnv() (Mode, error) {
	return ParseMode(os.Getenv(ModeEnvVar))
}
