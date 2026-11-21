package main

// The rows of who binds a person from outside and how an owner ends that binding: one owner per
// person (the link, the enrollment, a list and a transfer), the owner's Reset, and a removal from
// the person's last list. They run on passkey_signin_test.go's rig.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ownerOf signs email in with Google as a full member of example.com and makes them the owner of a
// new project key; it answers their session cookie.
func (r *passkeyRig) ownerOf(t *testing.T, key, email string) *http.Cookie {
	t.Helper()
	signedIn := codeFlowSignIn(t, r.handler, r.pool, map[string]any{"sub": "google-" + key, "email": email, "email_verified": true, "hd": "example.com"})
	cookie := sessionCookieOf(t, email+"'s Google sign-in", signedIn)
	if _, err := r.database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee, owner) values ($1, $1, $2, $2)`, key, email); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	return cookie
}

// personOn is owner's request, at the canonical origin, to method project key's list's entry for
// email: PUT names them, DELETE takes them off, and POST .../invitations and .../reset are the
// link and the Reset.
func personOn(handler http.Handler, owner *http.Cookie, method, key, email, then string) *httptest.ResponseRecorder {
	return call(handler, canonicalOrigin, method, "/api/v1/projects/"+key+"/access/people/"+url.PathEscape(email)+then, nil, owner)
}

// TestTheOwnersResetRevokesThePersonsPersonalTokens (R5): a personal token the person's passkey
// cookie minted stops at the owner's Reset, 401 TOKEN_REVOKED, while the person stays on the list:
// the Reset gives the address a new passkey account, as another account's sign-in would, so
// whoever used the link first keeps nothing the cookie made. The next link enrolls under that new
// account.
//
// Control: the Reset keeps the address's account (store.PgPasskeys.EndAccountTx recording no new
// subject), and the token still reads PRIV and the row fails.
func TestTheOwnersResetRevokesThePersonsPersonalTokens(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	minted := call(r.handler, passkeyOrigin, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "outsider's agent"}, cookie)
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("the outsider mints a personal token at their browser: %d %s", minted.Code, minted.Body.String())
	}
	read := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, passkeyOrigin+"/api/v1/projects", nil)
		request.Header.Set("Authorization", "Bearer "+token.Token)
		response := httptest.NewRecorder()
		r.handler.ServeHTTP(response, request)
		return response
	}
	if before := read(); before.Code != http.StatusOK {
		t.Fatalf("the outsider's token before the Reset answered %d %s, want 200", before.Code, before.Body.String())
	}
	subjectOf := func() string {
		var subject string
		if err := r.database.Pool.QueryRow(context.Background(), `select coalesce(sub, '') from people where email = $1`, outsiderEmail).Scan(&subject); err != nil {
			t.Fatal(err)
		}
		return subject
	}
	enrolledUnder := subjectOf()

	if reset := r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/reset", nil); reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of the outsider answered %d %s, want 200", reset.Code, reset.Body.String())
	}
	wantCode(t, "the outsider's personal token after the Reset", read(), http.StatusUnauthorized, "TOKEN_REVOKED")
	resetUnder := subjectOf()
	if resetUnder == "" || resetUnder == enrolledUnder {
		t.Fatalf("after the Reset the address records the account %q, want a new one in place of %q", resetUnder, enrolledUnder)
	}
	r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	if again := subjectOf(); again != resetUnder {
		t.Fatalf("the enrollment after the Reset records the account %q, want the Reset's %q", again, resetUnder)
	}
}

// TestTheOwnersResetEndsEveryProjectsUnusedLinks (R5): the owner's Reset ends the person's unused
// links of every project, not only the Reset's own: a link OTHER's owner made before PRIV's Reset
// enrolls nothing after it (404 INVITATION_UNKNOWN), so the person enrolls again only with a link
// made after the Reset.
//
// Control: end only the Reset's own project's links (store.DeleteInvitationsTx of PRIV), and
// OTHER's link begins an enrollment and the row fails.
func TestTheOwnersResetEndsEveryProjectsUnusedLinks(t *testing.T) {
	r := newPasskeyRig(t, nil)
	if _, err := r.database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee, owner) values ('OTHER', 'Other', $1, $1)`, privOwner); err != nil {
		t.Fatalf("seed OTHER: %v", err)
	}
	r.listOn(t, "OTHER", access.PrincipalPerson, outsiderEmail)
	r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	made := r.asOwner(http.MethodPost, "/api/v1/projects/OTHER/access/people/"+outsiderEmail+"/invitations", nil)
	_, fragment, found := strings.Cut(made.Body.String(), "#")
	other, _, _ := strings.Cut(fragment, `"`)
	if made.Code != http.StatusCreated || !found || len(other) != 43 {
		t.Fatalf("OTHER's owner makes a link for the outsider: %d %s, want 201 and its token", made.Code, made.Body.String())
	}

	if reset := r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/reset", nil); reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of the outsider on PRIV answered %d %s, want 200", reset.Code, reset.Body.String())
	}
	wantCode(t, "the enrollment with OTHER's link made before PRIV's Reset", r.beginEnrollment(r.handler, other), http.StatusNotFound, "INVITATION_UNKNOWN")
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the Reset %s holds %d passkey(s), want none", outsiderEmail, n)
	}
}

// TestOnlyTheOwnerOfEveryProjectNamingAPersonMakesTheirLink (R5): a passkey signs its person in to
// every project that names them, so its link binds them for every such project's owner. Mallory, a
// full member who owns MAL, names alice's outsider on MAL: her link for them, before they enroll
// with alice's link, is 409 LISTED_BY_ANOTHER_OWNER, since PRIV, alice's, names them too, and so
// is alice's own while MAL names them. Once the outsider enrolled with alice's link, Mallory's list
// cannot name them again (409 PASSKEY_OF_ANOTHER_OWNER), so no link of hers ever reaches their
// account. An owner of every project that names a person still makes their link: alice names the
// outsider on OTHER, hers too, and links them there.
//
// Control: make the link without asking who else names the person (createInvitation skipping
// store.ListedByAnotherOwner), and Mallory's link before the outsider's enrollment answers 201 and
// the row fails.
func TestOnlyTheOwnerOfEveryProjectNamingAPersonMakesTheirLink(t *testing.T) {
	r := newPasskeyRig(t, nil)
	const mallory = "mallory@example.com"
	malloryCookie := r.ownerOf(t, "MAL", mallory)
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory names the outsider on MAL: %d %s, want 200", listed.Code, listed.Body.String())
	}
	wantCode(t, "Mallory's link for the outsider, before their enrollment", personOn(r.handler, malloryCookie, http.MethodPost, "MAL", outsiderEmail, "/invitations"), http.StatusConflict, "LISTED_BY_ANOTHER_OWNER")
	wantCode(t, "alice's link for the outsider while MAL names them", r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/invitations", nil), http.StatusConflict, "LISTED_BY_ANOTHER_OWNER")

	if removed := personOn(r.handler, malloryCookie, http.MethodDelete, "MAL", outsiderEmail, ""); removed.Code != http.StatusOK {
		t.Fatalf("Mallory takes the outsider off MAL: %d %s, want 200", removed.Code, removed.Body.String())
	}
	r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	wantCode(t, "Mallory's list naming the outsider, after their enrollment with alice's link", personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""), http.StatusConflict, "PASSKEY_OF_ANOTHER_OWNER")
	wantCode(t, "Mallory's link for the outsider, after their enrollment", personOn(r.handler, malloryCookie, http.MethodPost, "MAL", outsiderEmail, "/invitations"), http.StatusConflict, "PERSON_NOT_LISTED")

	if _, err := r.database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee, owner) values ('OTHER', 'Other', $1, $1)`, privOwner); err != nil {
		t.Fatalf("seed OTHER: %v", err)
	}
	if listed := personOn(r.handler, r.alice, http.MethodPut, "OTHER", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("alice names the outsider on OTHER, hers too: %d %s, want 200", listed.Code, listed.Body.String())
	}
	linkToken(t, "alice's link for the outsider on OTHER", personOn(r.handler, r.alice, http.MethodPost, "OTHER", outsiderEmail, "/invitations"))
}

// TestAListRefusesAPersonWhosePasskeyAnotherOwnersLinkEnrolled (R5): Mallory names
// solo@other.example on MAL, which no other list names, and solo enrolls with her link; alice's list
// naming solo afterwards is 409 PASSKEY_OF_ANOTHER_OWNER, so Mallory's passkey never reads PRIV.
// Mallory's access answer names her as the passkey's link maker and MAL as its project. Once
// Mallory resets solo, alice names them.
//
// Control: name the person without reading whose link enrolled their passkeys
// (putProjectAccessPerson skipping refusePasskeyOfAnotherOwner), and alice's list names solo, 200,
// and the row fails.
func TestAListRefusesAPersonWhosePasskeyAnotherOwnersLinkEnrolled(t *testing.T) {
	r := newPasskeyRig(t, nil)
	const mallory, solo = "mallory@example.com", "solo@other.example"
	malloryCookie := r.ownerOf(t, "MAL", mallory)
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", solo, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory names solo on MAL: %d %s, want 200", listed.Code, listed.Body.String())
	}
	r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), linkToken(t, "Mallory's link for solo", personOn(r.handler, malloryCookie, http.MethodPost, "MAL", solo, "/invitations")), solo)

	wantCode(t, "alice's list naming solo, whose passkey Mallory's link enrolled", personOn(r.handler, r.alice, http.MethodPut, "PRIV", solo, ""), http.StatusConflict, "PASSKEY_OF_ANOTHER_OWNER")
	var answer struct {
		Passkeys []struct {
			Email   string  `json:"email"`
			MadeBy  string  `json:"made_by"`
			Project *string `json:"project"`
		} `json:"passkeys"`
	}
	read := call(r.handler, canonicalOrigin, http.MethodGet, "/api/v1/projects/MAL/access", nil, malloryCookie)
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &answer) != nil || len(answer.Passkeys) != 1 ||
		answer.Passkeys[0].MadeBy != mallory || answer.Passkeys[0].Project == nil || *answer.Passkeys[0].Project != "MAL" {
		t.Fatalf("Mallory's read of MAL's access: %d %s, want solo's passkey made by her through MAL", read.Code, read.Body.String())
	}

	if reset := personOn(r.handler, malloryCookie, http.MethodPost, "MAL", solo, "/reset"); reset.Code != http.StatusOK {
		t.Fatalf("Mallory resets solo: %d %s, want 200", reset.Code, reset.Body.String())
	}
	if listed := personOn(r.handler, r.alice, http.MethodPut, "PRIV", solo, ""); listed.Code != http.StatusOK {
		t.Fatalf("alice names solo once Mallory reset them: %d %s, want 200", listed.Code, listed.Body.String())
	}
}

// TestALinkUsedAfterAnotherOwnerNamesThePersonEnrollsNothing (R5): alice makes a link while PRIV
// alone names the outsider, and their enrollment begins; Mallory then names them on MAL. The
// enrollment's finish, which reads the lists again under the address's lock (store.PgPasskeys's
// Enroll), is 403 LISTED_BY_ANOTHER_OWNER and stores no passkey, and a new begin with the link is
// refused alike.
//
// Control: drop Enroll's read of who else names the person, and the finish answers 200 and the row
// fails.
func TestALinkUsedAfterAnotherOwnerNamesThePersonEnrollsNothing(t *testing.T) {
	r := newPasskeyRig(t, nil)
	token := r.invite(t, outsiderEmail)
	begun := r.beginEnrollment(r.handler, token)
	if begun.Code != http.StatusOK {
		t.Fatalf("the outsider's enrollment begins: %d %s, want 200", begun.Code, begun.Body.String())
	}
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory names the outsider on MAL: %d %s, want 200", listed.Code, listed.Body.String())
	}
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	finished := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/register/finish", device.register(t, begun.Body.Bytes()), ceremonyCookie(begun))
	wantCode(t, "the enrollment's finish once MAL names the outsider", finished, http.StatusForbidden, "LISTED_BY_ANOTHER_OWNER")
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("the outsider holds %d passkey(s), want none", n)
	}
	wantCode(t, "a new begin with alice's link once MAL names the outsider", r.beginEnrollment(r.handler, token), http.StatusForbidden, "LISTED_BY_ANOTHER_OWNER")
}

// TestTheOwnersResetRefusesAWorkspaceAddress: the owner's Reset of an address on a domain of the
// company's Workspace that the list names is 400 WORKSPACE_ADDRESS, as a link for it is: such an
// address signs in with Google, so it holds no passkey to reset.
//
// Control: drop the Reset's Workspace check (resetProjectAccessPerson), and the Reset answers 200
// and the row fails.
func TestTheOwnersResetRefusesAWorkspaceAddress(t *testing.T) {
	r := newPasskeyRig(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "example.ai"})
	const workspace = "someone@example.ai"
	r.listOn(t, "PRIV", access.PrincipalPerson, workspace)
	wantCode(t, "alice's Reset of "+workspace, personOn(r.handler, r.alice, http.MethodPost, "PRIV", workspace, "/reset"), http.StatusBadRequest, "WORKSPACE_ADDRESS")
}

// TestAResetUnderTheAddressLockRefusesAFullMember: store.PgPasskeys's EndAccountTx, which the
// Reset and the last-list removal call, reads the member kind again under the address's lock,
// which a Google sign-in takes too, so a full member whose first sign-in committed after the
// Reset's handler read the list is auth.ErrAddressHeldByMember, and nothing is written: their
// personal tokens and account stand.
//
// Control: drop EndAccountTx's member-kind read, and it revokes the full member's account and
// answers nil, and the row fails.
func TestAResetUnderTheAddressLockRefusesAFullMember(t *testing.T) {
	r := newPasskeyRig(t, nil)
	sessionCookieOf(t, outsiderEmail+"'s Google sign-in as a Workspace account", codeFlowSignIn(t, r.handler, r.pool, map[string]any{
		"sub": "google-outsider", "email": outsiderEmail, "email_verified": true, "hd": "example.com",
	}))
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.NewPgPasskeys(r.people.PgPeopleStore, passkeyOrigin).EndAccountTx(ctx, tx, outsiderEmail); !errors.Is(err, auth.ErrAddressHeldByMember) {
		t.Fatalf("EndAccountTx of a full member's address answered %v, want auth.ErrAddressHeldByMember", err)
	}
}

// TestATransferRefusesAProjectNamingAnotherOwnersPasskey (R5): alice's link enrolled the outsider's
// passkey, so handing PRIV to bob would leave alice, PRIV's owner until then, an account of her own
// binding that reads bob's project: the transfer is 409 PASSKEY_OF_ANOTHER_OWNER and PRIV stays
// hers. Once alice resets the outsider, the transfer goes through.
//
// Control: give the project away without reading whose links enrolled its people's passkeys
// (patchProjectAccess skipping refuseTransferOfAnotherOwnersPasskey), and the transfer answers 200
// and the row fails.
func TestATransferRefusesAProjectNamingAnotherOwnersPasskey(t *testing.T) {
	r := newPasskeyRig(t, nil)
	r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	const bob = "bob@example.com"
	r.ownerOf(t, "BOB", bob)
	give := map[string]string{"owner": bob}
	wantCode(t, "alice's transfer of PRIV to bob while the outsider holds her link's passkey", r.asOwner(http.MethodPatch, "/api/v1/projects/PRIV/access", give), http.StatusConflict, "PASSKEY_OF_ANOTHER_OWNER")
	var owner string
	if err := r.database.Pool.QueryRow(context.Background(), `select owner from projects where key = 'PRIV'`).Scan(&owner); err != nil || owner != privOwner {
		t.Fatalf("PRIV's owner after the refused transfer is %q (%v), want %s", owner, err, privOwner)
	}
	if reset := personOn(r.handler, r.alice, http.MethodPost, "PRIV", outsiderEmail, "/reset"); reset.Code != http.StatusOK {
		t.Fatalf("alice resets the outsider: %d %s, want 200", reset.Code, reset.Body.String())
	}
	if given := r.asOwner(http.MethodPatch, "/api/v1/projects/PRIV/access", give); given.Code != http.StatusOK {
		t.Fatalf("alice's transfer of PRIV to bob after the Reset: %d %s, want 200", given.Code, given.Body.String())
	}
}

// TestRemovingTheLastListingRevokesThePersonsPersonalTokens (R5): taking a person off the last list
// that names them ends what their account made, as the owner's Reset does: their personal token is
// refused at once, and once Mallory, another owner, names the address on MAL, the same token still
// reads nothing of MAL, since the removal gave the address a new passkey account. The address's
// next enrollment joins that new account, not the old one.
//
// Control: end the sign-in alone (store.PgPasskeys.EndAccountTx calling endPasskeySignInTx in
// place of endAccountTx, as the removal did before), and the old token reads MAL's access, 200,
// and the row fails.
func TestRemovingTheLastListingRevokesThePersonsPersonalTokens(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	minted := call(r.handler, passkeyOrigin, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "outsider's agent"}, cookie)
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("the outsider mints a personal token at their browser: %d %s", minted.Code, minted.Body.String())
	}
	read := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, passkeyOrigin+path, nil)
		request.Header.Set("Authorization", "Bearer "+token.Token)
		response := httptest.NewRecorder()
		r.handler.ServeHTTP(response, request)
		return response
	}
	if before := read("/api/v1/projects"); before.Code != http.StatusOK {
		t.Fatalf("the outsider's token before the removal answered %d %s, want 200", before.Code, before.Body.String())
	}
	if removed := personOn(r.handler, r.alice, http.MethodDelete, "PRIV", outsiderEmail, ""); removed.Code != http.StatusOK {
		t.Fatalf("alice takes the outsider off PRIV, their last list: %d %s, want 200", removed.Code, removed.Body.String())
	}
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory names the outsider on MAL: %d %s, want 200", listed.Code, listed.Body.String())
	}
	wantCode(t, "the outsider's old token reading MAL's access", read("/api/v1/projects/MAL/access"), http.StatusUnauthorized, "TOKEN_REVOKED")
}

// TestTheHourlyEndRevokesTheUnlistedPersonsPersonalTokens (R5): a person the lists stop naming by
// any write but a removal loses, at the hourly check that ends their sign-in, what their account
// made, as at a removal from their last list: once Mallory names the address on MAL, their old
// personal token still reads nothing of MAL.
//
// Control: end the sign-in alone at the hourly check (settle calling endPasskeySignInTx in place of
// endAccountTx for an unlisted person), and the old token reads MAL's access, 200, and the row
// fails.
func TestTheHourlyEndRevokesTheUnlistedPersonsPersonalTokens(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	minted := call(r.handler, passkeyOrigin, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "outsider's agent"}, cookie)
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("the outsider mints a personal token at their browser: %d %s", minted.Code, minted.Body.String())
	}
	if _, err := r.database.Pool.Exec(context.Background(), `delete from project_access where principal = $1`, outsiderEmail); err != nil {
		t.Fatal(err)
	}
	r.agePasskeyConfirmation(t, outsiderEmail)
	if whoami := whoamiOutside(r.handler, cookie); whoami.Code != http.StatusUnauthorized {
		t.Fatalf("the whoami that ran the hourly check answered %d %s, want 401", whoami.Code, whoami.Body.String())
	}
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory names the outsider on MAL: %d %s, want 200", listed.Code, listed.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, passkeyOrigin+"/api/v1/projects/MAL/access", nil)
	request.Header.Set("Authorization", "Bearer "+token.Token)
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	wantCode(t, "the outsider's old token reading MAL's access after the hourly end", response, http.StatusUnauthorized, "TOKEN_REVOKED")
}

// TestTheOwnersAnswerNamesWhomAnotherOwnersProjectNames (R5): while MAL, Mallory's, names alice's
// outsider too, alice's access answer lists them in listed_by_another_owner and not in invitable,
// so the Access tab offers no link the route would refuse (409 LISTED_BY_ANOTHER_OWNER) and says
// why; once Mallory takes them off MAL, they are invitable again and listed_by_another_owner is [].
//
// Control: leave invitable as it was (withOwnersPasskeys skipping store.ListedByOtherOwners), and
// the outsider is invitable while MAL names them and the row fails.
func TestTheOwnersAnswerNamesWhomAnotherOwnersProjectNames(t *testing.T) {
	r := newPasskeyRig(t, nil)
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory names the outsider on MAL: %d %s, want 200", listed.Code, listed.Body.String())
	}
	type fields struct {
		Invitable            []string `json:"invitable"`
		ListedByAnotherOwner []string `json:"listed_by_another_owner"`
	}
	read := func(what string) fields {
		t.Helper()
		var answer fields
		response := r.asOwner(http.MethodGet, "/api/v1/projects/PRIV/access", nil)
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &answer) != nil || answer.Invitable == nil || answer.ListedByAnotherOwner == nil {
			t.Fatalf("alice's read of PRIV's access %s: %d %s, want both fields", what, response.Code, response.Body.String())
		}
		return answer
	}
	while := read("while MAL names the outsider")
	if strings.Contains(strings.Join(while.Invitable, " "), outsiderEmail) || strings.Join(while.ListedByAnotherOwner, " ") != outsiderEmail {
		t.Fatalf("while MAL names the outsider alice's answer is %+v, want them listed by another owner and not invitable", while)
	}
	if removed := personOn(r.handler, malloryCookie, http.MethodDelete, "MAL", outsiderEmail, ""); removed.Code != http.StatusOK {
		t.Fatalf("Mallory takes the outsider off MAL: %d %s, want 200", removed.Code, removed.Body.String())
	}
	after := read("once MAL no longer names the outsider")
	if !strings.Contains(strings.Join(after.Invitable, " "), outsiderEmail) || len(after.ListedByAnotherOwner) != 0 {
		t.Fatalf("once MAL no longer names the outsider alice's answer is %+v, want them invitable and listed by no other owner", after)
	}
}

// TestTheOwnersAnswerNamesOnlyPeopleFromOutsideAsListedByAnotherOwner (R5): MAL, Mallory's, names
// dana, a full member, someone@example.ai, an address on a Workspace domain, and alice's outsider,
// each of whom PRIV, alice's, names too; alice's answer lists the outsider alone in
// listed_by_another_owner, since only a person from outside is refused a link for another owner's
// listing (dana's link is 409 FULL_MEMBER and the Workspace address's 400 WORKSPACE_ADDRESS
// whatever else names them), and neither of the other two is invitable either.
//
// Control: name every person another owner's project names (withOwnersPasskeys passing
// store.ListedByOtherOwners through, as at aa6950fcf1), and dana and the Workspace address appear in
// listed_by_another_owner and the row fails.
func TestTheOwnersAnswerNamesOnlyPeopleFromOutsideAsListedByAnotherOwner(t *testing.T) {
	r := newPasskeyRig(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "example.ai"})
	const dana, workspace = "dana@example.com", "someone@example.ai"
	sessionCookieOf(t, "dana's Google sign-in", codeFlowSignIn(t, r.handler, r.pool, map[string]any{"sub": "google-dana", "email": dana, "email_verified": true, "hd": "example.com"}))
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	for _, person := range []string{dana, workspace, outsiderEmail} {
		r.listOn(t, "PRIV", access.PrincipalPerson, person)
		if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", person, ""); listed.Code != http.StatusOK {
			t.Fatalf("Mallory names %s on MAL: %d %s, want 200", person, listed.Code, listed.Body.String())
		}
	}
	var answer struct {
		Invitable            []string `json:"invitable"`
		ListedByAnotherOwner []string `json:"listed_by_another_owner"`
	}
	response := r.asOwner(http.MethodGet, "/api/v1/projects/PRIV/access", nil)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &answer) != nil {
		t.Fatalf("alice's read of PRIV's access: %d %s", response.Code, response.Body.String())
	}
	if !slices.Equal(answer.ListedByAnotherOwner, []string{outsiderEmail}) || len(answer.Invitable) != 0 {
		t.Fatalf("alice's answer is %+v, want listed_by_another_owner [%s] alone and nobody invitable", answer, outsiderEmail)
	}
}

// TestTheOwnersAnswerListsTheProjectsOpenLinks (R5): alice's access answer lists each link of PRIV
// that can still enroll someone, with the made_at her Invite answered and no token, so her page
// drops a link it shows once something else ended it. A newer link ends the older one, a use
// ends the link it used, and a Reset ends the person's unused links: each leaves the list.
//
// Control: read the open links without `used_at is null` (store.OpenInvitations), and the used
// link stays listed and the row fails.
func TestTheOwnersAnswerListsTheProjectsOpenLinks(t *testing.T) {
	r := newPasskeyRig(t, nil)
	type invitation struct {
		Email  string `json:"email"`
		MadeAt string `json:"made_at"`
	}
	open := func(what string) []invitation {
		t.Helper()
		response := r.asOwner(http.MethodGet, "/api/v1/projects/PRIV/access", nil)
		var answer struct {
			OpenInvitations *[]invitation `json:"open_invitations"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &answer) != nil || answer.OpenInvitations == nil {
			t.Fatalf("alice's access answer %s: %d %s, want 200 with open_invitations", what, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "/enroll#") {
			t.Fatalf("alice's access answer %s holds a link's token: %s", what, response.Body.String())
		}
		return *answer.OpenInvitations
	}
	madeAt := func(response *httptest.ResponseRecorder) string {
		t.Helper()
		var answer struct {
			MadeAt string `json:"made_at"`
		}
		if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &answer) != nil {
			t.Fatalf("alice's Invite answered %d %s, want 201", response.Code, response.Body.String())
		}
		return answer.MadeAt
	}
	if got := open("before any Invite"); len(got) != 0 {
		t.Fatalf("open_invitations before any Invite = %+v, want []", got)
	}
	first := madeAt(r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/invitations", nil))
	if got := open("after the first Invite"); !slices.Equal(got, []invitation{{outsiderEmail, first}}) {
		t.Fatalf("open_invitations after the first Invite = %+v, want the one link made at %s", got, first)
	}
	secondResponse := r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/invitations", nil)
	second := madeAt(secondResponse)
	if got := open("after a second Invite"); !slices.Equal(got, []invitation{{outsiderEmail, second}}) {
		t.Fatalf("open_invitations after a second Invite = %+v, want the newer link alone, made at %s", got, second)
	}
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, linkToken(t, "the second link", secondResponse), outsiderEmail)
	if got := open("after the enrollment"); len(got) != 0 {
		t.Fatalf("open_invitations after the enrollment used the link = %+v, want []", got)
	}
	r.invite(t, outsiderEmail)
	if reset := r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/reset", nil); reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset answered %d %s, want 200", reset.Code, reset.Body.String())
	}
	if got := open("after the Reset"); len(got) != 0 {
		t.Fatalf("open_invitations after the Reset = %+v, want []", got)
	}
}

// TestTheOwnersResetEndsSignInAndDeletesPasskeys (R5): the owner's Reset of a person the list still
// names ends their sign-in and deletes their passkeys at once, and leaves them on the list, so a
// new link enrolls them again. Reset is the owner's browser's alone: a full member's address is
// 409 FULL_MEMBER.
//
// Control: skip the end in the Reset (resetProjectAccessPerson not calling
// store.PgPasskeys.EndAccountTx), and the row fails naming the passkey still stored.
func TestTheOwnersResetEndsSignInAndDeletesPasskeys(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	cookie := r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	unused := r.invite(t, outsiderEmail)

	reset := r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/reset", nil)
	if reset.Code != http.StatusOK || !strings.Contains(reset.Body.String(), `"people":["`+outsiderEmail+`"]`) {
		t.Fatalf("alice's Reset of the outsider answered %d %s, want 200 with the outsider still listed", reset.Code, reset.Body.String())
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the Reset %s holds %d passkey(s), want every one deleted", outsiderEmail, n)
	}
	if whoami := whoamiOutside(r.handler, cookie); whoami.Code != http.StatusUnauthorized {
		t.Fatalf("whoami after the Reset answered %d %s, want 401", whoami.Code, whoami.Body.String())
	}
	wantCode(t, "a passkey sign-in after the Reset", r.signIn(t, r.handler, device), http.StatusUnauthorized, "PASSKEY_UNKNOWN")
	wantCode(t, "the link made before the Reset", r.beginEnrollment(r.handler, unused), http.StatusNotFound, "INVITATION_UNKNOWN")

	again := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	signedInRestricted(t, r.handler, again, outsiderEmail)

	r.listOn(t, "PRIV", access.PrincipalPerson, privOwner)
	wantCode(t, "a Reset of a full member", r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+privOwner+"/reset", nil), http.StatusConflict, "FULL_MEMBER")
}

// TestPasskeyAccessFieldsAreTheOwnersAlone: GET /api/v1/projects/{key}/access answers the owner's
// browser session, where passkey sign-in is on, when each passkey of a listed person was enrolled
// (passkeys) and the people a link can be made for now (invitable: on no Workspace domain, no full
// member's); a full member the list names and the restricted member read neither field, and with
// passkey sign-in off the owner reads neither either.
//
// Control: answer the two fields to every reader (withOwnersPasskeys skipping its owner check),
// and the full member's read carries passkeys and the row fails.
func TestPasskeyAccessFieldsAreTheOwnersAlone(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	const dana = "dana@example.com"
	danaSignIn := codeFlowSignIn(t, r.handler, r.pool, map[string]any{"sub": "google-dana", "email": dana, "email_verified": true, "hd": "example.com"})
	r.listOn(t, "PRIV", access.PrincipalPerson, dana)
	r.listOn(t, "PRIV", access.PrincipalPerson, "pending@other.example")

	var owners struct {
		Passkeys  []model.EnrolledPasskey `json:"passkeys"`
		Invitable []string                `json:"invitable"`
	}
	read := r.asOwner(http.MethodGet, "/api/v1/projects/PRIV/access", nil)
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &owners) != nil {
		t.Fatalf("alice's read of PRIV's access: %d %s", read.Code, read.Body.String())
	}
	if len(owners.Passkeys) != 1 || owners.Passkeys[0].Email != outsiderEmail || owners.Passkeys[0].EnrolledAt.IsZero() {
		t.Fatalf("alice reads passkeys %+v, want the outsider's one, with its enrollment time", owners.Passkeys)
	}
	if want := []string{outsiderEmail, "pending@other.example"}; !slices.Equal(owners.Invitable, want) {
		t.Fatalf("alice reads invitable %v, want %v: no full member", owners.Invitable, want)
	}
	for who, response := range map[string]*httptest.ResponseRecorder{
		"dana, a full member the list names": call(r.handler, canonicalOrigin, http.MethodGet, "/api/v1/projects/PRIV/access", nil, sessionCookieOf(t, "dana's sign-in", danaSignIn)),
		"the restricted member":              call(r.handler, passkeyOrigin, http.MethodGet, "/api/v1/projects/PRIV/access", nil, cookie),
	} {
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"passkeys"`) || strings.Contains(response.Body.String(), `"invitable"`) {
			t.Fatalf("%s's read of PRIV's access: %d %s, want 200 with neither passkeys nor invitable", who, response.Code, response.Body.String())
		}
	}
	off, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})
	if off := call(off, canonicalOrigin, http.MethodGet, "/api/v1/projects/PRIV/access", nil, r.alice); off.Code != http.StatusOK || strings.Contains(off.Body.String(), `"passkeys"`) {
		t.Fatalf("alice's read with passkey sign-in off: %d %s, want 200 with no passkeys", off.Code, off.Body.String())
	}
}

// TestARemovalRacingAFirstEnrollmentCommitsBoth (R5): the person's first enrollment and the
// owner's removal of them from their last list, run at once through the real routes. The
// enrollment's finish takes the link, the address, and then, recording the projects the person no
// longer sees as a restricted member, their sessions' owner lock (store.RecordReachedProjects). It
// is held after writing the person's row, holding the link and the address, while the owner's
// DELETE starts; once it goes on, both commit: the finish answers 200 naming the person, the
// DELETE answers 200, and the removal, which read the lists after the enrollment, ends the account
// it made (no passkey is left).
//
// Control: remove the person before taking the links and the address (deleteProjectAccessPerson
// calling removePerson before lockPasskeyRemoval, as at 516b5bfa981), and the removal holds the
// person's sessions' lock while it waits for the link the finish holds, PostgreSQL fails one of
// them (40P01), and the row fails.
func TestARemovalRacingAFirstEnrollmentCommitsBoth(t *testing.T) {
	r := newPasskeyRig(t, nil)
	ctx := context.Background()
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	begun := r.beginEnrollment(r.handler, r.invite(t, outsiderEmail))
	if begun.Code != http.StatusOK {
		t.Fatalf("the outsider's enrollment begin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
	created := device.register(t, begun.Body.Bytes())
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := r.database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The finish waits, after writing the person's row and before recording their projects, for a
	// lock the test holds.
	const hold = 386208
	exec(`create function hold_the_enrollment() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(386208);
			return null;
		end $$`)
	exec(`create trigger hold_the_enrollment after update on people for each row execute function hold_the_enrollment()`)
	t.Cleanup(func() {
		_, _ = r.database.Pool.Exec(context.Background(), `drop trigger if exists hold_the_enrollment on people`)
		_, _ = r.database.Pool.Exec(context.Background(), `drop function if exists hold_the_enrollment()`)
	})
	held, err := r.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if _, err := held.Exec(ctx, `select pg_advisory_lock($1)`, hold); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if _, err := held.Exec(ctx, `select pg_advisory_unlock($1)`, hold); err != nil {
				t.Errorf("release the enrollment: %v", err)
			}
		}
	}
	defer release()
	waits := func(want int, what string) {
		t.Helper()
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
			var n int
			if err := r.database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
				where datname = current_database() and wait_event_type = 'Lock'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d waits on a lock after a minute, want %d", what, n, want)
			}
		}
	}

	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/register/finish", created, ceremonyCookie(begun))
	}()
	waits(1, "the finish held after writing the outsider's row")
	removed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		removed <- r.asOwner(http.MethodDelete, "/api/v1/projects/PRIV/access/people/"+url.PathEscape(outsiderEmail), nil)
	}()
	waits(2, "the removal waiting behind the finish")
	release()
	if response := <-finished; response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"`+outsiderEmail+`"`) {
		t.Fatalf("the outsider's enrollment finish answered %d %s, want 200 naming them", response.Code, response.Body.String())
	}
	if response := <-removed; response.Code != http.StatusOK {
		t.Fatalf("alice's removal of the outsider from PRIV answered %d %s, want 200", response.Code, response.Body.String())
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the removal from their last list the outsider holds %d passkey(s), want the enrollment's ended", n)
	}
}

// TestTheOneOwnerRuleAndTheLastListEndHoldWithPasskeysOff (R5): passkeys enrolled while passkey
// sign-in was on outlive it being turned off, so the writes that guard them run with it off too.
// While a server runs with DISPATCH_SIGNIN_PASSKEY unset over the same database, Mallory's list
// naming the outsider, whose passkey alice's link enrolled, is refused (409
// PASSKEY_OF_ANOTHER_OWNER), and alice's removal of the outsider from PRIV, their last list, ends
// their account and deletes their passkey. Once the setting is on again the old passkey signs in to
// nothing and the old cookie reads nothing.
//
// Control: skip the refusal and the end while the setting is off (each returning nil when
// Deps.Passkeys is nil, as at 49d3942d216), and Mallory's list answers 200, the passkey survives
// the removal, and the row fails.
func TestTheOneOwnerRuleAndTheLastListEndHoldWithPasskeysOff(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	cookie := r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	off, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})

	wantCode(t, "Mallory's list naming the outsider with passkeys off", personOn(off, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""), http.StatusConflict, "PASSKEY_OF_ANOTHER_OWNER")
	if removed := personOn(off, r.alice, http.MethodDelete, "PRIV", outsiderEmail, ""); removed.Code != http.StatusOK {
		t.Fatalf("alice's removal of the outsider from PRIV with passkeys off answered %d %s, want 200", removed.Code, removed.Body.String())
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the removal from their last list with passkeys off the outsider holds %d passkey(s), want every one deleted", n)
	}

	if signedIn := r.signIn(t, r.handler, device); signedIn.Code == http.StatusOK {
		t.Fatalf("the old passkey's sign-in once passkeys are on again answered 200 %s, want it refused", signedIn.Body.String())
	}
	if whoami := whoamiOutside(r.handler, cookie); whoami.Code != http.StatusUnauthorized {
		t.Fatalf("the old cookie's whoami once passkeys are on again answered %d %s, want 401", whoami.Code, whoami.Body.String())
	}
}

// TestAPasskeySignInIsRefusedWhileAnotherOwnersProjectNamesThePerson (R5): a passkey reads only
// projects its link's maker owns, so its sign-in is refused (403 LISTED_BY_ANOTHER_OWNER) while a
// project of another owner names its person directly, whatever wrote that list: here a write that
// skipped the list's refusal, as an earlier release or a direct write of the table would. The
// person signs in again once that project no longer names them.
//
// Control: skip the check in the sign-in (store.PgPasskeys.SignIn reading no made_by), and the
// sign-in answers 200 and the row fails.
func TestAPasskeySignInIsRefusedWhileAnotherOwnersProjectNamesThePerson(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	r.ownerOf(t, "MAL", "mallory@example.com")
	r.listOn(t, "MAL", access.PrincipalPerson, outsiderEmail)

	wantCode(t, "the outsider's passkey sign-in while MAL names them", r.signIn(t, r.handler, device), http.StatusForbidden, "LISTED_BY_ANOTHER_OWNER")
	if _, err := r.database.Pool.Exec(context.Background(), `delete from project_access where project_key = 'MAL' and principal = $1`, outsiderEmail); err != nil {
		t.Fatalf("take the outsider off MAL: %v", err)
	}
	if signedIn := r.signIn(t, r.handler, device); signedIn.Code != http.StatusOK {
		t.Fatalf("the outsider's passkey sign-in once MAL no longer names them answered %d %s, want 200", signedIn.Code, signedIn.Body.String())
	}
}

// TestAGoogleSignInThatReplacesAPasskeyAccountDeletesItsPasskeys (R5): a person from outside who
// later signs in with Google as a full member (an address of the Workspace on another domain)
// replaces their passkey account, and the replacement deletes that account's passkeys in its own
// transaction: none signs in again, and none counts as alice's passkey of the address, so
// Mallory's list naming them and alice's transfer of PRIV both answer 200. A Reset of the full
// member stays 409 FULL_MEMBER, as for any full member.
//
// Control: keep the passkeys at the replacement (signInAccountTx deleting none), and the passkey
// count is 1, Mallory's list answers 409 PASSKEY_OF_ANOTHER_OWNER, and the row fails.
func TestAGoogleSignInThatReplacesAPasskeyAccountDeletesItsPasskeys(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	sessionCookieOf(t, "the outsider's Google sign-in as a full member", codeFlowSignIn(t, r.handler, r.pool, map[string]any{
		"sub": "google-outsider", "email": outsiderEmail, "email_verified": true, "hd": "example.com",
	}))
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the Google account replaced the passkey account the outsider holds %d passkey(s), want none", n)
	}
	if signedIn := r.signIn(t, r.handler, device); signedIn.Code == http.StatusOK {
		t.Fatalf("the replaced account's passkey signed in: %s", signedIn.Body.String())
	}
	wantCode(t, "alice's Reset of the full member", r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/reset", nil), http.StatusConflict, "FULL_MEMBER")
	malloryCookie := r.ownerOf(t, "MAL", "mallory@example.com")
	if listed := personOn(r.handler, malloryCookie, http.MethodPut, "MAL", outsiderEmail, ""); listed.Code != http.StatusOK {
		t.Fatalf("Mallory's list naming the full member answered %d %s, want 200", listed.Code, listed.Body.String())
	}
	if given := r.asOwner(http.MethodPatch, "/api/v1/projects/PRIV/access", map[string]string{"owner": "mallory@example.com"}); given.Code != http.StatusOK {
		t.Fatalf("alice's transfer of PRIV to Mallory answered %d %s, want 200", given.Code, given.Body.String())
	}
}

// TestASignInCeremonyAllowsOneFinish: a sign-in's finish records its ceremony used before it checks
// the answer, so one begin allows one finish: after an answer that does not verify, the person's
// own answer to the same ceremony is 400 CEREMONY_EXPIRED, and a new begin signs them in.
//
// Control: record the ceremony only once the answer verifies (FinishSignIn not calling
// store.PgPasskeys.UseCeremony first, as at 49d3942d216), and the second finish answers 200 and the
// row fails.
func TestASignInCeremonyAllowsOneFinish(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	begun := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{})
	if begun.Code != http.StatusOK {
		t.Fatalf("the sign-in's begin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
	if failed := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", []byte(`{}`), ceremonyCookie(begun)); failed.Code == http.StatusOK {
		t.Fatalf("a finish with an empty answer answered 200 %s", failed.Body.String())
	}
	wantCode(t, "the person's own answer to the ceremony a failed finish took", call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", device.assert(t, begun.Body.Bytes()), ceremonyCookie(begun)), http.StatusBadRequest, "CEREMONY_EXPIRED")
	if signedIn := r.signIn(t, r.handler, device); signedIn.Code != http.StatusOK {
		t.Fatalf("a new sign-in answered %d %s, want 200", signedIn.Code, signedIn.Body.String())
	}
}
