package sqlstate

import "testing"

// The six classes in which a server answers about its own state are not judgements, and every
// other class is one, whatever the code within it; a code with no class to read judges nothing.
func TestOnlyTheSixClassesOfTheServersOwnStateAreNotJudged(t *testing.T) {
	for code, notJudged := range map[string]bool{
		"08006": true,  // connection failure
		"40001": true,  // serialization failure
		"53300": true,  // too many connections
		"55P03": true,  // lock not available
		"57P01": true,  // admin shutdown
		"57P03": true,  // cannot connect now
		"58030": true,  // io error
		"":      true,  // no code
		"5":     true,  // no class
		"28P01": false, // invalid password
		"3D000": false, // invalid catalog name
		"25006": false, // read-only sql transaction
		"42P01": false, // undefined table
		"23505": false, // unique violation
		"22012": false, // division by zero
	} {
		if got := NotJudged(code); got != notJudged {
			t.Errorf("NotJudged(%q) = %t, want %t", code, got, notJudged)
		}
	}
}
