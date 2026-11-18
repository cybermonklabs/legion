package schemanodes

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestNoAttributeNodeStoresItsValuesUnderABaseAttribute holds every live attribute node of a
// migrated store to a store name that is no base attribute. An attribute's value is a tuple
// whose kind is that store name (R1), and a value tuple of kind `state` or `visibility` out of a
// node is that node's own state or visibility: an ask block whose answer state was stored under
// `state` would read as a block that is `answered` rather than live. A node that carries no
// store name fails too, since every writer of an attribute value reads its tuple kind there.
// The base attributes are graphstore's own set, so one added there is held here with no change
// to this test.
func TestNoAttributeNodeStoresItsValuesUnderABaseAttribute(t *testing.T) {
	database := storetest.Open(t)
	nodes, err := graphstore.NodesOfKind(context.Background(), database.Pool, KindAttribute)
	if err != nil {
		t.Fatalf("read the attribute nodes: %v", err)
	}
	checked := 0
	for _, node := range nodes {
		if node.State != graphstore.StateLive {
			continue
		}
		checked++
		stored, _ := node.Attributes["store_name"].(string)
		switch {
		case stored == "":
			t.Errorf("attribute node %s carries no store name, want the name its values are stored under (StoreName)", node.ID)
		case graphstore.IsBaseAttribute(stored):
			t.Errorf("attribute node %s stores its values under %q, which is a base attribute of every node (graphstore.BaseAttributes %v)",
				node.ID, stored, graphstore.BaseAttributes())
		}
	}
	if checked == 0 {
		t.Fatal("the migrated store holds no live attribute node, so this test would prove nothing")
	}
	t.Logf("checked the store names of %d live attribute nodes against the base attributes %v", checked, graphstore.BaseAttributes())
}

// TestEveryStoreNameStaysOffTheBaseAttributes holds StoreName to its rule in the general case:
// an attribute named for any base attribute, of any type, is stored under a name that is no base
// attribute, ask's `state` is stored as `answer_state`, and every other name is stored as itself.
func TestEveryStoreNameStaysOffTheBaseAttributes(t *testing.T) {
	for _, base := range graphstore.BaseAttributes() {
		for _, typeName := range []string{"ask", "callout", "claim"} {
			if stored := StoreName(typeName, base); graphstore.IsBaseAttribute(stored) {
				t.Errorf("StoreName(%q, %q) = %q, which is a base attribute", typeName, base, stored)
			}
		}
	}
	if stored := StoreName("ask", "state"); stored != "answer_state" {
		t.Errorf("StoreName(\"ask\", \"state\") = %q, want answer_state", stored)
	}
	for _, name := range []string{"urgency", "answered_by", "kind", "serves", "ci_check"} {
		if stored := StoreName("ask", name); stored != name {
			t.Errorf("StoreName(\"ask\", %q) = %q, want the name itself", name, stored)
		}
	}
}
