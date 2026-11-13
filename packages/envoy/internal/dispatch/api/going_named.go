package api

// going_named.go holds which invocations of a proposal may run at once, on submit (goingAutomatic):
// only one whose every item the page's chip holds or the person's own typed words name, read as
// Dispatch reads references in text (goingNamedBy), whose every hand-over of the work - its
// route, its assignee - the words name or leaves as the issue holds it, and whose upload makes a
// document rather than writing over one only stored text named. The answer model writes the
// invocations from items any person or agent wrote, so anything only that text names is an offer
// the person picks. Every argument of the catalogue is classed here, and one that is not runs
// nothing at once.

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

// goingHeldArguments are the arguments that hand work on, to a session (route) or to a person: an
// issue's (assignee) or a project's unowned work (default_assignee). Each runs at once only where
// the words name its value or the invocation's issue already holds it (goingHeld); a project's
// default names no issue, so it runs at once only where the words name the person.
var goingHeldArguments = map[string]bool{"route": true, "assignee": true, "default_assignee": true}

// goingContentArguments are the arguments that name no item of Dispatch's: what a write says or
// sets on the item its other arguments name - a title, a status, a body, a document's content and
// its edits, an ask's options, a result's fields - and what places it there: a passage of the
// issue's own document (anchor, quote), a reference written into its text or refused (ref), a
// file path the bar refuses, a handoff's phase, which appends that phase's next version and
// overwrites nothing, and a new project's key, which a key already taken refuses. An upload's
// name is one of them for every action but the upload's own (goingUploadNames).
var goingContentArguments = map[string]bool{
	"action": true, "anchor": true, "body": true, "check": true, "commit": true, "components": true,
	"content": true, "delivery": true, "excluded": true, "external": true, "external_links": true,
	"failed": true, "force": true, "images": true, "include_verification": true, "key": true,
	"kind": true, "labels": true, "multiple": true, "name": true, "negative_control": true,
	"observed": true, "occurrence": true, "ops": true, "options": true, "path": true, "phase": true,
	"precondition": true, "priority": true, "question": true, "quote": true, "reason": true,
	"ref": true, "release": true, "replace_with": true, "repo": true, "role": true, "run": true,
	"screenshots": true, "skipped": true, "spec": true, "status": true, "summary": true,
	"surface": true, "title": true, "turn": true, "urgency": true, "verdict": true, "version": true,
}

// goingUploadNames is the argument of each action that names the document its upload writes,
// by action: an upload whose name is the name or the slug of a document its owner already holds
// is that document's next version (artifactByNameOrSlugSQL), so the name picks which document a
// create writes over (goingNamed.uploads).
var goingUploadNames = map[string]string{"dispatch_artifact": "name"}

// goingHeld is what an issue holds now of goingHeldArguments, by argument, "" for none, and false
// for an issue it could not read.
type goingHeld func(issue string) (map[string]string, bool)

// goingDocumentHeld reports whether owner holds a document whose name or slug name is, as an
// upload finds one, and false for a read that failed.
type goingDocumentHeld func(owner artifactTarget, name string) (held, read bool)

// goingHolds is what the store holds now that a proposal's invocations are marked automatic
// against: what each issue holds of its hand-overs (issue), and whether an upload's owner holds
// a document of its name (document). A nil read reads nothing, so what it would have answered has
// to be named in the words. What the document read answers is the proposal's hint alone: the
// write reads the document again, under the owner its route writes to, when it runs at once
// (goingUploadsOver).
type goingHolds struct {
	issue    goingHeld
	document goingDocumentHeld
}

// goingHoldsBy is the store's reads of goingHolds: what an issue holds of its hand-overs read
// once for each issue asked about, and whether an owner holds a document of a name read each time
// it is asked.
func (s *server) goingHoldsBy(ctx context.Context) goingHolds {
	return goingHolds{issue: s.goingHeldBy(ctx), document: s.goingDocumentHeldBy(ctx)}
}

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

// goingDocumentHeldBy reads from the store whether an owner holds a document of a name, by the
// lookup the upload itself makes (lookupArtifactByNameOrSlug), each time it is asked. A read that
// fails is logged and answered false, so the upload runs at once only where the words write its
// name.
func (s *server) goingDocumentHeldBy(ctx context.Context) goingDocumentHeld {
	return func(owner artifactTarget, name string) (bool, bool) {
		_, err := lookupArtifactByNameOrSlug(ctx, s.deps.Store.Pool, owner, name)
		switch {
		case err == nil:
			return true, true
		case errors.Is(err, pgx.ErrNoRows):
			return false, true
		}
		slog.Warn("dispatch going: read whether an upload's owner holds a document of its name", "name", name, "error", err)
		return false, false
	}
}

// goingAutomatic reports whether an invocation of action may run at once, on submit: only where
// every item it names (goingItemArguments, each text and each text of a list) is the chip's issue
// or project - chip is nil where the decision did not read the words as about the page - or is
// named in the person's own typed words (goingNamed.names), every hand-over it makes
// (goingHeldArguments) is named in the words or leaves the issue as it is (goingNamed.holds), and
// an upload's name (goingUploadNames) makes a document or names one the words write
// (goingNamed.uploads); session is the caller's own, which owns an upload naming no issue and no
// project. An argument none of these lists nor goingContentArguments classes is not automatic:
// what an argument names is never guessed. The one argument the bar asks the person for is not
// among the arguments here: what the person fills is theirs.
func goingAutomatic(action string, arguments map[string]any, chip *goingContext, session string, named goingNamed, holds goingHolds) bool {
	for argument, value := range arguments {
		if upload, uploads := goingUploadNames[action]; uploads && upload == argument {
			name, _ := value.(string)
			if !named.uploads(name, arguments, session, holds.document) {
				return false
			}
			continue
		}
		if goingHeldArguments[argument] {
			if !named.holds(argument, value, arguments, holds.issue) {
				return false
			}
			continue
		}
		kind, names := goingItemArguments[argument]
		if !names {
			if !goingContentArguments[argument] {
				return false
			}
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

// uploads reports whether an upload under name may run at once: the words write the name
// (goingWordsName), so the person asked for that document whether its owner holds one or not, or
// the owner the upload reaches (goingUploadOwner) holds no document whose name or slug it is, so
// the upload makes one. A name the owner already holds that only stored text wrote would write
// that document's next version, so it is an offer; so is one whose owner could not be read. This
// is the proposal's mark; the write holds an upload sent at once to it again when it runs
// (goingUploadsOver), since a document of the name may be made between the two.
func (n goingNamed) uploads(name string, arguments map[string]any, session string, document goingDocumentHeld) bool {
	name = strings.TrimSpace(name)
	if goingWordsName(n.words, name) {
		return true
	}
	owner, known := goingUploadOwner(arguments, session)
	if !known || document == nil {
		return false
	}
	held, read := document(owner, name)
	return read && !held
}

// goingUploadOwner is the owner an upload's arguments reach, as its rows are tried in order: the
// issue it names, else the project, else the caller's own session, each value as written, since
// the route matches it exactly; false where it names none and the caller proved no session.
func goingUploadOwner(arguments map[string]any, session string) (artifactTarget, bool) {
	if issue := goingText(arguments, "issue"); issue != "" {
		return artifactTarget{IssueKey: &issue}, true
	}
	if project := goingText(arguments, "project"); project != "" {
		return artifactTarget{Project: project}, true
	}
	return artifactTarget{Session: session}, session != ""
}

// goingRouteUploadOwner is the owner an upload's route writes to, resolved from the values of its
// path as that route resolves them: the issue (uploadArtifact), the project
// (uploadProjectArtifact) or the session (uploadAgentArtifact), each value exactly as the route
// reads it; false for a route that is no upload, or a value it lacks.
func goingRouteUploadOwner(route GoingRoute, values map[string]string) (artifactTarget, bool) {
	switch route.Method + " " + route.Pattern {
	case goingIssueUploadRoute:
		issue := values["key"]
		return artifactTarget{IssueKey: &issue}, issue != ""
	case goingProjectUploadRoute:
		return artifactTarget{Project: values["key"]}, values["key"] != ""
	case goingAgentUploadRoute:
		return artifactTarget{Session: values["session_id"]}, values["session_id"] != ""
	}
	return artifactTarget{}, false
}

// goingUploadsOver reports whether an upload the write is about to send would write the next
// version of a document only stored text named: the read before it (uploadedOver), made under
// the owner its route writes to as the store stands when the write runs, found a document of its
// name, and the person's typed words do not write that name (goingNamed.uploads, read again).
func goingUploadsOver(prepared goingPrepared, before goingObservation, words string) bool {
	argument, uploads := goingUploadNames[prepared.action]
	if !uploads {
		return false
	}
	_, held := before.value.(goingUploadBefore)
	return held && !goingWordsName(words, goingText(prepared.arguments, argument))
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
// underscore, so "the notes" writes notes and not notes-old, and "spec." writes spec. To name a
// document an argument points at (goingNamed.names) it is read only beside the document's owner,
// since a name alone is no reference; an upload's name (goingNamed.uploads, goingUploadsOver) it
// reads alone, since the words writing the name a document is made or written under is the
// person asking for that document by it.
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
