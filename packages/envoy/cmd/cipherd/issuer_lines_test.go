// issuer_lines_test.go pins the lines the broker writes about a cluster issuer whose tokens it
// cannot verify for want of a read (loggedIssuers): one when its tokens start to be answered 503,
// however many arrive, and one when it can be read again, written once by whichever path finds it
// first, a token of it that verifies or the background read of an issuer the boot could not read.
package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// logLines is the lines of logged, a buffer captureLog fills, that carry message, as captureLog's
// text handler writes them.
func logLines(logged *bytes.Buffer, message string) []string {
	var found []string
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, `msg="`+message+`"`) {
			found = append(found, line)
		}
	}
	return found
}

// TestAnIssuerTheBrokerCannotReadIsLoggedOnceAndOnceWhenReadAgain: while B's key set cannot be
// read, a burst of twenty tokens of B, sent at once and each answered ErrIssuerUnavailable, writes
// one line naming B and the key set's error; a token of A, which verifies, writes none; once B's
// key set answers, the first token of B that verifies writes one line, and the tokens after it
// none. A token of C whose caller hung up while C's discovery document could not be read writes
// no line.
func TestAnIssuerTheBrokerCannotReadIsLoggedOnceAndOnceWhenReadAgain(t *testing.T) {
	logged := captureLog(t)
	a, b, c := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	aKey, bKey, cKey := a.PublishKey(t, "a-key"), b.PublishKey(t, "b-key"), c.PublishKey(t, "c-key")
	issuers := []oidc.NamedIssuer{{Name: "a", URL: a.URL()}, {Name: "b", URL: b.URL()}, {Name: "c", URL: c.URL()}}
	// No spacing: every token of B reads B's key set, so the one line of a burst is the
	// logger's doing and not the spacing's.
	verifier := newLoggedIssuers(oidc.NewIssuerSet(issuers, enrolmentAudience, 0), issuers)
	token := func(issuer *oidctest.Issuer, key *oidctest.Key) string {
		return issuer.Mint(t, key, issuer.Claims("system:serviceaccount:example-ns:example-worker", enrolmentAudience))
	}
	// lines is the log's lines carrying message.
	lines := func(message string) []string { return logLines(logged, message) }
	b.FailKeys(http.StatusServiceUnavailable)

	t.Run("a burst of tokens of an issuer whose key set cannot be read writes one line", func(t *testing.T) {
		const burst = 20
		errs := make([]error, burst)
		var wg sync.WaitGroup
		for i := range burst {
			wg.Go(func() { _, _, errs[i] = verifier.VerifyClaims(context.Background(), token(b, bKey)) })
		}
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, oidc.ErrIssuerUnavailable) {
				t.Fatalf("token %d of B = %v, want oidc.ErrIssuerUnavailable", i, err)
			}
		}
		found := lines(issuerUnreadableMessage)
		if len(found) != 1 {
			t.Fatalf("%d tokens of B wrote %d %q lines, want one:\n%s", burst, len(found), issuerUnreadableMessage, logged)
		}
		for _, want := range []string{"level=WARN", "name=b", "url=" + b.URL(), "key set could not be read"} {
			if !strings.Contains(found[0], want) {
				t.Fatalf("the line %q does not carry %q", found[0], want)
			}
		}
	})

	t.Run("a token of an issuer that can be read writes no line", func(t *testing.T) {
		if _, _, err := verifier.VerifyClaims(context.Background(), token(a, aKey)); err != nil {
			t.Fatalf("the token of A = %v, want it verified", err)
		}
		if found := lines(issuerUnreadableMessage); len(found) != 1 || strings.Contains(found[0], "name=a") {
			t.Fatalf("a token of A changed the %q lines to %v, want B's one", issuerUnreadableMessage, found)
		}
		if found := lines(issuerReadableMessage); len(found) != 0 {
			t.Fatalf("a token of A wrote %v, want no %q line", found, issuerReadableMessage)
		}
	})

	t.Run("the first token of the issuer that verifies again writes one line", func(t *testing.T) {
		b.FailKeys(0)
		for i := range 3 {
			if _, _, err := verifier.VerifyClaims(context.Background(), token(b, bKey)); err != nil {
				t.Fatalf("token %d of B once its key set answers = %v, want it verified", i, err)
			}
		}
		found := lines(issuerReadableMessage)
		if len(found) != 1 {
			t.Fatalf("three tokens of B wrote %d %q lines, want one:\n%s", len(found), issuerReadableMessage, logged)
		}
		for _, want := range []string{"level=INFO", "name=b", "url=" + b.URL()} {
			if !strings.Contains(found[0], want) {
				t.Fatalf("the line %q does not carry %q", found[0], want)
			}
		}
		if found := lines(issuerUnreadableMessage); len(found) != 1 {
			t.Fatalf("the %q lines are now %v, want the one the burst wrote", issuerUnreadableMessage, found)
		}
	})

	t.Run("a token whose caller hung up writes no line", func(t *testing.T) {
		c.FailDiscovery(http.StatusServiceUnavailable)
		hungUp, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := verifier.VerifyClaims(hungUp, token(c, cKey)); !errors.Is(err, oidc.ErrIssuerUnavailable) {
			t.Fatalf("a token of C on an ended request, C's discovery document failing = %v, want oidc.ErrIssuerUnavailable", err)
		}
		if found := lines(issuerUnreadableMessage); len(found) != 1 || !strings.Contains(found[0], "name=b") {
			t.Fatalf("the token of C changed the %q lines to %v, want B's one: a caller that hung up says nothing of the issuer", issuerUnreadableMessage, found)
		}
	})
}

// TestAnIssuerTheBootCouldNotReadWritesOneLineWhenRead: B's discovery document fails at boot, so
// the boot's line says B was not read, and a token of B, answered ErrIssuerUnavailable, writes no
// line more. Once B answers, tokens of B are sent from four callers while the background read of
// B succeeds too, and the two paths write one issuerReadableMessage line naming B between them.
func TestAnIssuerTheBootCouldNotReadWritesOneLineWhenRead(t *testing.T) {
	logged := captureLog(t)
	a, b := oidctest.New(t), oidctest.New(t)
	a.PublishKey(t, "a-key")
	bKey := b.PublishKey(t, "b-key")
	issuers := []oidc.NamedIssuer{{Name: "a", URL: a.URL()}, {Name: "b", URL: b.URL()}}
	verifier := newLoggedIssuers(oidc.NewIssuerSet(issuers, enrolmentAudience, 0), issuers)
	tokenOfB := b.Mint(t, bKey, b.Claims("system:serviceaccount:example-ns:example-worker", enrolmentAudience))
	lines := func(message string) []string { return logLines(logged, message) }

	b.FailDiscovery(http.StatusServiceUnavailable)
	verifier.boot(context.Background())
	booted := lines(issuerBootMessage)
	if len(booted) != 2 || !strings.Contains(booted[1], "name=b") || !strings.Contains(booted[1], "read=false") {
		t.Fatalf("the boot wrote %v, want one line for each issuer, B's saying it was not read:\n%s", booted, logged)
	}
	if _, _, err := verifier.VerifyClaims(context.Background(), tokenOfB); !errors.Is(err, oidc.ErrIssuerUnavailable) {
		t.Fatalf("a token of B while B's discovery document fails = %v, want oidc.ErrIssuerUnavailable", err)
	}
	if found := lines(issuerUnreadableMessage); len(found) != 0 {
		t.Fatalf("a token of B after the boot's line said B was not read wrote %v, want no %q line", found, issuerUnreadableMessage)
	}

	b.FailDiscovery(0)
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		verifier.run(t.Context())
	}()
	var callers sync.WaitGroup
	for range 4 {
		callers.Go(func() {
			for {
				_, _, _ = verifier.VerifyClaims(context.Background(), tokenOfB)
				select {
				case <-ran:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}
	select {
	case <-ran:
	case <-time.After(time.Minute):
		t.Fatal("the background read of B did not succeed within a minute of B answering")
	}
	callers.Wait()
	if _, _, err := verifier.VerifyClaims(context.Background(), tokenOfB); err != nil {
		t.Fatalf("a token of B once B is read = %v, want it verified", err)
	}
	found := lines(issuerReadableMessage)
	if len(found) != 1 {
		t.Fatalf("the tokens of B and the background read of B wrote %d %q lines, want one:\n%s", len(found), issuerReadableMessage, logged)
	}
	for _, want := range []string{"level=INFO", "name=b", "url=" + b.URL()} {
		if !strings.Contains(found[0], want) {
			t.Fatalf("the line %q does not carry %q", found[0], want)
		}
	}
	if found := lines(issuerUnreadableMessage); len(found) != 0 {
		t.Fatalf("B's recovery wrote %v, want no %q line", found, issuerUnreadableMessage)
	}
}
