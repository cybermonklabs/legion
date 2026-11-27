package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestABrokerTokensFailedTieRefusesOnlyWhileAProjectIsPrivate: a broker-issued token's request
// ties its session to its person (session_owners, tieBrokerSession) before it reads anything.
// While a project is private the tie decides what the live agent view relays and which
// conversations a restricted member reads (access.SessionContentClause), so a tie that cannot be
// written refuses the request 503 DATABASE_UNAVAILABLE. While no project is private nothing reads
// the tie to decide anything (R9), so the failure is logged and the request served, and the
// session's next request writes the tie: the pair is held as written only once it is.
// session_owners refuses every insert by a trigger, which leaves its reads standing.
//
// Control: the refusal made whatever the deployment holds, as before, and the request with no
// private project is answered 503.
func TestABrokerTokensFailedTieRefusesOnlyWhileAProjectIsPrivate(t *testing.T) {
	seed := seedPrivateProject(t)
	ctx := context.Background()
	if _, err := seed.database.Pool.Exec(ctx, `
		create function refuse_session_owner() returns trigger language plpgsql as $$
		begin raise exception 'session_owners refuses this write'; end $$;
		create trigger refuse_session_owner before insert on session_owners for each row execute function refuse_session_owner();
	`); err != nil {
		t.Fatalf("make session_owners refuse every insert: %v", err)
	}
	tied := func(session string) bool {
		t.Helper()
		var held bool
		if err := seed.database.Pool.QueryRow(ctx, `select exists (select 1 from session_owners where session_id = $1)`, session).Scan(&held); err != nil {
			t.Fatalf("read %s's tie: %v", session, err)
		}
		return held
	}

	refused := seed.asSession(t, "session-tie-private", seedPublic, http.MethodGet, "/api/v1/whoami", nil, http.StatusServiceUnavailable)
	if code := decodeBody[refusalAnswer](t, refused).Code; code != "DATABASE_UNAVAILABLE" {
		t.Errorf("with PRIV private, a request whose tie failed is refused %s, want DATABASE_UNAVAILABLE", code)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	const session = "session-tie-public"
	served := seed.asSession(t, session, seedPublic, http.MethodGet, "/api/v1/whoami", nil, http.StatusOK)
	if who := decodeBody[whoamiAnswer](t, served); who.Session == nil || *who.Session != session {
		t.Errorf("with no private project, whoami under a token whose tie failed = %+v, want the session %s", who, session)
	}
	if tied(session) {
		t.Fatalf("%s is tied though session_owners refused the write", session)
	}
	if _, err := seed.database.Pool.Exec(ctx, `drop trigger refuse_session_owner on session_owners`); err != nil {
		t.Fatalf("let session_owners take writes again: %v", err)
	}
	seed.asSession(t, session, seedPublic, http.MethodGet, "/api/v1/whoami", nil, http.StatusOK)
	if !tied(session) {
		t.Errorf("%s's next request, with session_owners writable again, left it untied: the failed tie was held as written", session)
	}
}
