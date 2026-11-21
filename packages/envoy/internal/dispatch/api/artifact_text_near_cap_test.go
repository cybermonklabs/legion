package api

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// nearCapDocs is a document service whose decode-cap answer for the documents in near is the one
// it holds there, and the real service's for every other.
type nearCapDocs struct {
	docs.API
	mu   sync.Mutex
	near map[string]string
}

func (d *nearCapDocs) NearDecodeCap(ctx context.Context, artifactID string) (string, bool, error) {
	d.mu.Lock()
	why, held := d.near[artifactID]
	d.mu.Unlock()
	if held {
		return why, true, nil
	}
	return d.API.NearDecodeCap(ctx, artifactID)
}

// The page's read of a document, GET /text with `Dispatch-Trees: 1`, says whether the rebuild
// route takes the document because it stands near the decode cap, and why, as the document
// service answers it (docs.Service.NearDecodeCap, whose own test drives a document at that state
// with no write refused): false for an ordinary document, true with the reason for one the service
// answers near. A read without trees, the command line's and the agents', carries neither key.
func TestThePagesReadOfADocumentSaysWhetherTheRebuildTakesIt(t *testing.T) {
	fake := &nearCapDocs{near: make(map[string]string)}
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		fake.API = service
		return fake
	})
	issue := createInteractionIssue(t, handler, "TEST", "Near the decode cap", "An ordinary spec.\n")
	target := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text"
	type nearness struct {
		Near   *bool   `json:"near_decode_cap"`
		Reason *string `json:"near_decode_cap_reason"`
	}
	read := func(trees bool) nearness {
		var answer nearness
		if err := json.Unmarshal(treesRequest(t, handler, target, trees).Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	if answer := read(true); answer.Near == nil || *answer.Near || answer.Reason != nil {
		t.Fatalf("the page's read of an ordinary document answered %+v, want near_decode_cap false and no reason", answer)
	}
	const why = "the document's live state holds 1047000 structs, and a write of its whole text, 24000 more, would carry it past the 1048576 ygo decodes in one update"
	fake.mu.Lock()
	fake.near[issue.PrimaryArtifactID] = why
	fake.mu.Unlock()
	if answer := read(true); answer.Near == nil || !*answer.Near || answer.Reason == nil || *answer.Reason != why {
		t.Fatalf("the page's read of a document near the decode cap answered %+v, want near_decode_cap true with the service's reason", answer)
	}
	if answer := read(false); answer.Near != nil || answer.Reason != nil {
		t.Fatalf("a read without trees answered %+v, want neither key", answer)
	}
}
