// packages/envoy/cmd/cipher/answer_words.go
//
// How the request, status and exec forms word a broker's answer about a request: who decides it and
// where, the broker's sentence about it, and its id, each as the broker's address lets its answer
// print (cipheraddr.Address.Words), and the command that reads it again.
package main

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// reportPending says, once, before the exec form's wait, who decides the request — a person, or
// the model judge while it holds the request — where the secrets it would inject come from (src),
// and on which page of the broker a person decides it when its answer names one (waitingOn).
// shownID is the request's id as the exec form prints it.
func reportPending(stderr io.Writer, base cipheraddr.Address, shownID string, approver *string, brokerPage string, judging bool, wait time.Duration, src source) {
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
// (cipheraddr.Address.Words): on one line beside a clean address, Go-quoted where a control
// character in it would start a line of its own, and not at all beside one that carries a
// credential.
//
// A request the model judge holds (judging, the answer's judge_deciding) carries no page: its
// keys are ones the judge decides, so the sentence says the judge is deciding it and that nobody
// is asked, and never that its approver must approve it. If the judge cannot decide it, a later
// answer carries the page, and the exec form says so.
func waitingOn(base cipheraddr.Address, approver *string, brokerPage string, judging bool) string {
	if judging {
		return "the model judge is deciding it; nobody is asked to approve it unless the judge cannot decide it"
	}
	where := ""
	if brokerPage != "" {
		where = ": " + base.Words(brokerPage)
	}
	switch {
	case approver == nil:
		return "waiting for approval on the secrets broker" + where + "; the broker does not say whose"
	case *approver == record.AnyoneApprover:
		return "waiting for anyone signed in to the secrets broker to approve it" + where
	default:
		return "waiting for " + base.Words(*approver) + " to approve it on the secrets broker" + where
	}
}

// deniedBecause is what the exec form's denied line adds to name why: ": " and the broker's own
// sentence about the refusal (RequestResult.Detail), which names the secret that refused the
// request and the rule that did, as ": GCLOUD_DEMO: requester-not-listed". An agent reading that
// line on stderr then knows which secret it may not have and that asking again will not help,
// rather than only that something was denied. A broker that recorded no detail, and one from
// before the field, adds nothing, so such a denial reads exactly as it always has. The sentence
// prints as the broker's address base lets its answer print (cipheraddr.Address.Words).
func deniedBecause(base cipheraddr.Address, detail *string) string {
	if detail == nil {
		return ""
	}
	return ": " + base.Words(*detail)
}

// printDetail writes the broker's own sentence about a request, the one the request row keeps as
// its decision_detail, as the "detail: <text>" line the request and status forms both end with,
// as the broker's address base lets its answer print (cipheraddr.Address.Words). A broker that
// recorded none, and one from before the field, writes no line at all, so such an answer reads
// exactly as it always has.
func printDetail(stdout io.Writer, base cipheraddr.Address, detail *string) {
	if detail == nil {
		return
	}
	fmt.Fprintf(stdout, "detail: %s\n", base.Words(*detail))
}

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
// UUID, as many API tokens are - so it prints as Words.
func idWords(base cipheraddr.Address, id string) string {
	if !base.Clean() && brokerIDShape.MatchString(id) && plainAddress(base) && !addressHolds(base, id) {
		return id
	}
	return base.Words(id)
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
// status and the id, quoted for the shell, where the id prints as it is (idWords); beside a clean
// address, an id with a control character in it Go-quoted, as the lines before it print it; and
// beside an address that is not clean, for an id that does not print, the form to run and that
// its id is not printed, since a command with no id would not run.
func statusHint(base cipheraddr.Address, requestID string) string {
	shown := idWords(base, requestID)
	switch {
	case shown == requestID:
		return "Check it with: agent-secrets status " + shellWord(requestID)
	case base.Clean():
		return "Check it with: agent-secrets status " + shown
	}
	return "Check it with agent-secrets status and the request's id, which is not printed: the broker's address is not clean, and the id is no UUID the broker makes apart from the address's own text"
}
