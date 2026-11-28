package settingsfields_test

import (
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
)

// TestOfReadsJSONNames: a struct's string fields become their JSON names, and an empty one is
// left out, as omitempty leaves it out of the body.
func TestOfReadsJSONNames(t *testing.T) {
	got := settingsfields.Of(struct {
		A string `json:"a,omitempty"`
		B string `json:"b"`
	}{A: "x"})
	if want := map[string]string{"a": "x"}; !maps.Equal(got, want) {
		t.Fatalf("Of = %v, want %v", got, want)
	}
}

// TestOfPanicsOnANonStringField: a settings field is a string, so a struct with any other field is
// a programming error, refused at the first call rather than written as something else.
func TestOfPanicsOnANonStringField(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Of of a struct with an int field did not panic")
		}
	}()
	settingsfields.Of(struct {
		N int `json:"n"`
	}{N: 1})
}
