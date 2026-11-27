// packages/envoy/cmd/cipher/answer_words.go
//
// How the request, status and exec forms word a broker's answer about a request: who decides it and
// where, the broker's sentence about it, and its id, each as the broker's address lets its answer
// print (cipheraddr.Address.WordsOf), and the command that reads it again. Each member is held as
// redact.RemoteText, which Answer.Decode holds with the record of the request it answers, so beside
// a clean address it prints as the rule reads it with that request's credentials redacted.
package main

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/redact"
)

// reportPending says, once, before the exec form's wait, who decides the request — a person, or
// the model judge while it holds the request — where the secrets it would inject come from (src),
// and on which page of the broker a person decides it when its answer names one (waitingOn).
// shownID is the request's id as the exec form prints it.
func reportPending(stderr io.Writer, base cipheraddr.Address, shownID string, approver *redact.RemoteText, brokerPage redact.RemoteText, judging bool, wait time.Duration, src source) {
	fmt.Fprintf(stderr, "agent-secrets: request %s to %s is waiting for approval; waiting up to %s\n", shownID, src, wait)
	fmt.Fprintf(stderr, "agent-secrets: %s\n", waitingOn(base, approver, brokerPage, judging))
}

// waitingOn is the sentence that says whom a pending request waits on and where they decide it:
// the approver its credential record names (a person's login, or anyone signed in for a shared
// secret), and the page where that person decides it. That page is brokerPage, the record's page
// on the secrets broker, which the broker's answer carries as approve_url. It is the one place a
// line of this command names the broker's own host, and it names it because a person must open
// that page: the host is the one AGENT_SECRETS_URL already names, and no other line says where an
// answer came from (source_test.go's TestNoSessionLineNamesAHostOrAStoreID). The approver alone
// chooses the wording, so one switch writes all three sentences and the page is the suffix that
// switch appends: a broker from before that page carries none, the suffix is empty, and the same
// sentence names the approver alone, with no page - the request waits on the same person, who
// opens the broker themselves. The broker names an approver for every pending request; one from
// before it did names none, and the sentence says so rather than guessing. The page and the
// approver are the broker's, so each prints as the broker's address base lets its answer print
// (cipheraddr.Address.WordsOf): on one line beside a clean address, Go-quoted where a control
// character in it would start a line of its own, with the credentials the request carried
// redacted, and not at all beside one that carries a credential. The approver is told apart from
// anyone as it came (RemoteText.Equal).
//
// A request the model judge holds (judging, the answer's judge_deciding) carries no page: its
// keys are ones the judge decides, so the sentence says the judge is deciding it and that nobody
// is asked, and never that its approver must approve it. If the judge cannot decide it, a later
// answer carries the page, and the exec form says so.
func waitingOn(base cipheraddr.Address, approver *redact.RemoteText, brokerPage redact.RemoteText, judging bool) string {
	if judging {
		return "the model judge is deciding it; nobody is asked to approve it unless the judge cannot decide it"
	}
	where := ""
	if !brokerPage.IsEmpty() {
		where = ": " + base.WordsOf(brokerPage)
	}
	switch {
	case approver == nil:
		return "waiting for approval on the secrets broker" + where + "; the broker does not say whose"
	case approver.Equal(record.AnyoneApprover):
		return "waiting for anyone signed in to the secrets broker to approve it" + where
	default:
		return "waiting for " + base.WordsOf(*approver) + " to approve it on the secrets broker" + where
	}
}

// deniedBecause is what the exec form's denied line adds to name why: ": " and the broker's own
// sentence about the refusal (RequestResult.Detail), which names the secret that refused the
// request and the rule that did, as ": GCLOUD_DEMO: requester-not-listed". An agent reading that
// line on stderr then knows which secret it may not have and that asking again will not help,
// rather than only that something was denied. A broker that recorded no detail, and one from
// before the field, adds nothing, so such a denial reads exactly as it always has. The sentence
// prints as the broker's address base lets its answer print (cipheraddr.Address.WordsOf).
func deniedBecause(base cipheraddr.Address, detail *redact.RemoteText) string {
	if detail == nil {
		return ""
	}
	return ": " + base.WordsOf(*detail)
}

// printDetail writes the broker's own sentence about a request, the one the request row keeps as
// its decision_detail, as the "detail: <text>" line the request and status forms both end with,
// as the broker's address base lets its answer print (cipheraddr.Address.WordsOf). A broker that
// recorded none, and one from before the field, writes no line at all, so such an answer reads
// exactly as it always has.
func printDetail(stdout io.Writer, base cipheraddr.Address, detail *redact.RemoteText) {
	if detail == nil {
		return
	}
	fmt.Fprintf(stdout, "detail: %s\n", base.WordsOf(*detail))
}

// requestState is a state of a request this CLI acts on, as the broker spells it: a word of this
// CLI's own, which a line may name whatever the address. Every decision on a request's state reads
// it through stateOf, so the set is written once.
type requestState string

// The states of a request this CLI acts on.
const (
	statePending   requestState = "pending"
	stateGranted   requestState = "granted"
	stateDenied    requestState = "denied"
	stateCancelled requestState = "cancelled"
	stateExpired   requestState = "expired"
)

// requestStates are the states of a request this CLI acts on, as stateOf reads them.
var requestStates = []string{string(statePending), string(stateGranted), string(stateDenied), string(stateCancelled), string(stateExpired)}

// stateOf is the one of requestStates the broker's state is, compared as it came
// (RemoteText.OneOf), and "" for any other.
func stateOf(state redact.RemoteText) requestState {
	known, _ := state.OneOf(requestStates...)
	return requestState(known)
}

// notSentBack is what a line says of an id the broker answered that this client does not send
// back to it (RemoteText.SendBack answers no text): one the rule does not print as it came - it
// echoes a credential its request carried, or the rule cannot read it as clean - which no request
// carries and no file keeps, since nothing of a field leaves it that a print of it does not show.
const notSentBack = "its id is text the rule does not print as it came, which this command sends nowhere"

// brokerIDShape is the shape of an id the broker makes: a lower-case UUID, which it gives every
// request, grant and enrollment (uuid.NewString, uuid.New) and every launcher credential.
var brokerIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// idWords is id, a request's, grant's, enrollment's or launcher credential's id the broker
// answered, as a message prints it: as base lets the broker's words print
// (cipheraddr.Address.Words), and beside an address that does not let them print, still whole where
// it has the shape of an id the broker makes (brokerIDShape), the address could carry no
// credential (plainAddress) and it does not hold the id (addressHolds). Such an id is no
// credential, and a person needs it to follow the request (statusHint) beside an address Clean
// refuses only for its host, such as one with an underscore or a trailing dot. An id the address
// holds is the one value an answerer there could echo to have it print - a password that is a
// UUID, as many API tokens are - so it prints as Words. id is text this client keeps: an id
// RemoteText.SendBack answered, which is the rule's reading of the id with its request's
// credentials redacted; an id held as the box prints through idWordsOf.
func idWords(base cipheraddr.Address, id string) string {
	if !base.Clean() && brokerIDShape.MatchString(id) && plainAddress(base) && !addressHolds(base, id) {
		return id
	}
	return base.Words(id)
}

// idWordsOf is idWords of id, an id held as the broker answered it: the id as RemoteText.SendBack
// answers it, where the rule prints it as it came, by idWords' rule, so its UUID exception beside
// an address that is not clean is judged on that text; and any other id as base lets the broker's
// words print (cipheraddr.Address.WordsOf) - beside a clean address as the rule reads it, with the
// credentials its request carried redacted.
func idWordsOf(base cipheraddr.Address, id redact.RemoteText) string {
	if text, sent := id.SendBack(); sent {
		return idWords(base, text)
	}
	return base.WordsOf(id)
}

// projectsWordsOf is the projects a session named when it enrolled, as the broker's self answers
// them: each as base lets the broker's words print (cipheraddr.Address.WordsOf), joined as
// oauthwire.JoinProjects joins them, or "-" for none.
func projectsWordsOf(base cipheraddr.Address, projects []redact.RemoteText) string {
	if len(projects) == 0 {
		return "-"
	}
	return oauthwire.JoinProjects(wordsOfEach(base, projects))
}

// wordsOfEach is each of fields as base lets the broker's words print
// (cipheraddr.Address.WordsOf), in order, for a line that names several of one answer's fields.
func wordsOfEach(base cipheraddr.Address, fields []redact.RemoteText) []string {
	shown := make([]string, len(fields))
	for i, field := range fields {
		shown[i] = base.WordsOf(field)
	}
	return shown
}

// plainAddress reports whether base, as written, has no user info, no query, no fragment and no
// percent sign: the places a credential goes, and the one escape that would let the text this
// checks differ from what the client sends (url.Parse takes a malformed escape in a query, and
// net/http sends the decoded user info). The hosts the id exception is for carry none of these.
func plainAddress(base cipheraddr.Address) bool {
	raw := base.Raw()
	parsed, err := url.Parse(raw)
	return err == nil && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery &&
		parsed.Fragment == "" && !strings.ContainsAny(raw, "%@?#")
}

// addressHolds reports whether id, a UUID, occurs in the address's own text (base.Raw, its path
// among it), in any case and with or without its dashes: the forms in which an answerer there
// could echo a part of the address as an id.
func addressHolds(base cipheraddr.Address, id string) bool {
	bare := strings.ReplaceAll(id, "-", "")
	return strings.Contains(strings.ReplaceAll(strings.ToLower(base.Raw()), "-", ""), bare)
}

// statusHint is the command that reads requestID's state again, as a person pastes it: agent-secrets
// status and the id, quoted for the shell, where the id prints as it came (idWordsOf, of the text
// RemoteText.SendBack answers); beside a clean address, an id the rule changes as the lines before
// it print it - Go-quoted for a control character, a credential its request carried redacted; and
// beside an address that is not clean, for an id that does not print, the form to run and that its
// id is not printed, since a command with no id would not run.
func statusHint(base cipheraddr.Address, requestID redact.RemoteText) string {
	id, sent := requestID.SendBack()
	shown := idWordsOf(base, requestID)
	switch {
	case sent && shown == id:
		return "Check it with: agent-secrets status " + shellWord(id)
	case base.Clean():
		return "Check it with: agent-secrets status " + shown
	}
	return "Check it with agent-secrets status and the request's id, which is not printed: the broker's address is not clean, and the id is no UUID the broker makes apart from the address's own text"
}

// unaskedNames is what a line prints in place of the names a release holds that this run did not
// ask for (releasedName): a key of the broker's answer, which no record holds.
const unaskedNames = "not printed: a name of the broker's answer, which the rule cannot read with the release request's credentials redacted"

// releasedName is name, a name a release holds - a key of its values or of its airlock's names -
// as a line names it: as this run asked for it (asked, the names the person typed, which the key
// equals as it came), and otherwise as a name this run did not ask for, not printed. A key is the
// broker's text, or the text of whatever answers in its place, which can echo the release's Proof
// header; encoding/json decodes a key into no redact.RemoteText, so no record of the request holds
// it and the rule could not read it with that request's credentials redacted.
func releasedName(name string, asked []string) string {
	if slices.Contains(asked, name) {
		return name
	}
	return "a name this run did not ask for (" + unaskedNames + ")"
}
