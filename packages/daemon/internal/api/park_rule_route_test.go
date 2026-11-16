package api

import (
	"strings"
	"testing"
)

// The refusal an architect gets when it calls close_root on a child names park_child as how a
// child leaves. Under dispatch://INFRA-2971 a text that names that move carries its condition: a
// child is parked only when a person asked for that move, or when its work is done, folded into
// another issue or no longer needed.
func TestTheCloseRootRefusalOfAChildOffersNoParkWithoutTheRule(t *testing.T) {
	const parkRule = "only when a person asked for that move, or when the work is done, folded into another issue or no longer needed"
	text := closeRootRoute.forChild
	if strings.Contains(text, "park_child") && !strings.Contains(text, parkRule) {
		t.Fatalf("the ROOT_REQUIRED refusal offers park_child with no condition; a park it names must carry %q: %s", parkRule, text)
	}
}
