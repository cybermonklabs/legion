// packages/envoy/internal/broker/requests/narrowing_unchanged_test.go
// A narrowing on a request is read once, by the check, and kept nowhere: it is unsigned, it
// narrows nothing by itself, and the release is what applies one. So a request that carries a
// narrowing must leave behind exactly what the same request without one leaves — the same signed
// request object, the same approval record for the person who decides it, the same grant row —
// and the grant it makes must release owner-wide, as every grant does for a release that asks
// for no narrowing. A broker that recorded the narrowing anywhere would be promising the
// approver, or the next release, something the narrowing never said.
package requests_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// narrowingWordsNeverRecorded are the words a recorded narrowing would carry: the repositories it
// names and the field names it is written under. The account's own login is not among them
// because a lowercase-hex id — a rules version, a row's uuid — can spell one by chance, which
// would make this search answer for something that is no narrowing at all.
var narrowingWordsNeverRecorded = []string{standInRepository, "read_only", "github_app", "narrow"}

// TestNarrowingOnARequestChangesNothingElse: one request carrying a narrowing and one carrying
// none, both approved by the same person, leave behind the same signed request object fields,
// the same approval record and the same grant row; and each grant releases the owner-wide
// installations object its secret has always released, whose token reaches a repository the
// narrowing never named.
func TestNarrowingOnARequestChangesNothingElse(t *testing.T) {
	f := newNarrowingFixture(t, map[string]int64{standInOwner: 7, otherOwner: 9})
	narrowing := &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{standInRepository}, ReadOnly: true}

	narrowed, err := f.create(t, narrowing, "APP_KEY")
	if err != nil || narrowed.State != "pending" || narrowed.RecordID == nil {
		t.Fatalf("the narrowed Create = %+v, %v; want it pending on its approver", narrowed, err)
	}
	plain, err := f.create(t, nil, "OTHER_APP_KEY")
	if err != nil || plain.State != "pending" || plain.RecordID == nil {
		t.Fatalf("the Create with no narrowing = %+v, %v; want it pending on its approver", plain, err)
	}

	// The signed request object each record carries: the claims a session signed, which the
	// narrowing is no part of, since it is sent beside the signature and never inside it.
	narrowedBody, plainBody := recordBody(t, f, *narrowed.RecordID), recordBody(t, f, *plain.RecordID)
	narrowedClaims, plainClaims := requestObjectClaims(t, narrowedBody.Request), requestObjectClaims(t, plainBody.Request)
	if !slices.Equal(narrowedClaims, plainClaims) {
		t.Errorf("the narrowed request object signs the claims %v and the other %v; want the same claims: a narrowing is signed into neither", narrowedClaims, plainClaims)
	}
	holdsNoNarrowing(t, "the narrowed request object", requestObjectPayload(t, narrowedBody.Request))

	// The approval record the person decides. Its request line holds that request's own signed
	// object, and its expiry is the clock at the moment it was written, so the two differ there
	// and nowhere else.
	narrowedLines, plainLines := recordLinesBut(narrowedBody, "request: ", "expires_at: "), recordLinesBut(plainBody, "request: ", "expires_at: ")
	if !slices.Equal(narrowedLines, plainLines) {
		t.Errorf("the narrowed request's record reads %v and the other's %v; want the same record but for the request it carries", narrowedLines, plainLines)
	}
	holdsNoNarrowing(t, "the narrowed request's approval record", narrowedBody.Canonical())

	narrowedGrant := approve(t, f, *narrowed.RecordID)
	plainGrant := approve(t, f, *plain.RecordID)

	// The grant row the approval wrote: the same columns, and no narrowing among their values.
	narrowedRow, plainRow := grantRow(t, f, narrowedGrant), grantRow(t, f, plainGrant)
	if narrowedColumns, plainColumns := columnsOf(t, narrowedRow), columnsOf(t, plainRow); !slices.Equal(narrowedColumns, plainColumns) {
		t.Errorf("the narrowed request's grant holds the columns %v and the other's %v; want the same row", narrowedColumns, plainColumns)
	}
	holdsNoNarrowing(t, "the narrowed request's grant row", narrowedRow)

	// Each release with no narrowing: the owner-wide installations object, a token for each
	// account the secret's github-owners tag lists, as a grant made from a request with no
	// narrowing has always answered.
	for _, release := range []struct {
		name    string
		grantID string
		secret  string
	}{
		{"the narrowed request's grant", narrowedGrant, "APP_KEY"},
		{"the grant of the request with no narrowing", plainGrant, "OTHER_APP_KEY"},
	} {
		got, err := f.Values(t.Context(), release.grantID, f.enrollmentID, nil)
		if err != nil {
			t.Fatalf("the release of %s = %v", release.name, err)
		}
		tokens := installationTokens(t, got.Values[release.secret])
		if !slices.Equal(slices.Sorted(maps.Keys(tokens)), []string{otherOwner, standInOwner}) {
			t.Fatalf("%s released tokens for %v; want one for each account the github-owners tag lists", release.name, slices.Sorted(maps.Keys(tokens)))
		}
		// A token the narrowing had named would reach that repository alone; this one reaches a
		// repository the narrowing never named, which is what owner-wide means.
		if status, body := readStandInRepository(t, f.stand, tokens[standInOwner], standInOwner, "widgets"); status != http.StatusOK {
			t.Fatalf("%s released a token that reads %s/widgets with HTTP %d (%s); want 200: a release that asks for no narrowing is owner-wide",
				release.name, standInOwner, status, body)
		}
	}
}

// recordBody is the credential-request record the approval page prints for recordID, parsed back
// from the canonical bytes the broker stored.
func recordBody(t *testing.T, f *narrowingFixture, recordID string) record.Body {
	t.Helper()
	var canonical string
	if err := f.Store.Pool.QueryRow(context.Background(), `select body from credential_requests where id=$1`, recordID).Scan(&canonical); err != nil {
		t.Fatalf("read the record %s: %v", recordID, err)
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		t.Fatalf("parse the record %s (%q): %v", recordID, canonical, err)
	}
	return body
}

// recordLinesBut is the record's canonical lines with the ones opening with each prefix left out.
func recordLinesBut(body record.Body, prefixes ...string) []string {
	var kept []string
	for line := range strings.SplitSeq(body.Canonical(), "\n") {
		if !slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(line, prefix) }) {
			kept = append(kept, line)
		}
	}
	return kept
}

// requestObjectPayload is the JSON a compact JWS signs, as its own text.
func requestObjectPayload(t *testing.T, compact string) string {
	t.Helper()
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("the request object has %d segments; want a compact JWS of three", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode the request object's payload: %v", err)
	}
	return string(payload)
}

// requestObjectClaims is the names of the claims a compact JWS signs, sorted.
func requestObjectClaims(t *testing.T, compact string) []string {
	t.Helper()
	var claims map[string]json.RawMessage
	if err := json.Unmarshal([]byte(requestObjectPayload(t, compact)), &claims); err != nil {
		t.Fatalf("the request object's payload is no JSON object: %v", err)
	}
	return slices.Sorted(maps.Keys(claims))
}

// approve decides recordID as its approver does on the broker's page, and answers the grant.
func approve(t *testing.T, f *narrowingFixture, recordID string) string {
	t.Helper()
	decision, err := f.ApplyDecision(t.Context(), recordID, true, checkOperator)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("the approver's decision on %s = %+v, %v; want a grant", recordID, decision, err)
	}
	return decision.GrantID
}

// grantRow is grantID's whole row as JSON, every column of it, which is how a test reads what a
// grant holds without naming the columns it expects.
func grantRow(t *testing.T, f *narrowingFixture, grantID string) string {
	t.Helper()
	var row string
	if err := f.Store.Pool.QueryRow(context.Background(), `select to_jsonb(g)::text from grants g where g.id=$1`, grantID).Scan(&row); err != nil {
		t.Fatalf("read the grant %s: %v", grantID, err)
	}
	return row
}

// columnsOf is the sorted field names of a row read as JSON.
func columnsOf(t *testing.T, row string) []string {
	t.Helper()
	var columns map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row), &columns); err != nil {
		t.Fatalf("the row is no JSON object: %v", err)
	}
	return slices.Sorted(maps.Keys(columns))
}

// installationTokens is a github-app release's value as the token each account's installation
// got.
func installationTokens(t *testing.T, value string) map[string]string {
	t.Helper()
	var held struct {
		Installations map[string]struct {
			InstallationID int64  `json:"installation_id"`
			Token          string `json:"token"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(value), &held); err != nil {
		t.Fatalf("the released value is no JSON object of installations: %v", err)
	}
	tokens := map[string]string{}
	for owner, installation := range held.Installations {
		if installation.Token == "" || installation.InstallationID <= 0 {
			t.Fatalf("%s's installation is %+v; want a token and the installation it was minted for", owner, installation)
		}
		tokens[owner] = installation.Token
	}
	return tokens
}

// holdsNoNarrowing fails when what is a record of something the caller sent as a narrowing.
func holdsNoNarrowing(t *testing.T, name, what string) {
	t.Helper()
	for _, word := range narrowingWordsNeverRecorded {
		if strings.Contains(what, word) {
			t.Errorf("%s says %q, which names the narrowing the request carried; want a narrowing recorded nowhere: %s", name, word, what)
		}
	}
}
