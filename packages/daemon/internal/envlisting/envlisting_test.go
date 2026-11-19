package envlisting

import (
	"strings"
	"testing"
)

// A variable is read whole wherever it sits in the listing, the first line included. A planted
// Unique value is found inside a longer value, and a machine path that holds the plain word the
// test once searched for is not taken for it.
func TestAListingIsReadByWholeVariables(t *testing.T) {
	seed := Unique("seed-")
	if !strings.HasPrefix(seed, "seed-") || len(seed) != len("seed-")+16 || seed == Unique("seed-") {
		t.Fatalf("Unique(seed-) = %q, want seed- and 16 hex digits, different on each call", seed)
	}
	vars := Vars("LEGION_GITHUB_READ_ONLY=\nPWD=/src/legion-inherited-seed-value\nLEGION_GITHUB_OWNER=acme\n" +
		"NATS_URL=nats://" + seed + "@hub\nPATH=/usr/bin:/bin\n")
	if value, ok := vars["LEGION_GITHUB_READ_ONLY"]; !ok || value != "" {
		t.Errorf("the first line's variable = %q, set %v; want set and empty", value, ok)
	}
	if vars["LEGION_GITHUB_OWNER"] != "acme" || vars["PATH"] != "/usr/bin:/bin" {
		t.Errorf("vars = %v", vars)
	}
	if name, ok := Carrying(vars, seed); !ok || name != "NATS_URL" {
		t.Errorf("Carrying(the seed) = %q, %v; want NATS_URL, which holds it inside a URL", name, ok)
	}
	if name, ok := Carrying(vars, Unique("inherited-")); ok {
		t.Errorf("%s carries a value nothing planted", name)
	}
}
