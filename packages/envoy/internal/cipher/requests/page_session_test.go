package requests

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// pageIssuer is the identity provider of every approval-page session these tests sign in.
const pageIssuer = "https://accounts.google.com"

// ApplyDecision decides recordID as login on the broker's own approval page, the way a person
// signed in there does: it signs login in to the page first (signInToThePage) and decides under
// that session (ApplyPageDecision), so a test that decides as a person goes through the session's
// hold as every decision of the page does. The broker itself has no such method: a person decides
// only under a session the page read.
func (m *Machine) ApplyDecision(ctx context.Context, recordID string, approve bool, login string) (Decision, error) {
	session, err := m.signInToThePage(ctx, login)
	if err != nil {
		return Decision{}, err
	}
	return m.ApplyPageDecision(ctx, recordID, approve, session)
}

// signInToThePage writes an approval-page session of login, signed in by its own account of
// pageIssuer, live for an hour, as the page's sign-in callback does (store.Store.CreateBrowserSession),
// and answers it as a decision names it.
func (m *Machine) signInToThePage(ctx context.Context, login string) (store.BrowserSessionRef, error) {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails.
	session := store.BrowserSessionRef{Hash: sha256.Sum256(secret), Login: login}
	return session, m.Store.CreateBrowserSession(ctx, store.BrowserSignIn{
		Hash: session.Hash, Login: login, Account: pageAccount(login), Expires: time.Now().Add(time.Hour),
	})
}

// pageAccount is the account signInToThePage signs login in by.
func pageAccount(login string) store.IssuerSubject {
	return store.IssuerSubject{Issuer: pageIssuer, Subject: "sub-" + login}
}

// BenchmarkPageDecisionsUnderThePersonLock measures what the person's lock a page decision takes
// (store.HoldBrowserSessionTx) costs in contention. Each iteration opens decisions pending
// requests for the shared human-tier SHARED_KEY, each from an enrollment of its own, and decides
// them all at once, approving: under one person's sessions, whose decisions wait for each other on
// that person's lock, or under the sessions of as many different persons, whose do not. Each of
// the two runs alone and beside a subject revoke of the (first) person that starts with the
// decisions (enroll.Service.RevokePersonSubject), which takes the same lock; it names an account
// none of the sessions signed in by, so it ends nothing and every decision still decides. Each run
// reports the decisions' latency percentiles in milliseconds, and the revoke's where one runs; its
// ns/op counts each iteration's setup as well and is not the figure.
//
// Run it with -run '^$' -bench PageDecisionsUnderThePersonLock -benchtime 20x. For the same
// figures without the lock, take LockPerson out of store.HoldBrowserSessionTx and run it again.
func BenchmarkPageDecisionsUnderThePersonLock(b *testing.B) {
	// The grant log's line for every request and decision would split the benchmark's result
	// lines; warnings and errors still reach stderr.
	logtest.SetDefault(b, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	const decisions = 16
	for _, persons := range []int{1, decisions} {
		for _, revoke := range []bool{false, true} {
			name := fmt.Sprintf("persons=%d", persons)
			if revoke {
				name += "/revoke"
			}
			b.Run(name, func(b *testing.B) {
				benchmarkPageDecisions(b, decisions, persons, revoke)
			})
		}
	}
}

func benchmarkPageDecisions(b *testing.B, decisions, persons int, revoke bool) {
	m, _, _, _ := newFixture(b)
	ctx := context.Background()
	revoker := &enroll.Service{Store: m.Store}
	login := func(k int) string { return fmt.Sprintf("person-%d@example.com", k%persons) }
	var mu sync.Mutex
	var latencies, revokes []time.Duration
	iteration := 0
	for b.Loop() {
		iteration++
		records := make([]string, decisions)
		sessions := make([]store.BrowserSessionRef, decisions)
		for k := range decisions {
			enrollmentID, key := newEnrollment(b, m.Store, "session", fmt.Sprintf("bench-%d-%d", iteration, k), new(fixtureOperator), nil)
			req, err := m.Create(ctx, enrollmentID, signRequest(b, m, key, "measure the lock", "SHARED_KEY"), "")
			if err != nil || req.State != "pending" || req.RecordID == nil {
				b.Fatalf("Create = %+v, %v; want a pending request with a record", req, err)
			}
			records[k] = *req.RecordID
			if sessions[k], err = m.signInToThePage(ctx, login(k)); err != nil {
				b.Fatalf("sign %s in to the page: %v", login(k), err)
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for k := range decisions {
			wg.Go(func() {
				<-start
				began := time.Now()
				decision, err := m.ApplyPageDecision(ctx, records[k], true, sessions[k])
				took := time.Since(began)
				if err != nil || decision.State != "granted" {
					b.Errorf("the decision of %s under %s = %+v, %v; want granted", records[k], login(k), decision, err)
					return
				}
				mu.Lock()
				latencies = append(latencies, took)
				mu.Unlock()
			})
		}
		if revoke {
			wg.Go(func() {
				<-start
				began := time.Now()
				_, err := revoker.RevokePersonSubject(ctx, login(0), pageIssuer, fmt.Sprintf("bench-replaced-%d", iteration), pageAccount(login(0)).Subject, 1)
				took := time.Since(began)
				if err != nil {
					b.Errorf("the revoke of %s: %v", login(0), err)
					return
				}
				mu.Lock()
				revokes = append(revokes, took)
				mu.Unlock()
			})
		}
		close(start)
		wg.Wait()
	}
	reportPercentiles(b, "decision", latencies)
	if revoke {
		reportPercentiles(b, "revoke", revokes)
	}
}

// reportPercentiles reports the 50th, 99th and 100th percentiles of what, in milliseconds.
func reportPercentiles(b *testing.B, what string, latencies []time.Duration) {
	if len(latencies) == 0 {
		b.Errorf("no %s latency was measured", what)
		return
	}
	slices.Sort(latencies)
	for _, percentile := range []int{50, 99, 100} {
		at := min(len(latencies)*percentile/100, len(latencies)-1)
		b.ReportMetric(float64(latencies[at])/float64(time.Millisecond), fmt.Sprintf("%s-p%d-ms", what, percentile))
	}
}
