package graphstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalidAttributeValue is a write that states a node's state or its visibility at a value that
// attribute does not take (NodeStates; VisibilityProject and VisibilityAuthor). It is a sentinel
// so a route can answer it as a refusal of the caller's rather than as a failure of the server's;
// what a write is refused with is an InvalidAttributeValueError, which wraps it and names the
// attribute and the value.
var ErrInvalidAttributeValue = errors.New("a node's state or visibility is none of the values it takes")

// InvalidAttributeValueError is ErrInvalidAttributeValue for one value: the base attribute a write
// stated and the value it stated. Its words name both and every value the attribute takes, so a
// misspelled value reads as one.
type InvalidAttributeValueError struct {
	// Attribute is AttributeState or AttributeVisibility.
	Attribute string
	// Value is the value the write stated, which the attribute does not take.
	Value string
}

func (e *InvalidAttributeValueError) Error() string {
	return fmt.Sprintf("the node %s %q is none of %s", e.Attribute, e.Value, strings.Join(attributeValues(e.Attribute), ", "))
}

func (e *InvalidAttributeValueError) Unwrap() error { return ErrInvalidAttributeValue }

// attributeValues is every value a base attribute takes, or nil for one that takes any value: a
// node's project is any project's key, and its parties any parties.
func attributeValues(attribute string) []string {
	switch attribute {
	case AttributeState:
		return nodeStates[:]
	case AttributeVisibility:
		return visibilities[:]
	}
	return nil
}

// CheckAttributeValue refuses a value the base attribute does not take with an
// InvalidAttributeValueError, and answers nil for a value it takes and for any value of an
// attribute that takes any (attributeValues). It is the one rule every write of this store holds
// a node's state and visibility to, and a route that takes either from its caller asks it first.
func CheckAttributeValue(attribute, value string) error {
	values := attributeValues(attribute)
	if values == nil || slices.Contains(values, value) {
		return nil
	}
	return &InvalidAttributeValueError{Attribute: attribute, Value: value}
}

// checkBaseValues holds a write to CheckAttributeValue as it is queued, so a refused write
// queues its refusal alone (queueRefusal) and stores nothing: the state and the visibility of
// every node it writes, and every value tuple and every claim it states itself, of which a state
// or a visibility is all that can be refused. A node reserved for its author whose kind takes no
// such node is refused here too, with a VisibilityNotAdmittedError naming the kind
// (CheckVisibilityAdmitted); a restating node among them, since the visibility it carries is the
// one its insert would make. The first value refused is the answer.
func checkBaseValues(nodes []Node, tuples []Tuple, claims []Claim) error {
	for index := range nodes {
		node := &nodes[index]
		if err := CheckAttributeValue(AttributeState, node.State); err != nil {
			return err
		}
		if err := CheckAttributeValue(AttributeVisibility, node.Visibility); err != nil {
			return err
		}
		if err := CheckVisibilityAdmitted(node.Kind, node.ID, node.Visibility); err != nil {
			return err
		}
	}
	for index := range tuples {
		if tuple := &tuples[index]; tuple.ToKind == KindValue {
			if err := CheckAttributeValue(tuple.Kind, tuple.ToID); err != nil {
				return err
			}
		}
	}
	for index := range claims {
		if claim := &claims[index]; claim.ToKind == KindValue {
			if err := CheckAttributeValue(claim.Kind, claim.ToID); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkHeldValues is checkBaseValues of the tuples a write held for its cause holds, each read
// where it stands rather than copied: a version's block index holds a tuple a block.
func checkHeldValues(held []UncausedTuple) error {
	for index := range held {
		if tuple := &held[index].Tuple; tuple.ToKind == KindValue {
			if err := CheckAttributeValue(tuple.Kind, tuple.ToID); err != nil {
				return err
			}
		}
	}
	return nil
}
