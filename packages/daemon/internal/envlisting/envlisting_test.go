package envlisting

import "testing"

// A variable is read whole wherever it sits in the listing, the first line included, and a word
// inside another variable's value, such as a checkout path in PWD, is not a value of its own.
func TestAListingIsReadByWholeVariables(t *testing.T) {
	vars := Vars("LEGION_GITHUB_READ_ONLY=\nPWD=/src/legion-inherited-seed-value\nLEGION_GITHUB_OWNER=acme\nPATH=/usr/bin:/bin\n")
	if value, ok := vars["LEGION_GITHUB_READ_ONLY"]; !ok || value != "" {
		t.Errorf("the first line's variable = %q, set %v; want set and empty", value, ok)
	}
	if vars["LEGION_GITHUB_OWNER"] != "acme" || vars["PATH"] != "/usr/bin:/bin" {
		t.Errorf("vars = %v", vars)
	}
	if name, ok := HoldingValue(vars, "inherited", "seed-value"); ok {
		t.Errorf("%s counts as holding a value it only contains", name)
	}
	if name, ok := HoldingValue(vars, "acme"); !ok || name != "LEGION_GITHUB_OWNER" {
		t.Errorf("HoldingValue(acme) = %q, %v; want LEGION_GITHUB_OWNER", name, ok)
	}
}
