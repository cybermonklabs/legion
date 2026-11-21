package approval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

const (
	// tokenLifetime is how long a rendered page's decision forms stay good.
	tokenLifetime = time.Hour
	// maxRecords bounds the records one page prints with a button, and so the records one POST
	// may name and the outcomes one page reports.
	maxRecords = 100
	// timeLayout is how the page prints an instant.
	timeLayout = "2006-01-02 15:04:05 UTC"
)

// recordView is one record as the page prints it, every field from the broker's own record.
type recordView struct {
	ID string
	// MachineLogin is a launcher_credential record, decided by its typed code on Dispatch's
	// machine-login page and never here.
	MachineLogin bool
	// Identifiers are the secret names an agent_secret record asks for, or a machine login's host.
	Identifiers []string
	// Enrollment is the requesting enrollment the broker verified; nil for a machine login.
	Enrollment *record.Enrollment
	// EnrollmentSession is the session id that enrollment stated when it enrolled.
	EnrollmentSession string
	// RequestSession is the session id the request itself named, its own unsigned claim, set only
	// when it differs from EnrollmentSession.
	RequestSession string
	Reason         string
	// Lifetime is how long the grant an approval mints lasts, worded to read after both "Lasts
	// once approved" and "Approve gives this session the value of these secrets": "until the
	// session ends", or "for <duration>" when the request signed one.
	Lifetime    string
	RequestedAt string
	ExpiresAt   string
	Approver    string
	// State is the record's state, "expired" too for one past its expiry the sweeper has not
	// ended yet; DecidedBy, DecidedAt and Detail are its decision's. DecidedBy is empty for a
	// record nobody decided; it names the model judge where the judge decided it, and the
	// current policy where the policy made the answer (decidedBy).
	State, DecidedBy, DecidedAt, Detail string
	// DecidedByJudge is whether DecidedBy names the model judge rather than a person. The page
	// prints the two differently: a person's login as it is, and the judge's name as the literal
	// id it is, in the same <code> every other id on the page is in.
	DecidedByJudge bool
	// Pending is a record still waiting for a decision.
	Pending bool
	// MayDecide is whether the signed-in person may decide it (record.MayDecide).
	MayDecide bool
	// Buttons is whether the page offers Approve and Deny for it: a pending agent_secret record
	// the person may decide, and not one the model judge holds.
	Buttons bool
	// JudgeDeciding is whether the model judge holds this record now. The page prints such a
	// record read-only: nobody decides it here until the judge has given it up, and if the judge
	// cannot decide it, it comes back to its person as an ordinary card.
	JudgeDeciding bool
	// Projects is one row per requested secret tagged with a Dispatch project: the secret, its
	// project, whether that project is private (or one Dispatch does not hold), and whether the
	// requesting session names it. Empty for a record touching no project, which prints as before.
	Projects []projectRow
	// SessionProjects are the projects the session names: those of the credential that asked and
	// of the one the request hangs off now, which differ after the session moves to another box
	// (requests.RecordDetail.SessionProjects).
	SessionProjects []string
	// Unanswered, when not empty, is the one sentence the page prints for a record Dispatch could
	// not say whether the person may see (requests.ErrProjectAccessUnavailable): the page then
	// prints its id and that sentence and nothing else of it, no button included (unansweredView).
	Unanswered string
}

// unansweredText is what the page says of a request Dispatch could not answer for, after "the
// secrets broker": in that request's own box and in its outcome line alike.
const unansweredText = "could not ask Dispatch whether you may see this request, so it shows nothing of it and cannot decide it until Dispatch answers. Try again shortly."

// unansweredView is record id as the page prints it when Dispatch could not say whether the
// person may see it: its id and unansweredText, and nothing the record holds, since its names and
// its reason may be a private project's.
func unansweredView(id string) recordView {
	return recordView{ID: id, Unanswered: "The secrets broker " + unansweredText}
}

// unansweredOutcome is the outcome line of record id when Dispatch could not say whether the
// person may see it: its id rather than its names, for the same reason.
func unansweredOutcome(id string) string {
	return id + ": unavailable: the secrets broker " + unansweredText
}

// projectRow is one tagged secret of a record as the page prints it.
type projectRow struct {
	Secret, Project string
	Private, Named  bool
}

// tokenFields are the hidden fields every decision form carries: the records the page printed with
// a button, the token binding them to the session, and its expiry.
type tokenFields struct {
	Printed []string
	Token   string
	Expires string
}

// pageView is the record page.
type pageView struct {
	chrome
	// Outcomes are the lines a decision's redirect reports, one per record it decided.
	Outcomes []string
	// Records are the record the URL names, first, and then the other pending agent_secret
	// records the person may decide.
	Records []recordView
	// Unshown counts the pending records left off the page for maxRecords.
	Unshown int
	// Form is the decision forms' hidden fields; ApproveAll is every record Form.Printed names,
	// in page order, for the one "Approve all N shown" form, offered when there are two or more.
	Form            tokenFields
	ApproveAll      []string
	MachineLoginURL string
}

// ServeRecord is GET /approve/{record}: the page for one record. It checks the id's shape, then
// the session, then the record, in that order: a probe from a machine with no person behind it
// must get the sign-in redirect rather than a 404 that tells it whether a record id exists. So an
// id that is not a record id answers 400 whatever the session; with no live session it starts a
// sign-in that returns here, reading no record and naming no login_hint, so the redirect is the
// same for an id a record has as for one none has; and a signed-in person's unknown record id is
// a 404. With a live session, ?switch=1 (a person signing in as someone else) starts a sign-in
// that names the record's approver as login_hint when it names one person: that person already
// reads whether the record exists from the page. The page
// prints the record and every other pending agent_secret record the person may decide, with
// Approve and Deny for each the person may decide and one "Approve all N shown" when N is two or
// more, and, after a decision, one outcome line per decided record, read from the broker's own
// record now. Each secret request is printed with two sentences of its own: what Approve gives
// and for how long, and what Deny and the expiry do. A machine login's record is printed with
// the note that its typed code is decided on Dispatch's machine-login page, and offers no
// button.
//
// A record Dispatch could not say whether the person may see (requests.ErrProjectAccessUnavailable)
// is that one request unavailable, not the page: it answers 503 with the record's box saying so
// and showing nothing else of it (unansweredView), and the outcome lines and the other pending
// requests print as ever. Its sign-in as someone else names no login_hint, since the approver is
// part of what it does not show; a record the person may not see answers 404 before it, as an id
// no record has. The record is read for the person (requests.Machine.ReadRecordFor), which reads
// whether they may see it from its rows first and the record only after: so a record signed for
// another broker address, which this broker cannot read, answers a person it hides from 404 too.
func (p *Page) ServeRecord(w http.ResponseWriter, r *http.Request, sess Session) {
	id := r.PathValue("record")
	if !record.ValidID(id) {
		writeMessage(w, http.StatusBadRequest, codeRecordIDInput, say("Not a request", "Credential-request record ids are lowercase-hex SHA-256 hashes."))
		return
	}
	switching := r.URL.Query().Get("switch") == "1"
	if !sess.live() {
		p.beginSignIn(w, r, id, "", switching)
		return
	}
	detail, err := p.cfg.Machine.ReadRecordFor(r.Context(), id, sess.Login)
	unanswered := errors.Is(err, requests.ErrProjectAccessUnavailable)
	switch {
	case unanswered:
		slog.Warn("approval page: Dispatch could not say whether the person may see the request", "record", id, "login", sess.Login, "error", err)
		detail = requests.RecordDetail{RecordID: id}
	case errors.Is(err, pgx.ErrNoRows):
		m := say("No such request", "The secrets broker has no credential request "+id+".")
		m.Login = sess.Login
		writeMessage(w, http.StatusNotFound, codeRecordNotFound, m)
		return
	case err != nil:
		writeUnavailable(w, "read the request", err)
		return
	}
	if switching {
		p.beginSignIn(w, r, id, approverHint(detail.Approver), true)
		return
	}

	now := time.Now()
	main, status := unansweredView(id), http.StatusServiceUnavailable
	if !unanswered {
		main, status = p.recordView(detail, sess.Login, now), http.StatusOK
	}
	view := pageView{
		chrome:          chrome{Title: "Approve a secret request", Login: sess.Login, Record: id},
		Records:         []recordView{main},
		MachineLoginURL: p.cfg.MachineLoginURL,
	}
	if view.Outcomes, err = p.outcomes(r.Context(), r.URL.Query().Get("decided"), r.URL.Query().Get("refused"), sess.Login, now, detail, unanswered); err != nil {
		writeUnavailable(w, "read the decided requests", err)
		return
	}
	var printed []string
	if main.Buttons {
		printed = append(printed, id)
	}
	pending, err := p.cfg.Machine.PendingForApprover(r.Context(), sess.Login, requests.PendingStates{})
	if err != nil {
		writeUnavailable(w, "list the pending requests", err)
		return
	}
	for _, s := range pending {
		if s.Kind != record.KindAgentSecret || s.RecordID == id {
			continue
		}
		if len(printed) == maxRecords {
			view.Unshown++
			continue
		}
		other, err := p.cfg.Machine.ReadRecord(r.Context(), s.RecordID)
		switch {
		case errors.Is(err, requests.ErrProjectAccessUnavailable):
			// Listed a moment ago, and Dispatch could not answer for its secrets' projects
			// since: left off, as the list itself leaves off such a request.
			continue
		case err != nil:
			writeUnavailable(w, "read a pending request", err)
			return
		}
		if v := p.recordView(other, sess.Login, now); v.Buttons {
			view.Records = append(view.Records, v)
			printed = append(printed, v.ID)
		}
	}
	if len(printed) > 0 {
		expires := now.Add(tokenLifetime).Unix()
		view.Form = tokenFields{Printed: printed, Token: pageToken(sess.secret, expires, printed), Expires: strconv.FormatInt(expires, 10)}
	}
	if len(printed) > 1 {
		view.ApproveAll = printed
	}
	writePage(w, status, "record.html", view)
}

// approverHint is approver as a login_hint: the person's own login, and nothing for a shared
// secret, whose approver is anyone signed in.
func approverHint(approver string) string {
	if approver == record.AnyoneApprover {
		return ""
	}
	return approver
}

// recordView is detail as the page prints it to login at now.
func (p *Page) recordView(d requests.RecordDetail, login string, now time.Time) recordView {
	v := recordView{
		ID: d.RecordID, MachineLogin: d.Kind == record.KindLauncherCredential,
		Identifiers: d.Identifiers, Enrollment: d.Enrollment,
		EnrollmentSession: d.Session.Enrollment,
		Reason:            d.Reason, Lifetime: "until the session ends",
		RequestedAt: d.RequestedAt.UTC().Format(timeLayout), ExpiresAt: d.ExpiresAt.UTC().Format(timeLayout),
		Approver: approverText(d.Approver), State: d.State,
		MayDecide: record.MayDecide(d.Kind, d.Approver, d.Service, login),
	}
	if d.Session.Request != "" && d.Session.Request != d.Session.Enrollment {
		v.RequestSession = d.Session.Request
	}
	if d.LifetimeSeconds != nil {
		v.Lifetime = "for " + (time.Duration(*d.LifetimeSeconds) * time.Second).String()
	}
	if d.Decided != nil {
		v.DecidedBy, v.DecidedByJudge = decidedBy(*d.Decided)
		v.DecidedAt, v.Detail = d.Decided.At.UTC().Format(timeLayout), d.Decided.Detail
	}
	v.Pending = d.State == "pending" && d.ExpiresAt.After(now)
	if d.State == "pending" && !v.Pending {
		v.State = "expired"
	}
	v.JudgeDeciding = v.Pending && d.JudgeDeciding
	v.Buttons = v.Pending && !v.MachineLogin && v.MayDecide && !v.JudgeDeciding
	v.SessionProjects = d.SessionProjects
	for _, sp := range d.SecretProjects {
		v.Projects = append(v.Projects, projectRow{Secret: sp.Name, Project: sp.Project, Private: sp.Private, Named: slices.Contains(d.SessionProjects, sp.Project)})
	}
	return v
}

// policyDecider is who the page names for an event the current policy answered rather than the
// person or the model judge that pressed Approve: requests.PolicyDeniedActor's own. It is
// worded for a reader instead of printed as the actor string the row holds, since nobody signs
// in as "broker:policy" and every other name this file prints is a login or the judge's id.
const policyDecider = "the current policy"

// decidedBy is who the page says decided a record, read from its terminal event: the model judge
// where the event's actor is the judge's, the current policy where the policy made the answer,
// and the deciding person's login otherwise. It is empty where nobody decided the record — an
// expiry, or a cancellation the requesting session's end wrote — since such an event carries no
// login and neither of those two actors.
//
// The judge's event carries no login at all, on purpose: the person who would otherwise have
// decided is named nowhere in it (decider.eventLogin). Its actor is therefore the only thing
// that names the decider, and a page that read the login alone printed "approved at <time>" with
// nobody beside it. The name this returns for the judge is the one the audit row holds,
// "judge:<model id>", which is also what requests.decided_by keeps and what
// `agent-secrets status` prints under decided_by: one name for the judge everywhere a person
// reads one.
//
// The policy's own denial is where that leaves the judge with no name at all. A judge's approval
// of a request whose session the current policy has dropped from the secret's requesters tag is
// recorded denied under requests.PolicyDeniedActor (applyDecisionAttempt), which overwrites the
// judge's actor, so the event holds neither a login nor a judge actor and the page named nobody.
// The policy is what such an event names, since the policy and not the decider made the answer.
// A person's own approval the policy denies keeps the login of whoever pressed Approve beside
// that same actor, and reads as it always has.
func decidedBy(d requests.RecordDecision) (name string, judge bool) {
	if _, byTheJudge := record.JudgeLogin(d.Actor); byTheJudge {
		return d.Actor, true
	}
	if d.Login == "" && d.Actor == requests.PolicyDeniedActor {
		return policyDecider, false
	}
	return d.Login, false
}

// approverText is who a record's approver field says may decide it.
func approverText(approver string) string {
	if approver == record.AnyoneApprover {
		return "anyone signed in to this broker"
	}
	return approver
}

// outcomes is one line for each record id the comma-separated decided names, at most maxRecords
// of them, each said from the broker's own record now and never from the URL: an id that is not a
// record id, or names no record, gets none, and nor does a record of a private project login may
// not see (requests.Machine.ReadRecordFor), whose line would print its secrets' names. read is the
// record the page has just read, which a decision's own page always names, so it is not read
// again; readUnanswered says Dispatch could not say whether login may see it. A record Dispatch
// could not answer for gets the line that it is unavailable, naming its id and nothing it holds
// (unansweredOutcome), and the other lines print as ever. refused is the comma-separated id:code
// pairs ServeDecide wrote for the records it could not decide (refusalCodes); a record still
// pending names the reason its code picks, and a pair whose code is not one of them is ignored, so
// the address picks among the broker's own sentences and writes none of its own.
func (p *Page) outcomes(ctx context.Context, decided, refused, login string, now time.Time, read requests.RecordDetail, readUnanswered bool) ([]string, error) {
	codes := make(map[string]string)
	for pair := range strings.SplitSeq(refused, ",") {
		id, code, ok := strings.Cut(pair, ":")
		if _, known := refusalSentences[code]; ok && known && record.ValidID(id) {
			codes[id] = code
		}
	}
	var lines []string
	var seen []string
	for id := range strings.SplitSeq(decided, ",") {
		if !record.ValidID(id) || slices.Contains(seen, id) {
			continue
		}
		if len(seen) == maxRecords {
			break
		}
		seen = append(seen, id)
		if id == read.RecordID && readUnanswered {
			lines = append(lines, unansweredOutcome(id))
			continue
		}
		d := read
		if id != read.RecordID {
			var err error
			d, err = p.cfg.Machine.ReadRecordFor(ctx, id, login)
			switch {
			case errors.Is(err, requests.ErrProjectAccessUnavailable):
				lines = append(lines, unansweredOutcome(id))
				continue
			case errors.Is(err, pgx.ErrNoRows):
				continue
			case err != nil:
				return nil, err
			}
		}
		lines = append(lines, outcome(d, login, now, codes[id]))
	}
	return lines, nil
}

// outcome says what became of a record a decision named, from its state now. refusal is the code
// of why this page's decision of it was refused (refusalSentences), empty when it was not.
//
// "already decided: " opens the line whenever somebody other than the reader decided the record,
// the model judge and the current policy included: the reader clicked Approve and got the
// broker's refusal, and what they must read first is that the record was already decided and by
// whom. A record nobody decided — one that expired, or that its requesting session withdrew —
// has no decider to compare, and its own line says what became of it.
func outcome(d requests.RecordDetail, login string, now time.Time, refusal string) string {
	what := strings.Join(d.Identifiers, ", ") + ": "
	switch {
	case d.Kind != record.KindAgentSecret:
		return what + "a machine login, which is decided by typing its code on Dispatch's machine-login page."
	case d.Decided == nil && !record.MayDecide(d.Kind, d.Approver, d.Service, login):
		return what + "not yours to decide; only " + approverText(d.Approver) + " may."
	case d.Decided == nil && !d.ExpiresAt.After(now):
		return what + "expired before anyone decided it."
	case d.Decided == nil && refusalSentences[refusal] != "":
		return what + refusalSentences[refusal]
	case d.Decided == nil:
		return what + "still pending; it was not decided."
	}
	by, _ := decidedBy(*d.Decided)
	at := d.Decided.At.UTC().Format(timeLayout)
	if by != "" && by != login {
		what += "already decided: "
	}
	switch d.Decided.Event {
	case "approved":
		if d.Decided.Detail != "" {
			return what + "recorded as denied because the requesting session ended (approved by " + by + " at " + at + ")."
		}
		if d.State == "revoked" {
			return what + "granted by " + by + " at " + at + ", and revoked since."
		}
		return what + "granted (approved by " + by + " at " + at + ")."
	case "denied":
		return what + "denied by " + by + " at " + at + "."
	case "expired":
		return what + "expired at " + at + " before anyone decided it."
	case "cancelled":
		return what + "cancelled at " + at + ": the requesting session withdrew it or ended."
	}
	return what + d.Decided.Event + " at " + at + "."
}

// pageMAC binds a page's decision forms to the session whose secret it is and to exactly the
// records printed with a button: HMAC-SHA256 under the raw session secret, which only the browser
// holding the cookie has (Postgres keeps its hash), over "approve-page", the expiry and the sorted
// record ids, each line-separated. Record ids are fixed-length hex, so the message is unambiguous.
func pageMAC(secret []byte, expires int64, printed []string) []byte {
	sorted := slices.Sorted(slices.Values(printed))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("approve-page\n" + strconv.FormatInt(expires, 10) + "\n" + strings.Join(sorted, "\n")))
	return mac.Sum(nil)
}

// pageToken is pageMAC hex-encoded, as a page's decision forms carry it.
func pageToken(secret []byte, expires int64, printed []string) string {
	return hex.EncodeToString(pageMAC(secret, expires, printed))
}

// recordIDs reads a form's record ids, in order and each once, refusing any that is not a record
// id and more than maxRecords.
func recordIDs(values []string) ([]string, bool) {
	var ids []string
	for _, id := range values {
		if !record.ValidID(id) {
			return nil, false
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, len(ids) <= maxRecords
}

// firstRecordID is the first of a form's record ids that is one, for a link the page puts in a
// URL, and "" when the form names none. recordIDs is the decision's own, stricter reading: this
// one only has to find a page to send a person back to.
func firstRecordID(values []string) string {
	for _, id := range values {
		if record.ValidID(id) {
			return id
		}
	}
	return ""
}

// ServeDecide is POST /approve: decide the records a page's form names. It refuses, writing
// nothing, a POST from any other origin (sameOrigin), one whose form cannot be read, and one
// whose token does not bind it to this session and to a printed set holding every record it
// names, or whose token has expired. A POST whose session has lapsed decides nothing either, and
// answers 200 with a page linking the first record the form names: the page's
// Content-Security-Policy has form-action 'self', which forbids answering a form post with a
// redirect to the sign-in provider, so the person follows that link and signs in from the
// record's own page. Each record is then decided on its own by requests.Machine.ApplyDecision as
// the session's person, so who may decide, the current policy and RECORD_TERMINAL stay that
// function's: a replayed form decides nothing again. It answers 303 to the first record's page
// with the decided ids, whose outcomes that page reads from the records themselves.
func (p *Page) ServeDecide(w http.ResponseWriter, r *http.Request, sess Session) {
	if !p.sameOrigin(r) {
		refuseOrigin(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeMessage(w, http.StatusBadRequest, codeFormUnreadable, say("Not a decision", "The form could not be read, so nothing was decided."))
		return
	}
	if !sess.live() {
		m := say("Not signed in", "Your sign-in to the secrets broker has ended, so nothing was decided.")
		if id := firstRecordID(r.PostForm["record"]); id != "" {
			m.Link = &link{Href: approvePrefix + id, Text: "Open the request again to sign in"}
		} else {
			m.Lines = append(m.Lines, "This form names no request, so open the request's page again from the Inbox to sign in.")
		}
		writeMessage(w, http.StatusOK, "", m)
		return
	}
	form := r.PostForm
	var approve bool
	switch form.Get("decision") {
	case "approve":
		approve = true
	case "deny":
	default:
		writeMessage(w, http.StatusBadRequest, codeDecisionInvalid, say("Not a decision", "A decision is approve or deny, so nothing was decided."))
		return
	}
	printed, printedOK := recordIDs(form["printed"])
	decide, decideOK := recordIDs(form["record"])
	expires, err := strconv.ParseInt(form.Get("expires"), 10, 64)
	token, tokenErr := hex.DecodeString(form.Get("token"))
	if !printedOK || !decideOK || len(decide) == 0 || err != nil || tokenErr != nil {
		writeMessage(w, http.StatusBadRequest, codeDecisionRecordsInvalid, say("Not a decision", "The form names no request it may decide, so nothing was decided."))
		return
	}
	switch {
	case !hmac.Equal(token, pageMAC(sess.secret, expires, printed)):
		writeMessage(w, http.StatusForbidden, codePageTokenInvalid, say("Refused",
			"This form was not made for your sign-in, so nothing was decided. Open the request again."))
		return
	case time.Now().Unix() >= expires:
		writeMessage(w, http.StatusForbidden, codePageExpired, say("Page expired",
			"This page is more than an hour old, so nothing was decided. Open the request again."))
		return
	}
	for _, id := range decide {
		if !slices.Contains(printed, id) {
			writeMessage(w, http.StatusForbidden, codeRecordNotPrinted, say("Refused",
				"This form names a request its page did not print, so nothing was decided. Open the request again."))
			return
		}
	}
	decided := false
	var refused []string
	for _, id := range decide {
		outcome, code, made := p.decide(r.Context(), id, approve, sess.Login)
		decided = decided || made
		if code != "" {
			refused = append(refused, id+":"+code)
		}
		slog.Info("approval page decision", "login", sess.Login, "record", id, "decision", form.Get("decision"),
			"outcome", outcome)
	}
	// One write for the POST, however many records it named, and none at all when it decided
	// none: what GET /signin/status answers is when a person last approved or denied something
	// on a page (status.go), and the rollout's step 3 opens on that answer. A form resent after
	// its records were decided, or one a person may not decide, must leave the time where it was.
	if decided {
		p.recordDecision(r.Context())
	}
	to := "/approve/" + decide[0] + "?decided=" + strings.Join(decide, ",")
	if len(refused) > 0 {
		to += "&refused=" + strings.Join(refused, ",")
	}
	redirect(w, http.StatusSeeOther, to)
}

// refusalSentences is what the page says of a record its decision could not decide, by the code
// decide answered for it: a fixed set, so the address that carries the code to the page it lands
// on picks among these sentences and writes none of its own. A record the decision left pending
// reads one of them in place of "still pending; it was not decided.". Two refusals need no code: a
// record a private project refused the person is hidden from them on the page they land on, as an
// id no record has (requests.Machine.ReadRecordFor), and one Dispatch could not answer for prints its
// own line (unansweredOutcome).
var refusalSentences = map[string]string{
	refusedPolicy: "still pending; it was not decided: the current policy no longer lets you approve it.",
	refusedChain:  "still pending; it was not decided: its approval chain no longer verifies.",
	refusedError:  "still pending; it was not decided: the secrets broker could not decide it. Try again shortly.",
}

// The codes refusalSentences maps, which decide answers.
const (
	refusedPolicy = "policy"
	refusedChain  = "chain"
	refusedError  = "error"
)

// decide decides one record as login and answers what became of it, for the log, the code of why
// the broker refused to decide it (refusalSentences), empty when it did not refuse or when the
// page it lands on says why already, and whether this POST decided it: an approval or a denial the
// broker applied now, which is what the sign-in status's last decision counts. A record already
// decided, expired, gone, or one login may not decide, is decided by nobody here. A machine login
// is never decided here, whatever a form says: its typed code is decided on Dispatch's page. An
// approval the broker recorded as a denial answers with that decision's own detail, so the person
// who pressed approve reads which secret refused it and why rather than one fixed sentence. A
// record Dispatch could not say whether login may see (requests.ErrProjectAccessUnavailable) is
// decided by nobody, and the POST's other records are decided as ever: the page it lands on says
// that one is unavailable. An approval for which Dispatch could not say whether the request's
// person is a restricted member (*projectaccess.MemberKindUncheckable) is decided by nobody
// either, and its page says it could not be decided; neither writes an ERROR line. A record of a
// private project login may not see is "no such record", as for an id no record has (the
// machine's refusal is pgx.ErrNoRows alone), so the POST tells them nothing of it. A refusal
// record.ErrNotApprover carries for a record whose approver line names someone else is said by the
// page it lands on in its own words ("not yours to decide"), and carries no code; one for a record
// login may still see and decide is the current policy's — its owner, a max-grant or a withheld
// name moved since the page was printed.
func (p *Page) decide(ctx context.Context, id string, approve bool, login string) (outcome, refusal string, made bool) {
	kind, err := p.cfg.Machine.RecordKind(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no such record", "", false
	case err != nil:
		slog.Error("approval page: read the record's kind", "record", id, "error", err)
		return "error", refusedError, false
	case kind != record.KindAgentSecret:
		return "not decided: a machine login", "", false
	}
	decision, err := p.cfg.Machine.ApplyDecision(ctx, id, approve, login)
	switch {
	case err == nil && decision.State == "denied" && approve && decision.Detail != "":
		return "denied: " + decision.Detail, "", true
	case err == nil:
		return decision.State, "", true
	case errors.Is(err, pgx.ErrNoRows):
		return "no such record", "", false
	case errors.Is(err, requests.ErrExpired):
		return "expired", "", false
	case errors.Is(err, requests.ErrTerminal):
		return "already decided", "", false
	case errors.Is(err, requests.ErrGrantChainInvalid):
		return "refused: " + err.Error(), refusedChain, false
	case errors.Is(err, record.ErrNotApprover):
		if d, read := p.cfg.Machine.ReadRecord(ctx, id); read == nil && !record.MayDecide(d.Kind, d.Approver, d.Service, login) {
			return "refused: " + err.Error(), "", false
		}
		return "refused: " + err.Error(), refusedPolicy, false
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		return "unavailable: " + err.Error(), "", false
	case errors.As(err, new(*projectaccess.MemberKindUncheckable)):
		// Dispatch could not say whether the request's person is a restricted member, which an
		// approval asks (requests.Machine.restrictedOperator): nothing was decided, and the
		// person approves again once Dispatch answers. Dispatch not answering is no fault of the
		// page's, so no ERROR line; the decision line logs the outcome.
		return "unavailable: " + err.Error(), refusedError, false
	case errors.Is(err, requests.ErrJudgeHolds):
		return "refused: the model judge is deciding this request", "", false
	}
	slog.Error("approval page: decide the record", "record", id, "error", err)
	return "error", refusedError, false
}
