//go:build linux

package launcher

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// The parent keeps at most volumeDeleteReplyBytes of one reply of its deleter, and reads the rest
// of a longer one up to its newline and drops it, so the reply after it is read from its own
// start. A reply is a line the parent logs (trimmer.remove), so its size must be one this
// package chose rather than one the child happened to write. The replies here are a refusal 64
// times the bound, a refusal of ordinary size and a removal, read through the reader the remover
// is started with (newDeleterReplies). The negative control is the read with no bound
// (ReadString), which kept the whole of the long line and answered it as the refusal.
func TestADeletersReplyIsKeptToItsBoundAndTheNextReplyIsReadWhole(t *testing.T) {
	long := strings.Repeat("x", 64*volumeDeleteReplyBytes)
	replies := "failed " + strconv.Quote(long) + "\nfailed " + strconv.Quote("the second refusal") + "\nremoved\n"
	dir := &sweepDir{}
	remover := &containedRemover{requests: discardRequests{}, replies: newDeleterReplies(strings.NewReader(replies)),
		at: map[*sweepDir]int{dir: 0}, said: &saidBuffer{}}

	first := remover.removeAll(dir, "entry")
	if first == nil || errors.Is(first, errRemoverGone) {
		t.Fatalf("the long refusal was answered %.200v, want it as a refusal of that entry", first)
	}
	if said, bound := first.Error(), volumeDeleteReplyBytes+len(volumeDeleteReplyCut); len(said) > bound || !strings.HasSuffix(said, volumeDeleteReplyCut) {
		t.Errorf("the long refusal was answered as %d bytes ending %q, want at most %d ending %q",
			len(said), said[max(0, len(said)-len(volumeDeleteReplyCut)):], bound, volumeDeleteReplyCut)
	}
	if second := remover.removeAll(dir, "entry"); second == nil || second.Error() != "the second refusal" {
		t.Errorf("the reply after the long one was answered %v, want the refusal %q", second, "the second refusal")
	}
	if third := remover.removeAll(dir, "entry"); third != nil {
		t.Errorf("the third reply was answered %v, want the removal it says", third)
	}
}

// The bound is the most a reply may hold and still be kept whole, so a reply of exactly
// volumeDeleteReplyBytes is read as the deleter wrote it, and one byte more is cut. A reader that
// took the bound for a size a reply must stay below would mark a reply at the bound as cut and
// answer it still quoted, with the cut mark after a refusal nothing was cut from; the case above
// reads only a reply far past the bound and one far within it, so it cannot tell the two apart.
func TestADeletersReplyAtItsBoundIsKeptWholeAndOneByteMoreIsCut(t *testing.T) {
	for _, size := range []int{volumeDeleteReplyBytes, volumeDeleteReplyBytes + 1} {
		refusal := strings.Repeat("x", size-len("failed ")-len(`""`))
		reply := "failed " + strconv.Quote(refusal)
		if len(reply) != size {
			t.Fatalf("the reply is %d bytes, want %d", len(reply), size)
		}
		dir := &sweepDir{}
		remover := &containedRemover{requests: discardRequests{}, replies: newDeleterReplies(strings.NewReader(reply + "\nremoved\n")),
			at: map[*sweepDir]int{dir: 0}, said: &saidBuffer{}}

		got := remover.removeAll(dir, "entry")
		switch {
		case got == nil || errors.Is(got, errRemoverGone):
			t.Fatalf("a reply of %d bytes was answered %.200v, want it as a refusal of that entry", size, got)
		case size <= volumeDeleteReplyBytes && got.Error() != refusal:
			t.Errorf("a reply of %d bytes, the bound, was answered as %d bytes ending %q, want the refusal whole", size, len(got.Error()), got.Error()[max(0, len(got.Error())-len(volumeDeleteReplyCut)):])
		case size > volumeDeleteReplyBytes && !strings.HasSuffix(got.Error(), volumeDeleteReplyCut):
			t.Errorf("a reply of %d bytes, one past the bound, was answered without the mark %q", size, volumeDeleteReplyCut)
		}
		if next := remover.removeAll(dir, "entry"); next != nil {
			t.Errorf("the reply after one of %d bytes was answered %v, want the removal it says", size, next)
		}
	}
}

// discardRequests is a deleter's request pipe that goes nowhere, for a case that writes its
// replies itself.
type discardRequests struct{}

func (discardRequests) Write(p []byte) (int, error) { return len(p), nil }

func (discardRequests) Close() error { return nil }
