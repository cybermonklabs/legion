// carried_decode_test.go is decodeCarriedBody's bound, which a test over HTTP cannot see: the
// client writes the whole body whatever the route reads of it.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
)

// emptyIDs is the body {"session_ids":["","",…]} of n empty ids (n at least 1), written as it is
// read, counting the bytes the reader has given.
type emptyIDs struct {
	rest string // what is left of the piece being written
	left int    // the entries not yet begun
	read int
}

func newEmptyIDs(n int) *emptyIDs { return &emptyIDs{rest: `{"session_ids":[`, left: n} }

func (e *emptyIDs) Read(p []byte) (int, error) {
	k := 0
	for k < len(p) {
		if e.rest == "" {
			switch {
			case e.left > 1:
				e.rest = `"",`
			case e.left == 1:
				e.rest = `""]}`
			default:
				e.read += k
				if k == 0 {
					return 0, io.EOF
				}
				return k, nil
			}
			e.left--
		}
		c := copy(p[k:], e.rest)
		e.rest, k = e.rest[c:], k+c
	}
	e.read += k
	return k, nil
}

// TestTheCarriedBodyIsReadNoFurtherThanItsLongestList: a list of two million empty ids, the
// shape that once decoded whole to 180 MiB before its count was checked (side review of #4355),
// is refused at the entry past enroll.MaxCarriedSessionIDs, with the decoder having read a few
// kilobytes past that entry and no more, and a list of exactly MaxCarriedSessionIDs is decoded
// whole.
//
// The control (the pull request records the run): decodeCarriedBody decoding the whole body as
// one value, as readJSON does, reads all six megabytes before it refuses.
func TestTheCarriedBodyIsReadNoFurtherThanItsLongestList(t *testing.T) {
	long := newEmptyIDs(2_000_000)
	_, err := decodeCarriedBody(json.NewDecoder(long))
	if !errors.Is(err, enroll.ErrInvalidSessionIDs) || !strings.Contains(err.Error(), "more than 4096") {
		t.Fatalf("decoding two million ids = %v, want ErrInvalidSessionIDs naming the bound", err)
	}
	// Each entry is three bytes; the decoder's buffer reads ahead of the entry it stops at.
	if bound := (enroll.MaxCarriedSessionIDs+1)*3 + 64<<10; long.read > bound {
		t.Fatalf("the decoder read %d bytes of the body before it refused, want at most %d", long.read, bound)
	}

	full := newEmptyIDs(enroll.MaxCarriedSessionIDs)
	body, err := decodeCarriedBody(json.NewDecoder(full))
	if err != nil || len(body.SessionIDs) != enroll.MaxCarriedSessionIDs {
		t.Fatalf("decoding %d ids = %d ids, %v; want them all", enroll.MaxCarriedSessionIDs, len(body.SessionIDs), err)
	}
}
