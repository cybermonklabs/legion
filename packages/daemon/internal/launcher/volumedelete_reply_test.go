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
	t.Parallel()
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
	t.Parallel()
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

// A removal asked of a directory the listing did not open is refused by the path that directory
// stands at, quoted: a shard is a name a role chose under the build cache, and a name holding a
// newline would otherwise begin a line of its own in the pod's log.
func TestARemovalAskedOfADirectoryTheListingDidNotOpenNamesItQuoted(t *testing.T) {
	t.Parallel()
	const shard = "0f\nthe sweep is fine"
	dir := &sweepDir{root: ReclaimableRoot{Path: "/legion/go/build"}, shard: shard}
	remover := &containedRemover{requests: discardRequests{}, replies: newDeleterReplies(strings.NewReader("")),
		at: map[*sweepDir]int{}, said: &saidBuffer{}}

	err := remover.removeAll(dir, "entry")
	if err == nil {
		t.Fatal("the remover took a request for a directory the listing never opened")
	}
	if strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), strconv.Quote("/legion/go/build/"+shard)) {
		t.Errorf("the remover answered %q, want the directory's path quoted and on one line", err)
	}
}

// A deleter that answers nothing, or answers outside the protocol, is gone: the sweep stops at
// that entry and keeps what it deleted (errRemoverGone, sweep), rather than reading the next
// line as the answer to the next request or taking a broken answer for a removal. The replies
// are an empty stream, which is a child that died before it answered, a line that is no answer
// of the protocol, and a refusal whose words are not quoted; each is read through the reader the
// remover is started with, and every later request is answered the same. What the child said on
// its standard error goes with it. The negative controls are the answer to the end of the replies
// and the default branch each returned as a plain error, which the sweep took for a removal that
// failed and went on to the next entry.
func TestADeleterThatAnswersNothingOrOutsideTheProtocolIsGone(t *testing.T) {
	t.Parallel()
	for name, replies := range map[string]string{
		"an empty reply stream":                  "",
		"a reply of no answer this protocol has": "bogus\n",
		"a refusal whose words are not quoted":   "failed it is not quoted\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := &sweepDir{}
			said := &saidBuffer{}
			_, _ = said.Write([]byte("volume-trim-delete: the child's own words\n"))
			remover := &containedRemover{requests: discardRequests{}, replies: newDeleterReplies(strings.NewReader(replies)),
				at: map[*sweepDir]int{dir: 0}, said: said}

			first := remover.removeAll(dir, "entry")
			if !errors.Is(first, errRemoverGone) {
				t.Fatalf("the remover answered %v, want %v", first, errRemoverGone)
			}
			if !strings.Contains(first.Error(), "the child's own words") {
				t.Errorf("the remover answered %q, want what the child said on its standard error with it", first)
			}
			if again := remover.removeAll(dir, "entry"); again != first {
				t.Errorf("the next request was answered %v, want the same %v: no later removal can be asked of a remover that is gone", again, first)
			}
		})
	}
}

// The deleter's standard error is kept to a bound: what it said goes into errRemoverGone's text
// and from there into the sweep's stop reason and a line of the pod's log, so its size is this
// package's to choose and not the child's. The first bytes are kept, which say why, and a mark
// says the rest was cut; every write is answered as written, so exec's copy goes on draining the
// pipe. The negative control is the buffer that kept everything, which put all 256 KiB the child
// wrote into the error.
func TestTheDeletersStandardErrorIsKeptToItsBound(t *testing.T) {
	t.Parallel()
	said := &saidBuffer{}
	const first = "volume-trim-delete: the error that says why"
	long := strings.Repeat("y", 64*volumeDeleteSaidBytes)
	for _, written := range []string{first + "\n", long, "the last of it\n"} {
		if n, err := said.Write([]byte(written)); n != len(written) || err != nil {
			t.Fatalf("a write of %d bytes was answered %d, %v, want the whole of it written", len(written), n, err)
		}
	}
	dir := &sweepDir{}
	remover := &containedRemover{requests: discardRequests{}, replies: newDeleterReplies(strings.NewReader("")),
		at: map[*sweepDir]int{dir: 0}, said: said}
	gone := remover.removeAll(dir, "entry")
	if !errors.Is(gone, errRemoverGone) {
		t.Fatalf("the remover answered %v, want %v", gone, errRemoverGone)
	}
	// The bytes kept, each newline among them written as "; " on the one line (trailing), the
	// mark, and the parentheses: the first write's one newline is the only one kept.
	trailing := said.trailing()
	if bound := len(" (") + volumeDeleteSaidBytes + strings.Count(first+"\n", "\n") + len(volumeDeleteSaidCut) + len(")"); len(trailing) > bound {
		t.Errorf("what the child said is carried as %d bytes, want at most %d", len(trailing), bound)
	}
	if !strings.Contains(trailing, first) || !strings.HasSuffix(trailing, volumeDeleteSaidCut+")") {
		t.Errorf("what the child said is carried as %.120q…%q, want its first words and the mark %q at the end", trailing, trailing[max(0, len(trailing)-60):], volumeDeleteSaidCut)
	}
	if !strings.HasSuffix(gone.Error(), trailing) {
		t.Errorf("the remover's error does not end with what the child said: %.200q", gone)
	}
}

// The bound is the most of the deleter's standard error kept whole, so what it said at exactly
// the bound is carried as it said it, with no mark, and one byte more is cut to the bound with the
// mark after it, as a reply at its bound is (the case above writes 64 times the bound, with which a
// buffer that marked a write at the bound as cut is as green). Each is written in two writes, the
// second of which meets the bound, since the room is counted across writes. The bound is written
// out, 4096 bytes, the bound a reply of the deleter is kept to (volumeDeleteReplyBytes), rather
// than read off volumeDeleteSaidBytes, which a case staged from would move with.
func TestTheDeletersStandardErrorAtItsBoundIsKeptWholeAndOneByteMoreIsCut(t *testing.T) {
	t.Parallel()
	const bound = 4096
	for _, size := range []int{bound, bound + 1} {
		said := &saidBuffer{}
		written := strings.Repeat("z", size)
		for _, part := range []string{written[:size-96], written[size-96:]} {
			if n, err := said.Write([]byte(part)); n != len(part) || err != nil {
				t.Fatalf("a write of %d bytes was answered %d, %v, want the whole of it written", len(part), n, err)
			}
		}
		want := " (" + written + ")"
		if size > bound {
			want = " (" + written[:bound] + volumeDeleteSaidCut + ")"
		}
		if got := said.trailing(); got != want {
			t.Errorf("what the child said in %d bytes is carried as %d bytes ending %q, want %d bytes ending %q", size, len(got), got[max(0, len(got)-60):], len(want), want[max(0, len(want)-60):])
		}
	}
}

// What the deleter said and a reply of it longer than its bound reach the sweep's error through
// redact.Read, so a cut that falls inside a URL's user name keeps no part of it: the bound of the
// standard error, a read of the standard error taken while the child is still writing it (ended
// can read it then, so trailing gives Read every text as cut), and the bound of a reply. The
// deleter writes paths and its own errors, which hold no "://"; the URL here stands for a text no
// rule of the deleter's own keeps out. The negative controls are trailing and readReply each
// without redact.Read, which carried "legion-rea".
func TestTheDeletersCutTextsKeepNoPartOfAURLTheirCutFallsInside(t *testing.T) {
	t.Parallel()
	const url = " https://legion-reader@h.example/p?token=planted-query-token-12 and more"
	const atTheCut = " https://legion-rea"
	assertNoPart := func(t *testing.T, what, text string) {
		t.Helper()
		for _, part := range []string{"legion-rea", "reader@", "planted"} {
			if strings.Contains(text, part) {
				t.Errorf("%s holds %q, a part of the URL's user name or query: …%q", what, part, text[max(0, len(text)-80):])
			}
		}
	}
	t.Run("the standard error at its bound", func(t *testing.T) {
		said := &saidBuffer{}
		filler := strings.Repeat("y", volumeDeleteSaidBytes-len(atTheCut))
		_, _ = said.Write([]byte(filler + url))
		got := said.trailing()
		if !strings.HasPrefix(got, " ("+filler) {
			t.Fatalf("what the child said is carried as %.80q…, want it to start with what came before the URL", got)
		}
		assertNoPart(t, "what the child said", got)
	})
	t.Run("the standard error read while the child still writes", func(t *testing.T) {
		said := &saidBuffer{}
		_, _ = said.Write([]byte("volume-trim-delete: failed at" + atTheCut))
		got := said.trailing()
		if !strings.Contains(got, "volume-trim-delete: failed at") {
			t.Fatalf("what the child said is carried as %q, want its words before the URL", got)
		}
		assertNoPart(t, "what the child said", got)
	})
	t.Run("a reply at its bound", func(t *testing.T) {
		filler := strings.Repeat("x", volumeDeleteReplyBytes-len(`failed "`)-len(atTheCut))
		reply := "failed " + strconv.Quote(filler+url)
		dir := &sweepDir{}
		remover := &containedRemover{requests: discardRequests{}, replies: newDeleterReplies(strings.NewReader(reply + "\nremoved\n")),
			at: map[*sweepDir]int{dir: 0}, said: &saidBuffer{}}
		got := remover.removeAll(dir, "entry")
		if got == nil || errors.Is(got, errRemoverGone) || !strings.HasSuffix(got.Error(), volumeDeleteReplyCut) {
			t.Fatalf("the reply was answered %.120v, want a refusal of that entry with the mark %q", got, volumeDeleteReplyCut)
		}
		assertNoPart(t, "the refusal", got.Error())
	})
}

// discardRequests is a deleter's request pipe that goes nowhere, for a case that writes its
// replies itself.
type discardRequests struct{}

func (discardRequests) Write(p []byte) (int, error) { return len(p), nil }

func (discardRequests) Close() error { return nil }
