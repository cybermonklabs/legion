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
// recorded may stand under it, or it is a sign-in the helper holds for want of a record of its
// boxes (signInHolds.needed). It reads which sign-in is on the box (SignIns.SignedIn) only while
// such a hold stands, and drops a hold whose sign-in is no longer the one on the box: its key went
// with its record, so no later sign-in is it, and nothing here can trade it again. Where it cannot
// tell which sign-in is on the box (two people signed in and none named), it keeps every hold and
// asks nothing for them, since Current could not trade one either.
func (b *Broker) signInNeeded(now time.Time) bool {
	here := ""
	if b.signInHolds.holdsUnrecorded() {
		if _, thumbprint, err := b.SignIns.SignedIn(); err == nil {
			here = thumbprint
			if err := b.signInHolds.keepUnrecorded(thumbprint); err != nil {
				b.logger().Warn("the helper could not record that a sign-in it held for want of a record of its boxes has left this box; a helper started again holds it until it sees that again",
					"file", SignInBoxesNamed, "error", redact.File(err))
			}
		}
	}
	return b.signInHolds.needed(now, here)
}

// loadSignInHolds reads SignInBoxesFile, the record a helper before this one kept
// (signInHolds.load). Where there is none, or none it can read, the helper cannot tell which
// boxes a helper before it enrolled under the sign-in on this box: the release before kept no
// such record, and a lost record keeps none. A box it cannot see is refused its renewal an hour
// after the last trade of its sign-in. So it holds the sign-in on the box now
// (signInHolds.holdUnrecorded), which keepSignIns then trades until that sign-in leaves the box,
// saves the hold so a restart keeps it, and says so at WARN, naming the person. With nobody signed
// in, it saves an empty record: no box of a sign-in gone from the box can be kept by any trade
// here. Where it cannot tell which sign-in is on the box, it holds and saves nothing, so its next
// start tells again.
func (b *Broker) loadSignInHolds() {
	err := b.signInHolds.load(b.SignInBoxesFile)
	if err == nil {
		return
	}
	missing := errors.Is(err, fs.ErrNotExist)
	person, thumbprint, signedInErr := b.SignIns.SignedIn()
	switch {
	case signedInErr != nil:
		b.logger().Warn("the helper has no record it can read of the boxes it enrolled under a person's sign-in, and cannot tell which sign-in is on this box; a box enrolled under one before this start is not kept renewed",
			"file", SignInBoxesNamed, "error", err, "sign_in_error", signedInErr)
		return
	case thumbprint == "":
		if saveErr := b.signInHolds.save(); saveErr != nil {
			b.logger().Warn("the helper could not start the record of the boxes it enrols under a person's sign-in; a helper started again cannot tell a lost record from none",
				"file", SignInBoxesNamed, "error", redact.File(saveErr))
		}
		if !missing {
			b.logger().Warn("the helper could not read the record of the boxes it enrolled under a person's sign-in, and nobody is signed in on this box, so no trade here keeps any of them; it starts the record again",
				"file", SignInBoxesNamed, "error", err)
		}
		return
	}
	saveErr := b.signInHolds.holdUnrecorded(thumbprint)
	message := "the helper found no record of the boxes it enrolled under a person's sign-in, so a box a helper before it enrolled under the sign-in on this box may stand that no record names; it keeps that sign-in traded until it leaves this box, by a sign-out or a new agent-secrets login"
	attrs := []any{"person", person, "file", SignInBoxesNamed}
	if !missing {
		message = "the helper could not read the record of the boxes it enrolled under a person's sign-in, so a box enrolled under the sign-in on this box may stand that no record names; it keeps that sign-in traded until it leaves this box, by a sign-out or a new agent-secrets login"
		attrs = append(attrs, "error", err)
	}
	b.logger().Warn(message, attrs...)
	if saveErr != nil {
		b.logger().Warn("the helper could not save the sign-in it holds for want of a record of its boxes; a helper started again holds the sign-in on this box then",
			"file", SignInBoxesNamed, "error", redact.File(saveErr))
	}
}

// signInHolds is every enrollment that may stand under the sign-in's credential, which
// keepSignIns reads. The helper keeps no note at the broker of which credential enrolled a
// session, so it keeps this one itself.
type signInHolds struct {
	mu sync.Mutex
	// file is where boxes and unrecorded are kept across a restart (Broker.SignInBoxesFile, set
	// by load); "" keeps them for this process's life alone.
	file string
	// sessions are the enrollments of the sessions this helper renews, each with the end of its
	// lease and whether this helper enrolled it under its own machine credential, which no trade
	// of a sign-in keeps. One it renews without having enrolled it, which a session record gave
	// back after a restart, may stand under the sign-in, and counts.
	sessions map[string]heldEnrollment
	// boxes are the enrollments this helper made under a sign-in for a launcher's key
	// (Broker.EnrollSessionKey). Such a box renews with its own key and tells the helper nothing,
	// so each counts until it is unenrolled through this helper.
	boxes map[string]bool
	// unrecorded are the sign-ins, by their box keys' thumbprints, that were on this box when the
	// helper found no record it could read (Broker.loadSignInHolds): a box a helper before it
	// enrolled under one may stand that no record names. Each counts while it is the sign-in on
	// this box, and goes once another is, or none (keepUnrecorded).
	unrecorded map[string]bool
}

// heldEnrollment is what signInHolds knows of one session's enrollment.
type heldEnrollment struct {
	until   time.Time
	machine bool
}

// signInHoldsFile is the content of SignInBoxesPath's file.
type signInHoldsFile struct {
	// Boxes is signInHolds.boxes, by enrollment id.
	Boxes []string `json:"boxes"`
	// SignInsWithUnrecordedBoxes is signInHolds.unrecorded, by thumbprint.
	SignInsWithUnrecordedBoxes []string `json:"sign_ins_with_unrecorded_boxes"`
}

// needed reports whether an enrollment may stand under the sign-in at now: a box; a session whose
// lease runs past now and that this helper did not enroll under its own machine credential; or
// the sign-in on this box, whose box key's thumbprint is here ("" where it was not read), held for
// want of a record of its boxes.
func (h *signInHolds) needed(now time.Time, here string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.boxes) > 0 || (here != "" && h.unrecorded[here]) {
		return true
	}
	for _, held := range h.sessions {
		if !held.machine && held.until.After(now) {
			return true
		}
	}
	return false
}

// holdsUnrecorded reports whether a sign-in is held for want of a record of its boxes.
func (h *signInHolds) holdsUnrecorded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.unrecorded) > 0
}

// holdUnrecorded holds the sign-in whose box key's thumbprint is thumbprint for want of a record
// of its boxes, and saves it. It answers the error of that save, which leaves the hold for this
// process's life alone.
func (h *signInHolds) holdUnrecorded(thumbprint string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unrecorded == nil {
		h.unrecorded = map[string]bool{}
	}
	h.unrecorded[thumbprint] = true
	return h.saveLocked()
}

// keepUnrecorded drops every sign-in held for want of a record but the one whose box key's
// thumbprint is here, the sign-in on this box ("" where there is none), and saves what it dropped.
func (h *signInHolds) keepUnrecorded(here string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	dropped := false
	for thumbprint := range h.unrecorded {
		if thumbprint != here {
			delete(h.unrecorded, thumbprint)
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

// box records an enrollment this helper made under a person's sign-in for a launcher's key, and
// saves it. It answers the error of that save, which leaves the box counted until this helper
// stops.
func (h *signInHolds) box(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.boxes == nil {
		h.boxes = map[string]bool{}
	}
	h.boxes[id] = true
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
	if !h.boxes[id] {
		return nil
	}
	delete(h.boxes, id)
	return h.saveLocked()
}

// save writes the record as it stands (saveLocked).
func (h *signInHolds) save() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.saveLocked()
}

// saveLocked writes the boxes and the sign-ins held for want of a record to h.file, owner-only and
// atomically (writeFileOwnerOnly); nothing where h.file is "". The caller holds h.mu.
func (h *signInHolds) saveLocked() error {
	if h.file == "" {
		return nil
	}
	data, err := json.Marshal(signInHoldsFile{Boxes: sortedKeys(h.boxes), SignInsWithUnrecordedBoxes: sortedKeys(h.unrecorded)})
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
// or is no such record, an error naming it; either way nothing is added, and the caller decides
// what the helper holds (Broker.loadSignInHolds).
func (h *signInHolds) load(file string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.file = file
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", SignInBoxesNamed, redact.File(err))
	}
	var kept signInHoldsFile
	if err := json.Unmarshal(data, &kept); err != nil {
		return fmt.Errorf("%s is no record of boxes and sign-ins", SignInBoxesNamed)
	}
	if h.boxes == nil {
		h.boxes = map[string]bool{}
	}
	for _, id := range kept.Boxes {
		h.boxes[id] = true
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
