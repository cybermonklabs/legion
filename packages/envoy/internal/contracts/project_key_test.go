package contracts

import "testing"

// TestValidProjectKeyIsTheWholeKeyPattern: ValidProjectKey answers ProjectKeyPattern over the whole
// string - an uppercase letter, then 1 to 9 uppercase letters or digits - so a key inside a longer
// string, an issue key, a lowercase key and the old on/off value 1 are none.
func TestValidProjectKeyIsTheWholeKeyPattern(t *testing.T) {
	for key, want := range map[string]bool{
		"CORE": true, "C1": true, "ABCDEFGHIJ": true,
		"": false, "C": false, "1": false, "core": false, "CORE-1": false, " CORE": false, "ABCDEFGHIJK": false, "1CORE": false,
	} {
		if got := ValidProjectKey(key); got != want {
			t.Errorf("ValidProjectKey(%q) = %t, want %t", key, got, want)
		}
	}
}
