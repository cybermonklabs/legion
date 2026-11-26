// packages/envoy/internal/cipher/helper/signinkeep.go
//go:build linux

// The launcher credential of the person signed in on this box, kept live while an enrollment this
// helper made or renews may stand under it.
//
// The broker renews a session enrolled under a sign-in only while that sign-in's credential is
// live (enroll.Service.Renew, ErrSignInCredentialEnded), and the credential expires with the
// person's last traded identity token, an hour after each trade. So the helper trades the sign-in
// again (keepSignIns) while something may stand under it, and not otherwise: each trade moves the
// sign-in's "last used" time on Dispatch's sign-ins page, which then says whether the box is in use.
//
// A box a launcher enrolled through this helper renews with its own key and tells the helper
// nothing, so the helper keeps a record of each it enrolled under a sign-in (SignInBoxesPath).
// Where it finds no record it can read, as at its first start after a release that kept none, it
// cannot tell which boxes stand under the sign-in on this box, so it holds that sign-in and trades
// it until it leaves the box (loadSignInHolds).

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// SignInBoxesNamed is how every message names the file SignInBoxesPath answers: by what it is,
// never by its path, as MachineFileNamed names the machine file.
const SignInBoxesNamed = "the record of the boxes this helper enrolled under a person's sign-in (beside the session records, their file name with .signin-boxes after it)"

// SignInBoxesPath is where a helper whose session records are at statePath keeps the enrollments
// it made under a person's sign-in for a launcher's key, and the sign-in it holds for want of such
// a record (Broker.SignInBoxesFile): the records' own path with ".signin-boxes" after it, so two
// helpers whose records sit in one directory, which AGENT_SECRETS_STATE_FILE allows, keep one each.
func SignInBoxesPath(statePath string) string {
	return statePath + ".signin-boxes"
}

// keepSignIns keeps the launcher credential of the person signed in on this box live while an
// enrollment may stand under it (signInNeeded), until ctx ends: each tick, every
// cipherclient.SignInAskEvery, it asks SignIns, which trades again once the access token it holds
// is due, five minutes before its end, or the credential, cipherclient.CredentialRenewBefore
// before its end, whichever comes first (cipherclient.Client.Token). So the first ask that trades
// for the credential finds more than two asks' worth of it left, and a trade that fails there is
// tried twice more before it lapses. With the broker's default access token of 15 minutes, a token
// is due 10 minutes after it was minted, a moment after the tick two asks after the one that
// traded, so the asks trade every 15 minutes, or 10 to 15 minutes after a trade an enrolment made
// between two asks.
//
// Nobody signed in is nil at once, and a sign-in refused for good — the person revoked it, or the
// issuer no longer honours their refresh token — is remembered by Current and trades no more, so
// its credential lapses and the sessions under it end: that is how a person who can no longer be
// traded loses this box. A trade that fails while the credential is due is said at WARN by the
// client (cipherclient.Client.Token), and a failure with no credential left in hand at WARN here;
// both are asked again at the next tick.
func (b *Broker) keepSignIns(ctx context.Context) {
	every := b.signInKeepEvery
	if every <= 0 {
		every = cipherclient.SignInAskEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !b.signInNeeded(time.Now()) {
			continue
		}
		if _, err := b.SignIns.Current(ctx); err != nil && ctx.Err() == nil {
			b.logger().Warn("the helper could not trade the sign-in on this box to keep its launcher credential live; it asks again at the next tick", "error", b.URL.Error(err))
		}
	}
}

// signInNeeded reports whether keepSignIns asks for the sign-in at now: an enrollment this helper
// recorded may stand under it (signInHolds.needed). A box and a sign-in held for want of a record
// each name the sign-in they stand under, by its box key's thumbprint, so it reads which sign-in
// is on the box (SignIns.SignedIn) while either is recorded, and drops each that names another:
// its key went with its record, so no later sign-in is it, and nothing here can trade it again.
// Where it cannot tell which sign-in is on the box (two people signed in and none named), it keeps
// every record and asks nothing for them, since Current could not trade one either.
//
// While the helper has not yet decided what a missing or unreadable record holds, it tries again
// first (decideSignInHolds).
func (b *Broker) signInNeeded(now time.Time) bool {
	if b.signInHolds.undecided() {
		b.decideSignInHolds(false)
	}
	here := ""
	if b.signInHolds.namesSignIns() {
		if _, thumbprint, err := b.SignIns.SignedIn(); err == nil {
			here = thumbprint
			if err := b.signInHolds.keepSignIn(thumbprint); err != nil {
				b.logger().Warn("the helper could not record that a sign-in its record named has left this box; a helper started again keeps it until it sees that again",
					"file", SignInBoxesNamed, "error", redact.File(err))
			}
		}
	}
	return b.signInHolds.needed(now, here)
}

// loadSignInHolds reads SignInBoxesFile, the record a helper before this one kept
// (signInHolds.load), and where there is none it can read, decides what the helper holds
// instead (decideSignInHolds).
func (b *Broker) loadSignInHolds() {
	if b.signInHolds.load(b.SignInBoxesFile) != nil {
		b.decideSignInHolds(true)
	}
}

// decideSignInHolds decides what a helper with no record of its boxes it can read holds
// (signInHolds.decide), and from then on saves its record. Where the record is not there and no
// helper ran on this machine before (FirstOnMachine), nothing was enrolled under a sign-in that
// the record would name, so it holds nothing. Otherwise it cannot tell which boxes a helper before
// it enrolled under the sign-in on this box: the release before kept no such record, and a lost
// record keeps none. A box it cannot see is refused its renewal an hour after the last trade of
// its sign-in, so it holds the sign-in on the box now, which keepSignIns then trades until that
// sign-in leaves the box, saves the hold so a restart keeps it, and says so at WARN, naming the
// person. With nobody signed in, it holds nothing: no box of a sign-in gone from the box can be
// kept by any trade here.
//
// Where it cannot tell which sign-in is on the box (two people signed in and none named, a record
// in the store that cannot be read), it decides nothing and saves no record, so neither a box
// enrolled meanwhile nor a restart writes a record that holds nothing: keepSignIns tries again at
// each tick, and a helper started again tries at its start. It says so at WARN at start alone.
func (b *Broker) decideSignInHolds(atStart bool) {
	loadErr := b.signInHolds.undecidedBecause()
	missing := errors.Is(loadErr, fs.ErrNotExist)
	person, thumbprint := "", ""
	if !missing || !b.FirstOnMachine {
		var err error
		if person, thumbprint, err = b.SignIns.SignedIn(); err != nil {
			if atStart {
				b.logger().Warn("the helper has no record it can read of the boxes it enrolled under a person's sign-in, and cannot tell which sign-in is on this box; it saves no record and asks again at each tick",
					"file", SignInBoxesNamed, "error", loadErr, "sign_in_error", err)
			}
			return
		}
	}
	saveErr := b.signInHolds.decide(thumbprint)
	switch {
	case thumbprint != "":
		message := "the helper found no record of the boxes it enrolled under a person's sign-in, so a box a helper before it enrolled under the sign-in on this box may stand that no record names; it keeps that sign-in traded until it leaves this box, by a sign-out or a new agent-secrets login"
		attrs := []any{"person", person, "file", SignInBoxesNamed}
		if !missing {
			message = "the helper could not read the record of the boxes it enrolled under a person's sign-in, so a box enrolled under the sign-in on this box may stand that no record names; it keeps that sign-in traded until it leaves this box, by a sign-out or a new agent-secrets login"
			attrs = append(attrs, "error", loadErr)
		}
		b.logger().Warn(message, attrs...)
	case !missing:
		b.logger().Warn("the helper could not read the record of the boxes it enrolled under a person's sign-in, and nobody is signed in on this box, so no trade here keeps any of them; it starts the record again",
			"file", SignInBoxesNamed, "error", loadErr)
	}
	if saveErr != nil {
		b.logger().Warn("the helper could not save the record of the boxes it enrols under a person's sign-in; a helper started again decides again what it holds",
			"file", SignInBoxesNamed, "error", redact.File(saveErr))
	}
}

// signInHolds is every enrollment that may stand under the sign-in's credential, which
// keepSignIns reads. The helper keeps no note at the broker of which credential enrolled a
// session, so it keeps this one itself.
type signInHolds struct {
	mu sync.Mutex
	// file is where boxes and unrecorded are kept across a restart (Broker.SignInBoxesFile, set
	// by load, or by decide); "" keeps them for this process's life alone.
	file string
	// pending is the record's path while what a missing or unreadable record holds is not yet
	// decided, and pendingErr why it was not read: nothing is saved until decide, so no record
	// that holds nothing is written in the meantime (Broker.decideSignInHolds).
	pending    string
	pendingErr error
	// sessions are the enrollments of the sessions this helper renews, each with the end of its
	// lease and whether this helper enrolled it under its own machine credential, which no trade
	// of a sign-in keeps. One it renews without having enrolled it, which a session record gave
	// back after a restart, may stand under the sign-in, and counts.
	sessions map[string]heldEnrollment
	// boxes are the enrollments this helper made under a sign-in for a launcher's key
	// (Broker.EnrollSessionKey), each with the thumbprint of the sign-in's box key it stands
	// under. Such a box renews with its own key and tells the helper nothing, so each counts
	// while its sign-in is the one on this box, until it is unenrolled through this helper; once
	// another sign-in is on the box, or none, no trade here can keep it, and it goes (keepSignIn).
	boxes map[string]string
	// unrecorded are the sign-ins, by their box keys' thumbprints, that were on this box when the
	// helper found no record it could read (Broker.loadSignInHolds): a box a helper before it
	// enrolled under one may stand that no record names. Each counts while it is the sign-in on
	// this box, and goes once another is, or none (keepSignIn).
	unrecorded map[string]bool
}

// heldEnrollment is what signInHolds knows of one session's enrollment.
type heldEnrollment struct {
	until   time.Time
	machine bool
}

// signInHoldsFile is the content of SignInBoxesPath's file.
type signInHoldsFile struct {
	// Boxes is signInHolds.boxes: each enrollment id, with its sign-in's thumbprint.
	Boxes map[string]string `json:"boxes"`
	// SignInsWithUnrecordedBoxes is signInHolds.unrecorded, by thumbprint.
	SignInsWithUnrecordedBoxes []string `json:"sign_ins_with_unrecorded_boxes"`
}

// needed reports whether an enrollment may stand under the sign-in at now: a session whose lease
// runs past now and that this helper did not enroll under its own machine credential; or, where
// here is the thumbprint of the sign-in on this box ("" where it was not read), a box enrolled
// under that sign-in, or that sign-in held for want of a record of its boxes.
func (h *signInHolds) needed(now time.Time, here string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if here != "" && h.unrecorded[here] {
		return true
	}
	for _, under := range h.boxes {
		if here != "" && under == here {
			return true
		}
	}
	for _, held := range h.sessions {
		if !held.machine && held.until.After(now) {
			return true
		}
	}
	return false
}

// namesSignIns reports whether a box, or a sign-in held for want of a record, is recorded: each
// names a sign-in, which needed counts only while it is the one on this box.
func (h *signInHolds) namesSignIns() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.boxes) > 0 || len(h.unrecorded) > 0
}

// keepSignIn drops every box and every hold that names a sign-in other than the one whose box
// key's thumbprint is here, the sign-in on this box ("" where there is none), and saves what it
// dropped.
func (h *signInHolds) keepSignIn(here string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	dropped := false
	for thumbprint := range h.unrecorded {
		if thumbprint != here {
			delete(h.unrecorded, thumbprint)
			dropped = true
		}
	}
	for id, under := range h.boxes {
		if under != here {
			delete(h.boxes, id)
			dropped = true
		}
	}
	if !dropped {
		return nil
	}
	return h.saveLocked()
}

// session records a session's enrollment this helper made, under a person's sign-in (signIn) or
// this machine's own credential, with the lease the broker gave it.
func (h *signInHolds) session(id string, until time.Time, signIn bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]heldEnrollment{}
	}
	h.sessions[id] = heldEnrollment{until: until, machine: !signIn}
}

// box records an enrollment this helper made for a launcher's key under the sign-in whose box
// key's thumbprint is under, and saves it. It answers the error of that save, which leaves the box
// counted until this helper stops.
func (h *signInHolds) box(id, under string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.boxes == nil {
		h.boxes = map[string]string{}
	}
	h.boxes[id] = under
	return h.saveLocked()
}

// renewed records that a session's enrollment renewed to until. One this helper did not enroll
// is recorded as one that may stand under the sign-in.
func (h *signInHolds) renewed(id string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[string]heldEnrollment{}
	}
	held := h.sessions[id]
	held.until = until
	h.sessions[id] = held
}

// ended forgets an enrollment the broker no longer holds, after a revoke this helper made, saving
// the boxes where it was one of them. It answers the error of that save, which leaves the box
// counted again after a restart, until the next save that lands.
func (h *signInHolds) ended(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
	if _, held := h.boxes[id]; !held {
		return nil
	}
	delete(h.boxes, id)
	return h.saveLocked()
}

// undecided reports whether what a missing or unreadable record holds is not yet decided.
func (h *signInHolds) undecided() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pending != ""
}

// undecidedBecause is why the record was not read, while that is not yet decided.
func (h *signInHolds) undecidedBecause() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pendingErr
}

// decide settles a record that was not read: the sign-in whose box key's thumbprint is thumbprint
// is held ("" holds none), and the record is saved from now on, at once with what it holds. It
// answers the error of that save.
func (h *signInHolds) decide(thumbprint string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.file, h.pending, h.pendingErr = h.pending, "", nil
	if thumbprint != "" {
		if h.unrecorded == nil {
			h.unrecorded = map[string]bool{}
		}
		h.unrecorded[thumbprint] = true
	}
	return h.saveLocked()
}

// saveLocked writes the boxes and the sign-ins held for want of a record to h.file, owner-only and
// atomically (writeFileOwnerOnly); nothing where h.file is "". The caller holds h.mu.
func (h *signInHolds) saveLocked() error {
	if h.file == "" {
		return nil
	}
	boxes := h.boxes
	if boxes == nil {
		boxes = map[string]string{}
	}
	data, err := json.Marshal(signInHoldsFile{Boxes: boxes, SignInsWithUnrecordedBoxes: sortedKeys(h.unrecorded)})
	if err != nil {
		return err
	}
	return writeFileOwnerOnly(h.file, data)
}

// sortedKeys is set's keys in order, an empty list for none.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// load reads file, kept by the helper that ran before this one, and keeps file for every later
// save. A file that is not there is an error wrapping fs.ErrNotExist, and one that cannot be read,
// or is no such record, an error naming it; either way nothing is added, file waits for decide,
// and the caller decides what the helper holds (Broker.decideSignInHolds).
func (h *signInHolds) load(file string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := os.ReadFile(file)
	if err != nil {
		h.pending, h.pendingErr = file, fmt.Errorf("read %s: %w", SignInBoxesNamed, redact.File(err))
		return h.pendingErr
	}
	var kept signInHoldsFile
	if err := json.Unmarshal(data, &kept); err != nil {
		h.pending, h.pendingErr = file, fmt.Errorf("%s is no record of boxes and sign-ins", SignInBoxesNamed)
		return h.pendingErr
	}
	h.file = file
	if h.boxes == nil {
		h.boxes = map[string]string{}
	}
	for id, under := range kept.Boxes {
		if under != "" {
			h.boxes[id] = under
		}
	}
	if h.unrecorded == nil {
		h.unrecorded = map[string]bool{}
	}
	for _, thumbprint := range kept.SignInsWithUnrecordedBoxes {
		if thumbprint != "" {
			h.unrecorded[thumbprint] = true
		}
	}
	return nil
}
