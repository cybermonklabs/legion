// Package flagerr reads the flag package's parse errors for the envoy-infra commands that never
// echo a value they could not parse, since a seed may have been pasted there (envoy-infra render,
// envoy-infra buckets).
package flagerr

import (
	"flag"
	"strings"
)

// Malformed returns the name of the flag whose value the flag package could not parse, read from
// its message, "invalid value %q for flag -%s: %v" or, for a boolean flag, "invalid boolean value
// %q for -%s: %v"; "" for any other error. The quoted value comes first, so the name follows the
// message's last marker, and only a flag the set defines is returned.
func Malformed(fs *flag.FlagSet, err error) string {
	msg := err.Error()
	if !strings.HasPrefix(msg, "invalid value ") && !strings.HasPrefix(msg, "invalid boolean value ") {
		return ""
	}
	for _, marker := range []string{" for flag -", " for -"} {
		if i := strings.LastIndex(msg, marker); i >= 0 {
			if name, _, _ := strings.Cut(msg[i+len(marker):], ":"); fs.Lookup(name) != nil {
				return name
			}
		}
	}
	return ""
}
