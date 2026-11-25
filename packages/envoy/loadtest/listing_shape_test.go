package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAListingNamesEachArtifactByAnIDInDispatchsShape: readListing takes a listing whose every
// entry names its artifact by an id in Dispatch's shape, as it came (in-shape); refuses one whose
// entry's id is out of that shape where it decodes it (dispatchID), in words that name none of the
// id (out-of-shape); and refuses one whose entry names no id, which dispatchID takes as naming
// nothing: an empty string (no-id-empty), a null (no-id-null) and an absent member (no-id-absent).
func TestAListingNamesEachArtifactByAnIDInDispatchsShape(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	key := shapeKey("1")
	t.Run("in-shape", func(t *testing.T) {
		listing, err := readListing(key, []byte(`[{"id":"`+id+`","kind":"doc"}]`))
		if err != nil || len(listing) != 1 || listing[0].ID != id {
			t.Errorf("readListing = %+v, %v; want the one artifact %s", listing, err, id)
		}
	})
	t.Run("out-of-shape", func(t *testing.T) {
		_, err := readListing(key, []byte(`[{"id":"`+id+`"},{"id":"../`+redacttest.Token+`"}]`))
		if err == nil || !strings.HasPrefix(err.Error(), key+" artifacts: the listing is not the shape replay-fetch reads") ||
			len(redacttest.Held(err.Error())) != 0 {
			t.Errorf("readListing = %v; want the listing refused where it decodes, naming none of the id", err)
		}
	})
	for name, entry := range map[string]string{"empty": `{"id":""}`, "null": `{"id":null}`, "absent": `{"kind":"doc"}`} {
		t.Run("no-id-"+name, func(t *testing.T) {
			_, err := readListing(key, []byte(`[{"id":"`+id+`"},`+entry+`]`))
			if err == nil || err.Error() != key+" artifacts: the listing names an artifact by no id" {
				t.Errorf("readListing(%s) = %v; want the listing refused for naming an artifact by no id", entry, err)
			}
		})
	}
}
