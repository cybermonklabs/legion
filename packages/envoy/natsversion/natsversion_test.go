package natsversion

import (
	"strings"
	"testing"
)

// Every release whose scan of a consumer's stored messages can skip a literal filter beside a
// wildcard one is refused, naming its version and the fixed releases; every release that reads
// every filter is taken; and a version that cannot be read is refused, since a server nobody can
// place is not known to be fixed. A prerelease comes before its release: the first candidates of
// v2.11.12 (RC.1 to RC.3) and of v2.12.4 (RC.1 and RC.2) still read a block through
// gsl.IntersectStree, so every candidate of a line's first fixed release is refused, while a
// candidate of a later patch, which is after a fixed release, is taken.
func TestRefusalNamesEveryServerThatCanSkipAFilter(t *testing.T) {
	t.Parallel()
	for version, fixed := range map[string]bool{
		"2.9.25":        false,
		"2.10.0":        false,
		"2.10.22":       false,
		"2.10.29":       false,
		"v2.10.29":      false,
		"2.11.0":        false,
		"2.11.11":       false,
		"2.11.12":       true,
		"2.11.17":       true,
		"2.12.0":        false,
		"2.12.3":        false,
		"2.12.4":        true,
		"2.12.15":       true,
		"2.14.0":        true,
		"2.15.1":        true,
		"v2.15.3":       true,
		"2.11.12-RC.1":  false,
		"2.11.12-RC.3":  false,
		"2.11.12-RC.4":  false,
		"2.11.13-RC.1":  true,
		"2.12.4-RC.1":   false,
		"2.12.4-RC.2":   false,
		"2.12.5-RC.1":   true,
		"2.14.0-RC.1":   false,
		"2.14.1-RC.1":   true,
		"2.15.0-RC.1":   true,
		"2.11.12+build": true,
		"2.12.3-RC.9":   false,
		"3.0.0":         true,
		"":              false,
		"2.15":          false,
		"two.fifteen.1": false,
	} {
		err := Refusal(version)
		if fixed {
			if err != nil {
				t.Errorf("Refusal(%q) = %v, want nil", version, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("Refusal(%q) = nil, want a refusal", version)
			continue
		}
		if !strings.Contains(err.Error(), FixedReleases) || !strings.Contains(err.Error(), version) {
			t.Errorf("Refusal(%q) = %q, want it to name the version and %q", version, err, FixedReleases)
		}
	}
}
