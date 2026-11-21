// packages/envoy/internal/cipher/requests/project_visibility_test.go
// The side review of cybermonklabs/legion#4135 (review 5927943331): a visibility Dispatch did not
// answer as "public" or "private" releases nothing, and an approval the project rule turns into a
// denial is recorded as the request's denial.
package requests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// visibilityRewrite is an http.RoundTripper in front of the Dispatch stand-in that rewrites
// project's answer: with remove, the visibility field is left out; otherwise it reads to.
type visibilityRewrite struct {
	project, to string
	remove      bool
}

func (f visibilityRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || res.StatusCode != http.StatusOK || !strings.HasSuffix(r.URL.Path, "/projects/"+f.project) {
		return res, err
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		return nil, err
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}
	if f.remove {
		delete(answer, "visibility")
	} else {
		answer["visibility"] = f.to
	}
	out, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	res.Body, res.ContentLength = io.NopCloser(bytes.NewReader(out)), int64(len(out))
	res.Header.Del("Content-Length")
	return res, nil
}

// visibilityFront is a projectaccess.Access whose States answers project with visibility, and
// passes every other question on: what a client that kept any visibility it was answered would
// hand the request machine.
type visibilityFront struct {
	inner               projectaccess.Access
	project, visibility string
}

func (f visibilityFront) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return f.inner.Access(ctx, project, person)
}

func (f visibilityFront) MemberKind(ctx context.Context, person string) (string, error) {
	return f.inner.MemberKind(ctx, person)
}

func (f visibilityFront) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	states, err := f.inner.States(ctx, projects)
	if err != nil {
		return states, err
	}
	if answer, held := states[f.project]; held {
		answer.Visibility = f.visibility
		states[f.project] = answer
	}
	return states, nil
}

// TestAVisibilityDispatchDoesNotKnowReleasesNothing is the side review's finding 1: a tagged
// secret's release reads only "public" as public. TEAM_TOKEN is granted while TEAM is public;
// Dispatch then answers TEAM with no visibility, or with a word that is neither "public" nor
// "private". The release is refused as ErrProjectAccessUnavailable, the value stays in the
// broker, and the grant stays live: once Dispatch answers "public" again it releases as before.
// The first two rows run the real client, which refuses such an answer itself
// (projectaccess.ErrUnknownVisibility); the last two hand the request machine the answer as a
// client that kept it would, so the machine's own reading is held to the rule too.
func TestAVisibilityDispatchDoesNotKnowReleasesNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		access func(dispatch *projectaccesstest.Server) projectaccess.Access
	}{
		{"Dispatch's answer carries no visibility", func(d *projectaccesstest.Server) projectaccess.Access {
			return projectaccess.New(d.URL, d.Account, &http.Client{Transport: visibilityRewrite{project: teamProject, remove: true}})
		}},
		{"Dispatch's answer carries a visibility of another word", func(d *projectaccesstest.Server) projectaccess.Access {
			return projectaccess.New(d.URL, d.Account, &http.Client{Transport: visibilityRewrite{project: teamProject, to: "internal"}})
		}},
		{"the machine is handed an empty visibility", func(d *projectaccesstest.Server) projectaccess.Access {
			return visibilityFront{inner: projectaccess.New(d.URL, d.Account, nil), project: teamProject, visibility: ""}
		}},
		{"the machine is handed a visibility of another word", func(d *projectaccesstest.Server) projectaccess.Access {
			return visibilityFront{inner: projectaccess.New(d.URL, d.Account, nil), project: teamProject, visibility: "internal"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, dispatch := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
			_, grantID := grantOf(t, m, enr, key, "read it", "TEAM_TOKEN")
			m.Projects = c.access(dispatch)
			values, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{})
			if !errors.Is(err, ErrProjectAccessUnavailable) {
				t.Fatalf("Values of TEAM_TOKEN with TEAM's visibility unread = %+v, %v; want ErrProjectAccessUnavailable, never a release read as public", values, err)
			}
			if _, released := values.Values["TEAM_TOKEN"]; released {
				t.Fatalf("TEAM_TOKEN was released with TEAM's visibility unread: %+v", values)
			}
			answerFrom(t, m, dispatchRows(publicTeam))
			if again, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); err != nil || again.Values["TEAM_TOKEN"] != "team-v1" {
				t.Fatalf("Values once Dispatch answers TEAM public again = %+v, %v; want team-v1: an unread answer ends no grant", again, err)
			}
		})
	}
}

// TestAnApprovalOfASecretWhoseProjectTurnedPrivateIsRecordedAsADenial is the side review's
// finding 2 (currentPolicyAdmits' projectDeny): PRIV_HUMAN, which anyone may approve, is asked for
// while PRIV is public by a session naming no project; PRIV then turns private, and carol, on
// PRIV's access list, approves. The project rule no longer gives the session the secret, and no
// approver could give it back, so the approval is recorded as the request's own denial by the
// policy, with no grant, as a requesters tag that drops the session is. (The review named
// PRIV_KEY, which is agent tier and so is granted at once and never pending; PRIV_HUMAN is the
// human-tier secret of the same project.)
func TestAnApprovalOfASecretWhoseProjectTurnedPrivateIsRecordedAsADenial(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newProjectFixture(t, proxyrules.Rules{}, publicTeam)
	publicPriv := dispatchRows(publicTeam)
	publicPriv[privProject] = projectaccesstest.Row{Visibility: "public", Owner: otherPerson}
	answerFrom(t, m, publicPriv)
	req, err := m.Create(ctx, enr, signRequest(t, m, key, "read it", "PRIV_HUMAN"), "", nil)
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(PRIV_HUMAN) while PRIV is public = %+v, %v; want it pending for anyone to decide", req, err)
	}
	answerFrom(t, m, dispatchRows(publicTeam))

	decision, err := m.ApplyDecision(ctx, *req.RecordID, true, insider)
	const want = "PRIV_HUMAN: " + policy.ReasonProjectNotNamed
	if err != nil || decision.State != "denied" || decision.GrantID != "" || decision.Detail != want {
		t.Fatalf("%s's approval after PRIV turned private = %+v, %v; want it denied with no grant and the detail %q", insider, decision, err, want)
	}
	var state, decidedBy, detail string
	var grants int
	if err := m.Store.Pool.QueryRow(ctx, `select r.state, coalesce(r.decided_by,''), coalesce(r.decision_detail,''),
		(select count(*) from grants g where g.request_id=r.id) from requests r where r.id=$1`, req.ID).
		Scan(&state, &decidedBy, &detail, &grants); err != nil {
		t.Fatalf("read the request row: %v", err)
	}
	if state != "denied" || decidedBy != PolicyDeniedActor || detail != want || grants != 0 {
		t.Fatalf("the request row = state %q decided_by %q detail %q with %d grants; want it denied by %s, %q, and no grant",
			state, decidedBy, detail, grants, PolicyDeniedActor, want)
	}
	var event, login, actor, eventDetail string
	if err := m.Store.Pool.QueryRow(ctx, `select event, coalesce(login,''), actor, coalesce(detail,'')
		from credential_request_events where record_id=$1`, *req.RecordID).Scan(&event, &login, &actor, &eventDetail); err != nil {
		t.Fatalf("read the record's event: %v", err)
	}
	if event != "denied" || login != insider || actor != PolicyDeniedActor || eventDetail != want {
		t.Fatalf("the record's event = %s by login %q, actor %q, detail %q; want denied, login %s, actor %s, detail %q",
			event, login, actor, eventDetail, insider, PolicyDeniedActor, want)
	}
}
