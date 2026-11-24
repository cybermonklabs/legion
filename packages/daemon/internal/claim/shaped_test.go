package claim

import "testing"

// TestATokenIsShapedOnlyAsTheDaemonMakesOne: Shaped holds for each token NewToken and
// ControllerToken make, and for no other text: one with no issue, an issue project in capitals,
// a project or an issue part outside a token's letters, or a role no claim is on.
func TestATokenIsShapedOnlyAsTheDaemonMakesOne(t *testing.T) {
	for _, role := range Roles {
		token, err := NewToken("legion", "LEGION-208", role)
		if err != nil {
			t.Fatal(err)
		}
		if !token.Shaped() {
			t.Errorf("NewToken's %q is not shaped", token)
		}
	}
	if token := ControllerToken("legion"); !token.Shaped() {
		t.Errorf("ControllerToken's %q is not shaped", token)
	}
	for _, token := range []Token{
		"",
		"legion-legion-architect",
		"legion-legion-LEGION-208-architect",
		"legion-legion-legion-208-reader",
		"legion--legion-208-architect",
		"legion-legion-legion-architect",
		"legion-legion-legion-208x-architect",
		"legion-legion-ghs_0123456789abcdef-208-architect",
		"legion-legion-legion-208-architect\n",
		"other-legion-legion-208-architect",
		"legion-Legion-controller",
		"legion-legion-legion-208-controller",
	} {
		if token.Shaped() {
			t.Errorf("%q is shaped; no token the daemon makes has its shape", token)
		}
	}
}
