// Package schemanodestest holds the rule the tests of more than one package read for the name the
// store holds an attribute's values under (StoreName), which no code of schemanodes runs: the seed
// of the type nodes (store/migrations/20261109051232_graph_supertags.up.sql) writes each attribute
// node's store name itself. It is an ordinary package rather than a _test.go file because Go
// cannot share test-only code across packages, and it imports nothing of schemanodes, so
// schemanodes's own tests can import it.
package schemanodestest

import "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// StoreName is the name the store holds the values of attribute name of block type typeName
// under: the kind of each tuple that states one of its values (R1), the `store_name` its
// attribute node carries, and what follows the `/` in that node's id. A document, the editor and
// the schema schemanodes.Read builds know the attribute by name alone, so the store names it
// apart only where it must. It is name itself except where name is a base attribute every node
// carries (graphstore.IsBaseAttribute), whose value tuple would read as the node's own visibility
// or state: ask's `state` is stored as `answer_state`, and any other such attribute as its type's
// name, `_` and its own name.
//
// The mapping exists only while a markdown attribute carries a base attribute's name, as ask's
// `state` does. Once no type declares such an attribute, StoreName goes and every attribute's
// store name is its name. The seed of the type nodes
// (store/migrations/20261109051232_graph_supertags.up.sql) writes what this answers, and
// cmd/dispatch's TestEveryBlockTypeIsASupertagNodeAndValidationReadsIt holds the two together.
func StoreName(typeName, name string) string {
	switch {
	case !graphstore.IsBaseAttribute(name):
		return name
	case typeName == "ask" && name == "state":
		return "answer_state"
	default:
		return typeName + "_" + name
	}
}
