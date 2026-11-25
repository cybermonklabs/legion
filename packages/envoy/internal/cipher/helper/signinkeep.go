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

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
)

// defaultSignInKeepEvery is how often keepSignIns asks for the sign-in's credential. The client
// trades again once the access token it holds is due, five minutes before its end, or the
// credential, fifteen minutes before its end, whichever comes first (cipherclient.Client.Token),
// so the first ask that trades for the credential finds more than two asks' worth of it left, and
// a trade that fails there is tried twice more before it lapses. With the broker's default access
// token of 15 minutes, a token is due 10 minutes after it was minted, a moment after the tick two
// asks after the one that traded, so the asks trade every 15 minutes, or 10 to 15 minutes after a
// trade an enrolment made between two asks.
const defaultSignInKeepEvery = 5 * time.Minute

// SignInBoxesNamed is how every message names the file SignInBoxesPath answers: by what it is,
// never by its path, as MachineFileNamed names the machine file.
const SignInBoxesNamed = "the record of the boxes this helper enrolled under a person's sign-in (signin-boxes.json, beside the session records)"

// SignInBoxesPath is where a helper whose session records are at statePath keeps the enrollments
// it made under a person's sign-in for a launcher's key (Broker.SignInBoxesFile).
func SignInBoxesPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "signin-boxes.json")
}

// keepSignIns keeps the launcher credential of the person signed in on this box live while an
// enrollment may stand under it (signInHolds.needed), until ctx ends: each tick it asks SignIns,
// which trades again once the credential is due. Nobody signed in is nil at once, and a sign-in
// refused for good — the person revoked it, or the issuer no longer honours their refresh token —
// is remembered by Current and trades no more, so its credential lapses and the sessions under it
// end: that is how a person who can no longer be traded loses this box. A trade that fails while
// the credential is due is said at WARN by the client (cipherclient.Client.Token), and a failure
// with no credential left in hand at WARN here; both are asked again at the next tick.
func (b *Broker) keepSignIns(ctx context.Context) {
	every := b.signInKeepEvery
	if every <= 0 {
		every = defaultSignInKeepEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !b.signInHolds.needed(time.Now()) {
			continue
		}
		if _, err := b.SignIns.Current(ctx); err != nil && ctx.Err() == nil {
			b.logger().Warn("the helper could not trade the sign-in on this box to keep its launcher credential live; it asks again at the next tick", "error", b.URL.Error(err))
		}
	}
}

// signInHolds is every enrollment that may stand under the sign-in's credential, which
// keepSignIns reads. The helper keeps no note at the broker of which credential enrolled a
// session, so it keeps this one itself.
type signInHolds struct {
	mu sync.Mutex
	// sessions are the enrollments of the sessions this helper renews, each with the end of its
	// lease and whether this helper enrolled it under its own machine credential, which no trade
	// of a sign-in keeps. One it renews without having enrolled it, which a session record gave
	// back after a restart, may stand under the sign-in, and counts.
	sessions map[string]heldEnrollment
	// boxes are the enrollments this helper made under a sign-in for a launcher's key
	// (Broker.EnrollSessionKey). Such a box renews with its own key and tells the helper nothing,
	// so each counts until it is unenrolled through this helper; Broker.SignInBoxesFile keeps them
	// across a restart.
	boxes map[string]bool
}

// heldEnrollment is what signInHolds knows of one session's enrollment.
type heldEnrollment struct {
	until   time.Time
	machine bool
}

// needed reports whether an enrollment may stand under the sign-in at now: a box, or a session
// whose lease runs past now and that this helper did not enroll under its own machine credential.
func (h *signInHolds) needed(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.boxes) > 0 {
		return true
	}
	for _, held := range h.sessions {
		if !held.machine && held.until.After(now) {
			return true
		}
	}
	return false
}

// enrolled records an enrollment this helper made: for a launcher's key (box) or a session of its
// own, under a sign-in's credential or this machine's own (machine), with the lease the broker
// gave it. A box under this machine's credential stands under no sign-in and is not recorded; one
// under the sign-in is saved to file. It answers the error of that save, which leaves the box
// counted until this helper stops.
func (h *signInHolds) enrolled(id string, until time.Time, box, machine bool, file string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !box {
		if h.sessions == nil {
			h.sessions = map[string]heldEnrollment{}
		}
		h.sessions[id] = heldEnrollment{until: until, machine: machine}
		return nil
	}
	if machine {
		return nil
	}
	if h.boxes == nil {
		h.boxes = map[string]bool{}
	}
	h.boxes[id] = true
	return h.saveLocked(file)
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
// the boxes to file where it was one of them. It answers the error of that save, which leaves the
// box counted again after a restart, until the next save that lands.
func (h *signInHolds) ended(id, file string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
	if !h.boxes[id] {
		return nil
	}
	delete(h.boxes, id)
	return h.saveLocked(file)
}

// saveLocked writes the boxes to file, owner-only and atomically (writeFileOwnerOnly); nothing
// where file is "". The caller holds h.mu.
func (h *signInHolds) saveLocked(file string) error {
	if file == "" {
		return nil
	}
	ids := make([]string, 0, len(h.boxes))
	for id := range h.boxes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return writeFileOwnerOnly(file, data)
}

// load reads file's boxes, kept by the helper that ran before this one: a file that is not there
// holds none. One that cannot be read is an error naming it, and the boxes it held are not kept,
// so each lapses an hour after the last trade of its sign-in unless a session of this helper's
// keeps the sign-in traded.
func (h *signInHolds) load(file string) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", SignInBoxesNamed, redact.File(err))
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return fmt.Errorf("%s is no JSON list of enrollment ids", SignInBoxesNamed)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.boxes == nil {
		h.boxes = map[string]bool{}
	}
	for _, id := range ids {
		h.boxes[id] = true
	}
	return nil
}
