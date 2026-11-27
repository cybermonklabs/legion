package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The one read's answer is the bytes WriteJSON writes for the same response, whatever the number of
// entries and of the goroutines that encode them: its entries are encoded in chunks, several at a
// time, and written in their order, so a chunk boundary, a worker that finishes before the one
// ahead of it and a page of no whole chunk each leave the answer as a single pass writes it.
func TestTheOneReadsAnswerHasTheBytesWriteJSONWritesWhateverItsWorkers(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 25, 9, 30, 0, 123456789, time.UTC)
	entry := func(index int) meInboxEntry {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		row := &model.MyReplyRow{
			ThreadID: id, Kind: "comment", Ref: "/issues/CORE-1/comments/" + id,
			// A subject the encoder escapes, as it escapes every text of a page.
			Subject:     fmt.Sprintf("Entry %d <b>&</b> \u2028 \"quoted\"", index),
			SubjectTree: model.DisplayTree(fmt.Sprintf(`{"type":"doc","content":[{"type":"text","text":"%d"}]}`, index)),
			Reply:       model.MyReplyReply{ID: id, Kind: "comment", Author: model.Actor{Kind: "user", ID: "carol"}, CreatedAt: at},
			Unread:      index%2 == 0, UnreadCount: index % 5,
		}
		built := meInboxEntry{
			Node:      meInboxNode{Kind: "comment", ID: id},
			Relations: []meInboxRelation{{Kind: "participates_in", Direction: meInboxOutbound, At: at}},
			Moment:    at.Add(time.Duration(index) * time.Second),
		}
		if index%3 == 0 {
			built.Agent = &meInboxAgent{MyReplyRow: *row}
		} else {
			built.Thread = row
		}
		return built
	}
	for _, count := range []int{0, 1, meInboxEncodeChunk - 1, meInboxEncodeChunk, meInboxEncodeChunk + 1, 3*meInboxEncodeChunk + 5, 40 * meInboxEncodeChunk} {
		response := meInboxResponse{
			Person: "alice", AsOf: at, Horizon: 7, Total: count, Limit: 0, Offset: 0,
			Entries: make([]meInboxEntry, 0, count),
		}
		for index := range count {
			response.Entries = append(response.Entries, entry(index))
		}
		want := httptest.NewRecorder()
		WriteJSON(want, http.StatusOK, response)
		for _, workers := range []int{1, 2, 4, 16} {
			t.Run(fmt.Sprintf("%d entries, %d workers", count, workers), func(t *testing.T) {
				written := httptest.NewRecorder()
				writeMeInboxAnswerBy(written, response, workers)
				if !bytes.Equal(written.Body.Bytes(), want.Body.Bytes()) {
					t.Fatalf("the answer of %d entries by %d workers is %d bytes, not the %d WriteJSON writes",
						count, workers, written.Body.Len(), want.Body.Len())
				}
			})
		}
		answer := httptest.NewRecorder()
		writeMeInboxAnswer(answer, response)
		if !bytes.Equal(answer.Body.Bytes(), want.Body.Bytes()) {
			t.Fatalf("writeMeInboxAnswer of %d entries is %d bytes, not the %d WriteJSON writes", count, answer.Body.Len(), want.Body.Len())
		}
	}
}
