package textcheck

import (
	"errors"
	"fmt"
)

// errCommented is why a check refuses a Markdown text whose two readings disagree on it
// (bothReadings): an HTML comment holds what the check reads, or stands inside it.
var errCommented = errors.New("the text as written and as the rendered page shows it disagree on " +
	"it, so an HTML comment decides it: take the comment out of what the check reads")

// bothReadings is the verdict of a check that read a Markdown text twice: as written, which an
// agent or an editor reads, and as the rendered page shows it (visibleMarkdown), which a person
// reads; written and shown are its errors in each. Each check that reads a guide holds both, so
// that a passage only an HTML comment holds, which the page does not show, fails rather than
// passing, while the text as written is held too. A check that passes in one reading and fails in
// the other, or fails differently in each, is reading what a comment hides or what a comment
// stands inside of, and is refused for it (errCommented), naming each reading that fails. Where
// both fail alike, the written one answers, so a text whose comments stand outside what the check
// reads fails with the written reading's error alone.
func bothReadings(written, shown error) error {
	const asWritten, asShown = "as written, HTML comments and all", "as the rendered page shows it, its HTML comments hidden"
	switch {
	case written == nil && shown == nil:
		return nil
	case written == nil:
		return fmt.Errorf("%s: %w: %w", asShown, shown, errCommented)
	case shown == nil:
		return fmt.Errorf("%s: %w: %w", asWritten, written, errCommented)
	case written.Error() != shown.Error():
		return fmt.Errorf("%s: %w; %s: %w: %w", asWritten, written, asShown, shown, errCommented)
	}
	return written
}
