package api

// going_named.go holds which invocations of a proposal may run at once, on submit (goingAutomatic):
// only one whose every item the page's chip holds or the person's own typed words name, read as
// Dispatch reads references in text (goingNamedBy), and whose every hand-over of the work - its
// route, its assignee - the words name or leaves as the issue holds it. The answer model writes
// the invocations from items any person or agent wrote, so anything only that text names is an
// offer the person picks.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The kinds of item an argument names, as goingNamed reads each.
const (
	goingNamesIssue    = "issue"
	goingNamesProject  = "project"
	goingNamesItem     = "item"
	goingNamesDocument = "document"
	goingNamesSession  = "session"
)

// goingItemArguments are the arguments that name an item of Dispatch's an invocation acts on,
// links to or moves beside, by the kind of item each names: an issue (every issue argument the
// search lists, the issues one waits on and the two a rank moves it between), the project, an
// ask, a comment, a message or a result by its id, a document by its id, slug or file name under
// its owner, and the session a message goes to.
var goingItemArguments = func() map[string]string {
	named := map[string]string{
		"project":    goingNamesProject,
		"session":    goingNamesSession,
		"retract":    goingNamesItem,
		"blocked_by": goingNamesIssue,
		"before":     goingNamesIssue,
		"after":      goingNamesIssue,
	}
	for argument, kind := range goingItemFields {
		switch kind {
		case "issue":
			named[argument] = goingNamesIssue
		case "document":
			named[argument] = goingNamesDocument
		default:
			// An ask, a comment or a message, each named by its id.
			named[argument] = goingNamesItem
		}
	}
	return named
}()

// goingHeldArguments are the arguments that hand an issue's work on, to a session (route) or to a
// person (assignee): each runs at once only where the words name its value or the issue already
// holds it (goingHeld).
var goingHeldArguments = map[string]bool{"route": true, "assignee": true}

// goingHeld is what an issue holds now of goingHeldArguments, by argument, "" for none, and false
// for an issue it could not read.
type goingHeld func(issue string) (map[string]string, bool)

// goingHeldBy reads what each issue holds of goingHeldArguments from the store, once for each
// issue asked about. A read that fails holds nothing, so the argument has to be named in the
// words: it is logged and answered false.
func (s *server) goingHeldBy(ctx context.Context) goingHeld {
	read := map[string]map[string]string{}
	return func(issue string) (map[string]string, bool) {
		if held, asked := read[issue]; asked {
			return held, held != nil
		}
		var route, assignee string
		err := s.deps.Store.Pool.QueryRow(ctx,
			`select coalesce(route, ''), coalesce(assignee, '') from issues where key = $1`, issue).Scan(&route, &assignee)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.Warn("dispatch going: read what an issue holds of a proposal's hand-over", "issue", issue, "error", err)
			}
			read[issue] = nil
			return nil, false
		}
		held := map[string]string{"route": route, "assignee": assignee}
		read[issue] = held
		return held, true
	}
}

// goingAutomatic reports whether an invocation may run at once, on submit: only where every item
// it names (goingItemArguments, each text and each text of a list) is the chip's issue or project
// - chip is nil where the decision did not read the words as about the page - or is named in the
// person's own typed words (goingNamed.names), and every hand-over it makes (goingHeldArguments)
// is named in the words or leaves the issue as it is (goingNamed.holds). The one argument the bar
// asks the person for is not among the arguments here: what the person fills is theirs.
func goingAutomatic(arguments map[string]any, chip *goingContext, named goingNamed, held goingHeld) bool {
	for argument, value := range arguments {
		if goingHeldArguments[argument] {
			if !named.holds(argument, value, arguments, held) {
				return false
			}
			continue
		}
		kind, names := goingItemArguments[argument]
		if !names {
			continue
		}
		values := []any{value}
		if list, isList := value.([]any); isList {
			values = list
		}
		for _, element := range values {
			item, isText := element.(string)
			if !isText || strings.TrimSpace(item) == "" {
				continue
			}
			if !named.names(kind, item, arguments, chip) {
				return false
			}
		}
	}
	return true
}

// goingNamed is what the person's own typed words name, read as Dispatch reads references in text
// (text.Mentions): every issue key they write bare or in a reference; every project key written
// in upper case as a token of its own, exactly as the key is spelled, or in a reference; every
// id of an ask, a comment, a message or a result they write, whole or short, bare or in a
// reference; every document a reference names, under its owner; and every whitespace-delimited
// token, for a session, a route or a person, which have no reference of their own. A reference
// names its owner too: a link to an issue's document or ask names that issue. A word that is no
// reference names nothing, whatever its letters spell, so a project key that is an English word
// (AS, THE) is named only where the person typed it in upper case.
type goingNamed struct {
	words     string
	issues    map[string]bool
	projects  map[string]bool
	ids       map[string]bool
	documents map[string]bool
	tokens    map[string]bool
}

// goingEveryKey is the resolver the typed words are read with (text.Mentions): every bare issue
// key they write names that issue - whether it exists is the write's to find out - and they
// belong to no document, so a `#id` names nothing and a short id stays as written.
type goingEveryKey struct{}

func (goingEveryKey) Issues(keys []string) (map[string]bool, error) {
	every := make(map[string]bool, len(keys))
	for _, key := range keys {
		every[key] = true
	}
	return every, nil
}

func (goingEveryKey) Questions() (map[string]text.Ref, error) { return nil, nil }

func (goingEveryKey) Items([]text.Prefix) (map[int]string, error) { return nil, nil }

// goingNamedBy reads words, against the server's own URL so a dashboard address names its page.
func goingNamedBy(words, serverURL string) goingNamed {
	named := goingNamed{
		words: words, issues: map[string]bool{}, projects: map[string]bool{}, ids: map[string]bool{},
		documents: map[string]bool{}, tokens: map[string]bool{},
	}
	// goingEveryKey answers no error, so Mentions answers none.
	located, _ := text.Mentions(words, serverURL, goingEveryKey{})
	for _, ref := range located {
		if ref.IssueKey != "" {
			named.issues[ref.IssueKey] = true
		}
		if ref.Project != "" {
			named.projects[ref.Project] = true
		}
		owner := ref.IssueKey + ref.Project
		switch {
		case ref.Kind == "artifact" || ref.Kind == "block" && ref.Item == "":
			named.documents[owner+"/"+ref.ID] = true
		case ref.Kind == "ask" || ref.Kind == "comment" || ref.Kind == "message" || ref.Kind == "block":
			if id, isID := goingIDOf(ref.ID); isID {
				named.ids[id] = true
			}
			if ref.Document != "" {
				named.documents[owner+"/"+ref.Document] = true
			}
		}
	}
	tokens := strings.FieldsFunc(words, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	})
	for _, token := range tokens {
		if projectKeyPattern.MatchString(token) {
			named.projects[token] = true
		}
		if id, isID := goingIDOf(token); isID {
			named.ids[id] = true
		}
	}
	for _, field := range strings.Fields(words) {
		if token := strings.Trim(field, `.,;:!?()[]"'<>`); token != "" {
			named.tokens[token] = true
		}
	}
	return named
}

// names reports whether item, the value of an argument naming an item of kind, is the chip's or
// is named in the words. A document's slug or file name is named only where its owner - the
// invocation's issue, else its project - is the chip's or named in the words too, and the words
// name the document under that owner by reference or write its name as a word (goingWordsName):
// a slug alone is an ordinary word any document of any owner may carry.
func (n goingNamed) names(kind, item string, arguments map[string]any, chip *goingContext) bool {
	switch kind {
	case goingNamesIssue:
		return chip != nil && chip.Issue != "" && strings.EqualFold(chip.Issue, item) || n.issues[item]
	case goingNamesProject:
		return chip != nil && chip.Project != "" && strings.EqualFold(chip.Project, item) || n.projects[item]
	case goingNamesItem:
		id, isID := goingIDOf(item)
		return isID && n.ids[id]
	case goingNamesSession:
		return n.tokens[item]
	case goingNamesDocument:
		if goingIsUUID(item) {
			id, _ := goingIDOf(item)
			return n.ids[id]
		}
		for _, owner := range []string{goingNamesIssue, goingNamesProject} {
			key, isText := arguments[owner].(string)
			if !isText || key == "" {
				continue
			}
			return n.names(owner, key, arguments, chip) && (n.documents[key+"/"+item] || goingWordsName(n.words, item))
		}
	}
	return false
}

// holds reports whether a hand-over argument may run at once: its value is named whole in the
// words as a token of its own, or it is what the invocation's issue already holds, so the write
// leaves it as it is. A value that clears it (null, "") is never named, so it runs at once only on
// an issue that holds none.
func (n goingNamed) holds(argument string, value any, arguments map[string]any, held goingHeld) bool {
	given, _ := value.(string)
	if given != "" && n.tokens[given] {
		return true
	}
	issue, _ := arguments["issue"].(string)
	if held == nil || issue == "" {
		return false
	}
	current, read := held(issue)
	return read && current[argument] == given
}

// goingIDOf is an item's id as the words and an argument are compared by, and whether value is an
// id at all: a whole uuid in its canonical spelling, and an id written short (text.IsIDPrefix) in
// lower case, since hex is read without case.
func goingIDOf(value string) (string, bool) {
	if goingIsUUID(value) {
		return uuid.MustParse(value).String(), true
	}
	if text.IsIDPrefix(value) {
		return strings.ToLower(value), true
	}
	return "", false
}

// goingIsUUID reports whether value is a whole uuid, in either spelling an id's characters allow:
// 8-4-4-4-12 hex digits, or 32 hex digits.
func goingIsUUID(value string) bool {
	if len(value) != 36 && len(value) != 32 {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

// goingWordsName reports whether the typed words write a document's name as a word: name occurs
// in them, with case ignored, and neither end runs on into a letter, a digit, a hyphen or an
// underscore, so "the notes" writes notes and not notes-old, and "spec." writes spec. It is read
// only beside the document's owner (goingNamed.names): a name alone is no reference.
func goingWordsName(words, name string) bool {
	haystack, needle := strings.ToLower(words), strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		return false
	}
	for from := 0; ; {
		at := strings.Index(haystack[from:], needle)
		if at < 0 {
			return false
		}
		start, end := from+at, from+at+len(needle)
		if (start == 0 || !goingWordRune(haystack[start-1])) && (end == len(haystack) || !goingWordRune(haystack[end])) {
			return true
		}
		from = start + 1
	}
}

// goingWordRune reports whether b continues a word a document is named by: a letter, a digit, a
// hyphen or an underscore, or any byte of a character past ASCII.
func goingWordRune(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b >= 0x80
}
