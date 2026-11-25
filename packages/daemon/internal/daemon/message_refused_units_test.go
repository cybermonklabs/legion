package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A message-refused notice counts the refused body and the words its begins block leaves out in
// one unit, the UTF-16 unit Dispatch counts a body in, as every count of carried words does
// (texts.Carry): its length is the body's units, and the words object's rest counts every unit of
// the words after what begins holds, the lines after the first among them. Before, the length
// counted runes while the rest counted UTF-16 units, so a character outside the Basic Multilingual
// Plane made the two disagree, and the rest counted only the rest of the first line, so a reason of
// two lines read as one begins held whole (rest 0, no more).
func TestAMessageRefusedNoticeCountsTheLengthAndTheRestInUTF16Units(t *testing.T) {
	long := strings.Repeat("the fuzz suite hangs \U0001F680 on this input ", 8)
	for _, tc := range []struct {
		name  string
		body  string
		words string // the words begins quotes from, the whole of them
		post  bool   // a message post rather than a status write's reason
	}{
		{name: "a worker's words of two lines", body: moveSentence(phase.Testing, phase.Implementing) + ": The fuzz suite hangs \U0001F680\nsecond line",
			words: "The fuzz suite hangs \U0001F680\nsecond line"},
		{name: "a worker's first line past the preview, then a second", body: moveSentence(phase.Testing, phase.Implementing) + ": " + long + "\nsecond line",
			words: long + "\nsecond line"},
		{name: "the daemon's own message of three lines", body: "READY #42 at head \U0001F680\n\nthe rest of the packet", post: true,
			words: "READY #42 at head \U0001F680\n\nthe rest of the packet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := statusWriteRefused("LEGION-2", record.OrdinaryStatusWrite("in_progress", "testing", tc.body), cause, statusWrites{status: true})
			if tc.post {
				rec = messagePostRefused("LEGION-2", record.MessagePost{Body: tc.body}, cause)
			}
			notice, err := record.NoticeOf(rec)
			if err != nil {
				t.Fatal(err)
			}
			got := refused(t, rec)
			if notice.Length != goldenUnits(tc.body) {
				t.Errorf("the notice's length is %d, want the body's %d UTF-16 units (it has %d runes), the unit Dispatch counts a body and the rest in",
					notice.Length, goldenUnits(tc.body), len([]rune(tc.body)))
			}
			if !strings.HasPrefix(tc.words, got.Begins) || got.Begins == "" {
				t.Fatalf("begins is %q, not the head of the words %q; this case proves nothing", got.Begins, tc.words)
			}
			if rest, want := got.Words["begins"].Rest, goldenUnits(tc.words)-goldenUnits(got.Begins); rest != want {
				t.Errorf("the words object's rest is %d, want %d: every UTF-16 unit of the words after begins (%q), the lines after the first among them",
					rest, want, got.Begins)
			}
		})
	}
}
