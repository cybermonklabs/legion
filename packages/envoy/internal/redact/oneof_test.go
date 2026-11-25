package redact

import "testing"

// TestOneOfAnswersTheCallersWordForTheFieldAsItCame: a field that is one of the words answers that
// word, compared as it came, even where the rule's reading of the field is cut because it shares
// a run with a credential the request carried (carried); one that differs in case, holds a word
// inside it, or is empty answers "" and false; an empty field is the word "" where words holds it.
func TestOneOfAnswersTheCallersWordForTheFieldAsItCame(t *testing.T) {
	words := []string{"receipt_timeout", "delivery_failed"}
	for _, row := range []struct {
		name  string
		field RemoteText
		words []string
		want  string
		one   bool
	}{
		{"first", NewRemoteText("receipt_timeout"), words, "receipt_timeout", true},
		{"second", NewRemoteText("delivery_failed"), words, "delivery_failed", true},
		{"carried", NewRemoteText("delivery_failed").WithoutTokens("delivery_failed"), words, "delivery_failed", true},
		{"case", NewRemoteText("Delivery_Failed"), words, "", false},
		{"inside", NewRemoteText("not delivery_failed"), words, "", false},
		{"empty", NewRemoteText(""), words, "", false},
		{"zero", RemoteText{}, words, "", false},
		{"no-words", NewRemoteText("receipt_timeout"), nil, "", false},
		{"empty-word", RemoteText{}, []string{"receipt_timeout", ""}, "", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, one := row.field.OneOf(row.words...)
			if got != row.want || one != row.one {
				t.Errorf("OneOf(%q) = %q, %t; want %q, %t", row.words, got, one, row.want, row.one)
			}
		})
	}
	if carried := NewRemoteText("delivery_failed").WithoutTokens("delivery_failed"); carried.String() == "delivery_failed" {
		t.Fatalf("the carried row's field prints %q as it came, so it shows nothing of a cut reading", carried)
	}
}
