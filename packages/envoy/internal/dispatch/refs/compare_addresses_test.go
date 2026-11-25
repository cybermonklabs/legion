package refs

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// addressParts are the pieces kinds and ids are made of: the empty string, letters, a kind and
// kinds it is a prefix of, ':' itself, a part that ends in ':', a byte below ':' ('0', '/') and
// bytes above it ('_', '~', 0xff), and an id's own prefix.
var addressParts = []string{"", "a", "b", ":", "0", "/", "~", "\xff", "block", "block_x", "blo", "lens", "lens_block", "z:", "READ-1/tree-md#"}

// requireAddressOrder fails unless CompareAddresses orders left and right as strings.Compare
// orders their addresses written out.
func requireAddressOrder(t *testing.T, left, right Target) {
	t.Helper()
	leftAddress, rightAddress := left.Kind+":"+left.ID, right.Kind+":"+right.ID
	if got, want := CompareAddresses(left, right), strings.Compare(leftAddress, rightAddress); got != want {
		t.Fatalf("CompareAddresses(%#v, %#v) answers %d, and strings.Compare of the written addresses %q and %q answers %d",
			left, right, got, leftAddress, rightAddress, want)
	}
}

// CompareAddresses orders two nodes as strings.Compare orders their addresses written out,
// `<kind>:<id>`, without writing either: over every pair of nodes whose kind and id are each one
// of addressParts, and over 200,000 pairs drawn from a fixed seed whose kind and id are each up to
// three of them, so one kind is a prefix of the other, a kind or an id holds ':', and the byte
// after a shorter kind is above ':' or below it.
func TestCompareAddressesOrdersAsTheWrittenAddresses(t *testing.T) {
	for _, leftKind := range addressParts {
		for _, leftID := range addressParts {
			for _, rightKind := range addressParts {
				for _, rightID := range addressParts {
					requireAddressOrder(t, Target{Kind: leftKind, ID: leftID}, Target{Kind: rightKind, ID: rightID})
				}
			}
		}
	}
	random := rand.New(rand.NewPCG(3, 4))
	part := func() string {
		var joined strings.Builder
		for range random.IntN(4) {
			joined.WriteString(addressParts[random.IntN(len(addressParts))])
		}
		return joined.String()
	}
	for range 200_000 {
		requireAddressOrder(t, Target{Kind: part(), ID: part()}, Target{Kind: part(), ID: part()})
	}
}
