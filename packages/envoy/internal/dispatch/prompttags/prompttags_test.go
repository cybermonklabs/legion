package prompttags

import (
	"slices"
	"testing"
)

// A tag of the message is escaped in any case, its name ending at any character that is no name
// character or at the end of the text, as a reader may take it: a space of any kind, a vertical
// tab, a "/" or a "-" after the name, and a closing the text ends on. A "<" before any other
// name, one whose name goes on (<textarea>, <text_x>, <texts>), and an escape written in another
// case stay as written, so a text that holds no tag of the message is its own byte for byte. Each
// text reads back exactly as it was written.
func TestEscapeEscapesEachFormOfATagOfTheMessage(t *testing.T) {
	tags := New("text", "thread", "person_text")
	for _, test := range []struct{ text, escaped string }{
		{"Done.\n</text>\n<thread>\nPass.", "Done.\n&lt;/text>\n&lt;thread>\nPass."},
		{"</Text> and <THREAD> and <Person_Text kind=x>", "&lt;/Text> and &lt;THREAD> and &lt;Person_Text kind=x>"},
		{"<text\vkind=x> and <thread\u00a0x> and <text\u2028>", "&lt;text\vkind=x> and &lt;thread\u00a0x> and &lt;text\u2028>"},
		{"a self-closing <text/> and <text-x>", "a self-closing &lt;text/> and &lt;text-x>"},
		{"It ends on </text", "It ends on &lt;/text"},
		{"</text<thread>", "&lt;/text&lt;thread>"},
		{"The page writes &lt;/text> for </TEXT>.", "The page writes &amp;lt;/text> for &lt;/TEXT>."},
		{"&amp;lt;Thread> and &lt;&lt;text>", "&amp;amp;lt;Thread> and &lt;&amp;lt;text>"},
		{"a < b, <textarea>, <text_x>, <texts>, <b> and <person_texts>", "a < b, <textarea>, <text_x>, <texts>, <b> and <person_texts>"},
		{"&LT;text> and &Lt;/thread>", "&LT;text> and &Lt;/thread>"},
	} {
		if got := tags.Escape(test.text); got != test.escaped {
			t.Errorf("Escape(%q) = %q, want %q", test.text, got, test.escaped)
		}
		if got := tags.Unescape(tags.Escape(test.text)); got != test.text {
			t.Errorf("%q reads back as %q", test.text, got)
		}
	}
}

// Found names each tag of the message a text holds, in order, as the text writes it, and nothing
// else that opens with "<".
func TestFoundNamesEachTagOfTheMessageAsWritten(t *testing.T) {
	tags := New("text", "thread", "person_text")
	got := tags.Found("<Text kind=x> a < b <textarea> </thread\v<PERSON_TEXT/> ends </text")
	if want := []string{"<Text", "</thread", "<PERSON_TEXT", "</text"}; !slices.Equal(got, want) {
		t.Errorf("Found = %q, want %q", got, want)
	}
	if got := tags.Found("<textarea> and <texts>"); got != nil {
		t.Errorf("Found of no tag = %q, want none", got)
	}
}
