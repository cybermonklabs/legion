package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestACredentialRecordingNoAccountIsHeldToWhenTheAddressAccountWasRecorded: a credential that
// records no Google account is held to when it was made, against when Dispatch recorded the
// address's account (people.sub_recorded_at). The reviewer's R13-1: account A held carol's address
// at the broker and never at Dispatch, and its credentials record no account (a machine login from
// before the broker recorded accounts, a personal token that login minted); then B's first sign-in
// records B. Each row:
//
//   - with the address's account recorded but no recording time (every row before the column), no
//     credential is refused: the deploy resets nothing;
//   - once the recording time is set, A's personal token made before it is refused at use, at an
//     approve's re-read and as a minter, and A's broker credential made before it is refused at its
//     admission (401 BROKER_TOKEN_RECORDS_NO_ACCOUNT), as is one whose creation time the broker
//     did not record; a broker credential mints and approves nothing anyway (brokerPersonScopes);
//   - at the boundary the broker credential's time crosses two clocks, so one made 1 minute after
//     the recording is refused and one made 10 minutes after acts (brokerCredentialMargin);
//   - a personal token made after the recording (Dispatch's own clock) acts;
//   - a personal token recording no account made after the recording mints a token that records
//     the address's account, so the no-account state stops there.
//
// Control: tokenAccountReplaced and brokerAccountReplaced without the time arm (a no-account
// credential never replaced); A's credentials made before the recording act.
func TestACredentialRecordingNoAccountIsHeldToWhenTheAddressAccountWasRecorded(t *testing.T) {
	rig := newOutsideBrokerRig(t)
	deps := rig.offDeps
	deps.SignInIssuer = "https://accounts.google.com"
	served := directServer(deps)
	const person = "carol@example.com"
	ctx := context.Background()
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := rig.database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var recordedAt time.Time
	if err := rig.database.Pool.QueryRow(ctx, `select now() - interval '20 minutes'`).Scan(&recordedAt); err != nil {
		t.Fatal(err)
	}
	personalToken := func(name string, createdAt time.Time) (plaintext, id string) {
		t.Helper()
		plaintext, hash, err := newAgentToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.database.Pool.QueryRow(ctx, `
			insert into agent_tokens (owner, name, token_hash, prefix, scopes, created_at)
			values ($1, $2, $3, $4, $5, $6) returning id::text`, person, name, hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength],
			[]string{model.ScopePerson, model.ScopeApprove}, createdAt).Scan(&id); err != nil {
			t.Fatalf("store the personal token %q: %v", name, err)
		}
		return plaintext, id
	}
	broker := func(createdAt time.Time) ciphertoken.Caller {
		return ciphertoken.Caller{Person: person, CredentialCreatedAt: createdAt}
	}
	before := recordedAt.Add(-time.Hour)
	beforeToken, beforeID := personalToken("made before the recording", before)
	afterToken, afterID := personalToken("made after the recording", recordedAt.Add(time.Minute))
	tokenActs := func(when, plaintext string, want bool) {
		t.Helper()
		_, err := served.personalTokenActor(ctx, plaintext)
		switch {
		case want && err != nil:
			t.Errorf("%s: %v, want it to act", when, err)
		case !want:
			wantAPIError(t, err, http.StatusUnauthorized, "TOKEN_REVOKED", when)
		}
	}
	brokerActs := func(when string, caller ciphertoken.Caller, want bool) {
		t.Helper()
		_, err := served.brokerPersonAdmitted(ctx, caller)
		switch {
		case want && err != nil:
			t.Errorf("%s: %v, want it admitted", when, err)
		case !want:
			wantAPIError(t, err, http.StatusUnauthorized, "BROKER_TOKEN_RECORDS_NO_ACCOUNT", when)
		}
	}

	exec(`update people set sub = 'account-b' where email = $1`, person)
	storetest.SetRecordingTime(t, rig.database, person, nil)
	tokenActs("the deploy control: a personal token recording no account, no recording time", beforeToken, true)
	brokerActs("the deploy control: a broker credential recording no account, no recording time", broker(before), true)
	brokerActs("the deploy control: a broker credential with no creation time, no recording time", broker(time.Time{}), true)

	storetest.SetRecordingTime(t, rig.database, person, recordedAt)
	tokenActs("A's personal token made before B's recording", beforeToken, false)
	tokenActs("a personal token made after B's recording", afterToken, true)
	brokerActs("A's broker credential made before B's recording", broker(before), false)
	brokerActs("a broker credential whose creation time the broker did not record", broker(time.Time{}), false)
	brokerActs("a broker credential made 1 minute after the recording, inside the margin", broker(recordedAt.Add(time.Minute)), false)
	brokerActs("a broker credential made 10 minutes after the recording", broker(recordedAt.Add(10*time.Minute)), true)

	standing := func(actor model.Actor) error {
		_, _, err := served.approverStanding(httptest.NewRequest(http.MethodPost, "/api/v1/credential-requests/x/approve", nil), actor)
		return err
	}
	owner := person
	byToken := func(id string) model.Actor {
		return model.Actor{Kind: "session", Owner: &owner, ID: person, Act: &model.ActorAct{TokenID: id}}
	}
	if err := standing(byToken(beforeID)); err == nil {
		t.Error("an approve's re-read of A's personal token made before the recording stood, want it refused")
	}
	if err := standing(byToken(afterID)); err != nil {
		t.Errorf("an approve's re-read of a personal token made after the recording: %v, want it to stand", err)
	}

	mint := func(tokenID *string) (string, error) {
		t.Helper()
		tx, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var id string
		err = tx.QueryRow(ctx, mintAgentTokenSQL, person, "minted", []byte("hash of a mint"), "prefix00", []string{model.ScopePerson}, []string{},
			nil, nil, tokenID, true).
			Scan(&id, new(string), new(string), new([]string), new([]string), new(any), new(any), new(any))
		if err != nil {
			return "", err
		}
		var subject *string
		if err := tx.QueryRow(ctx, `select subject from agent_tokens where id = $1`, id).Scan(&subject); err != nil {
			t.Fatal(err)
		}
		if subject == nil {
			return "", nil
		}
		return *subject, nil
	}
	if _, err := mint(&beforeID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a mint by A's personal token made before the recording answered %v, want nothing inserted", err)
	}
	if got, err := mint(&afterID); err != nil || got != "account-b" {
		t.Errorf("a mint by a personal token recording no account made after the recording recorded %q (%v), want the address's account account-b", got, err)
	}
	wantAPIError(t, mintCallerGone(&beforeID), http.StatusUnauthorized, "TOKEN_REVOKED",
		"the refusal of a mint by A's personal token made before the recording")
}
