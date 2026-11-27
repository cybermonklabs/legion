// as_of_token_test.go holds Dispatch to the cut a read-only token carries (model.Credential.Cut,
// decision 6 of INFRA-4620's plan). A row of kind answers answers such a token as of its cut,
// whatever as_of the request sends, and names that cut in Dispatch-As-Of. GET /api/v1/whoami
// answers it and names the cut. Every other read and GET /ws/live answer it 403
// AS_OF_TOKEN_NOT_SERVED naming the cut, and never read its as_of. Every row outside the read
// class answers it 403 READ_ONLY_TOKEN, as it answers every read-only token.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// cutTokenAnswer is the broker's answer for the machine login's read token with a cut, as the
// golden file the broker's own test holds its answers to (ciphertokentest.NarrowedAnswers,
// machine-login-with-a-cut), bound to jkt, cut at cut and naming the person brokerTestPerson.
// But for its cut, it is the machine login's read token (machine-login) that the t-routes harness
// reads with (asOfRig.callers).
func cutTokenAnswer(t *testing.T, jkt string, cut asof.Cut) ciphertoken.Introspection {
	t.Helper()
	answer := ciphertokentest.NarrowedAnswers(t, jkt, brokerTestSession, time.Now().Add(time.Hour).Unix())["machine-login-with-a-cut"]
	if answer.AsOf == "" {
		t.Fatal("the golden machine-login-with-a-cut answer names no cut")
	}
	answer.AsOf = cut.String()
	answer.Person = brokerTestPerson
	return answer
}

// sayCutToken says token to rig's stub broker as the machine login's read token cut at cut
// (cutTokenAnswer), bound to caller's key. It holds Dispatch's credential of the token to be
// read-only, to hold person and to carry cut, so a test cannot pass for want of any of them.
func sayCutToken(t *testing.T, rig *brokerTokenRig, caller *brokerCaller, token string, cut asof.Cut) *model.Credential {
	t.Helper()
	rig.broker.Says(token, cutTokenAnswer(t, caller.jkt, cut))
	actor, _, err := rig.served.brokerTokenActor(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
	if credential := actor.Credential; err != nil || credential == nil || !credential.ReadOnly ||
		!credential.Holds(model.ScopePerson) || !credential.Cut.Equal(cut.Time()) {
		t.Fatalf("the token cut at %s: Dispatch's actor is %+v (%v), want a read-only credential holding person and carrying the cut", cut, actor, err)
	}
	return actor.Credential
}

// cutTokenRefusal is the message of the 403 AS_OF_TOKEN_NOT_SERVED that the row named name
// ("<method> <pattern>") answers a token cut at cut, written out here and not read from the
// server, so a refusal that drifted from it fails. It names the cut as whoami does (cutText).
func cutTokenRefusal(cut asof.Cut, name string) string {
	return "this token reads Dispatch as of " + cutText(cut.Time()) + "; " + name + " holds no past answer"
}

// cutTokenRefusalDifference says how response differs from the 403 AS_OF_TOKEN_NOT_SERVED that
// the row named name answers a token cut at cut, and is empty when it is that refusal: 403 with
// that code and message, and no WWW-Authenticate challenge, since no scope makes the row answer
// the token.
func cutTokenRefusalDifference(response *httptest.ResponseRecorder, cut asof.Cut, name string) string {
	refusal := gateRefusalOf(response)
	switch {
	case response.Code != http.StatusForbidden || refusal.Code != contracts.DispatchAsOfTokenNotServed:
		return "answered " + http.StatusText(response.Code) + " " + strings.TrimSpace(response.Body.String())
	case response.Header().Get("WWW-Authenticate") != "":
		return "carries the challenge " + response.Header().Get("WWW-Authenticate")
	case refusal.Error != cutTokenRefusal(cut, name):
		return fmt.Sprintf("refused with the message %q, want %q", refusal.Error, cutTokenRefusal(cut, name))
	}
	return ""
}

// databaseClock is the database's clock_timestamp(), read once.
func databaseClock(t *testing.T, rig *brokerTokenRig) time.Time {
	t.Helper()
	return databaseCut(t, rig.database).Time()
}

// malformedAsOf is an as_of that is no time: a token with no cut is refused it 400 INVALID_AS_OF
// on a row of kind answers, and a token that carries a cut never has it read.
const malformedAsOf = "yesterday"

// TestACutTokenIsAnsweredAsOfItsCutWhateverAsOfItSends reads every route of kind answers, taken
// from the routes' declarations (declaredReads), with a read-only token cut at the cut. The world
// is the t-routes harness's (seedAsOfWorld), written again after the cut (writeAfterTheCut), and
// each read is the harness's read of that route (asOfEqualityRows). For each read, the read-only
// token that holds person and no cut, sent as_of at the cut, gives the answer the cut token must
// give: 200 naming the cut in Dispatch-As-Of, or 404 for what the world holds only after the cut.
// The cut token, the same token but for its cut, sends the read four times: with no as_of, with an
// as_of an hour before the cut, before the world was written, with an as_of later than the cut,
// the database's clock after the later writes, and with an as_of that is no time (yesterday),
// which a token with no cut is refused 400. Each answer must be that answer: the same status, the
// same Dispatch-As-Of and Dispatch-As-Of-Absent, and the same body but for took_ms. A token whose
// cut lost to its as_of, whose as_of was read at all, or that was answered the present, answers
// otherwise.
func TestACutTokenIsAnsweredAsOfItsCutWhateverAsOfItSends(t *testing.T) {
	rig := newAsOfRig(t)
	world := seedAsOfWorld(t, rig.handler)
	rig.settle(t)
	cut := rig.cut(t)
	time.Sleep(5 * time.Millisecond)
	world.writeAfterTheCut(t, rig.handler)
	rig.settle(t)
	fillTextVersionsPast(t, rig.database, cut)
	later := rig.cut(t)
	earlier := asof.CutAt(cut.Time().Add(-time.Hour))

	callers := rig.callers(t)
	token := rig.cutCaller(t, cut)
	held := slices.IndexFunc(callers, func(caller asOfReader) bool { return caller.login == token.login })
	if held < 0 {
		t.Fatalf("no caller of the harness names %s, the person the cut token names", token.login)
	}
	reference := callers[held]
	t.Logf("the cut is %s; the token's as_of is none, %s, %s and %q", cut, earlier, later, malformedAsOf)

	rows := map[string]asOfRow{}
	for _, row := range asOfEqualityRows {
		rows[row.route] = row
	}
	answers := 0
	reads := declaredReads()
	for _, name := range slices.Sorted(maps.Keys(reads)) {
		if reads[name].AsOf != asOfAnswers {
			continue
		}
		answers++
		row, held := rows[name]
		if !held {
			t.Errorf("%s answers as_of, and the harness holds no read of it", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, read := range row.reads(world) {
				want := rig.read(t, reference, withAsOf(t, read.target, cut), false)
				if problem := answeredAsOfTheCut(read, want, cut); problem != "" {
					t.Fatalf("%s as %s, %s with as_of at the cut: %s", name, reference.name, read.target, problem)
				}
				for _, sent := range []struct{ name, target string }{
					{"no as_of", read.target},
					{"an as_of before the cut", withAsOf(t, read.target, earlier)},
					{"an as_of after the cut", withAsOf(t, read.target, later)},
					{"an as_of that is no time", withAsOfValue(t, read.target, malformedAsOf)},
				} {
					got := rig.read(t, token, sent.target, false)
					if difference := cutTokenAnswerDifference(want, got); difference != "" {
						t.Errorf("%s as %s with %s (%s): %s", name, token.name, sent.name, sent.target, difference)
					}
				}
			}
		})
	}
	if answers == 0 {
		t.Fatal("no route's declaration states kind asOfAnswers, so the token was sent to none")
	}
}

// answeredAsOfTheCut says how want, a read with as_of at cut, fails to be an answer a cut token
// can be held to, and is empty when it is one: 404 for the document removed after the cut and
// for what only the writes after the cut made, and otherwise 200 naming the cut in
// Dispatch-As-Of.
func answeredAsOfTheCut(read asOfRead, want asOfAnswer, cut asof.Cut) string {
	switch {
	case read.removed || read.missing:
		if want.status != http.StatusNotFound {
			return fmt.Sprintf("answered %d %s, want 404", want.status, abbreviateAnswer(want.body))
		}
	case want.status != http.StatusOK:
		return fmt.Sprintf("answered %d %s, want 200", want.status, abbreviateAnswer(want.body))
	case want.header.Get(contracts.DispatchAsOfHeader) != cut.String():
		return fmt.Sprintf("its %s header is %q, want the cut %s", contracts.DispatchAsOfHeader, want.header.Get(contracts.DispatchAsOfHeader), cut)
	}
	return ""
}

// cutTokenAnswerDifference says how got, the cut token's answer, differs from want, the same read
// with as_of at the cut by the same token with no cut, and is empty when it is that answer: the
// same status, the same Dispatch-As-Of and Dispatch-As-Of-Absent, and the same body but for
// took_ms, the request's own time.
func cutTokenAnswerDifference(want, got asOfAnswer) string {
	if got.status != want.status {
		return fmt.Sprintf("answered %d %s, and as_of at the cut answered %d", got.status, abbreviateAnswer(got.body), want.status)
	}
	for _, header := range []string{contracts.DispatchAsOfHeader, contracts.DispatchAsOfAbsentHeader} {
		if gotValue, wantValue := got.header.Get(header), want.header.Get(header); gotValue != wantValue {
			return fmt.Sprintf("its %s header is %q, and as_of at the cut's is %q", header, gotValue, wantValue)
		}
	}
	wantDoc, wantErr := decodeAsOfBody(want.body)
	gotDoc, gotErr := decodeAsOfBody(got.body)
	if wantErr != nil || gotErr != nil {
		if string(got.body) != string(want.body) {
			return fmt.Sprintf("its body is %s, and as_of at the cut's is %s", abbreviateAnswer(got.body), abbreviateAnswer(want.body))
		}
		return ""
	}
	dropMember(wantDoc, []string{"took_ms"})
	dropMember(gotDoc, []string{"took_ms"})
	if difference, found := firstDifference("", wantDoc, gotDoc); found {
		return difference
	}
	return ""
}

// TestEveryReadRowThatHoldsNoPastAnswerRefusesACutToken builds every row's own chain as register
// does (routeHandler), test hooks in, and sends each its probe body and a valid Idempotency-Key
// under a read-only token cut an hour before the database's clock. The rows are read off the
// table, not listed, and their kinds off their declarations (declared), not off the row
// (apiRoute.Read). The walk runs four times: with no as_of, with an as_of half an hour after the
// token's cut, with an as_of an hour after the database's clock, and with an as_of that is no
// time (yesterday). A token with no cut is refused each of the last two 400 INVALID_AS_OF on a
// row of kind answers. On every pass:
//
//   - a row of kind answers runs its handler, replaced by a recorder, once under the token's cut,
//     and its answer names that cut in Dispatch-As-Of;
//   - GET /api/v1/whoami runs its own handler once, with no cut on the context, and answers 200
//     naming the token's cut;
//   - every other read answers 403 AS_OF_TOKEN_NOT_SERVED naming the cut and the row, and never
//     a 400, and its handler does not run;
//   - every row outside the read class, every write among them, answers 403 READ_ONLY_TOKEN, and
//     its handler does not run.
//
// A row is counted once its assertions have run, and the count is held to the rows the
// registered server mounts, so a walk that skipped a row fails.
func TestEveryReadRowThatHoldsNoPastAnswerRefusesACutToken(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	clock := databaseClock(t, rig)
	cut := asof.CutAt(clock.Add(-time.Hour))
	const token = "a-cut-token-walking-the-table"
	credential := sayCutToken(t, rig, caller, token, cut)
	table := readOnlyTable()
	replays := rig.served.keyedReplays()
	mounted := len(rig.served.routeIndex)

	for _, pass := range []struct{ name, asOf string }{
		{name: "no as_of"},
		{name: "an as_of after the cut", asOf: cut.Time().Add(30 * time.Minute).Format(time.RFC3339Nano)},
		{name: "an as_of after the database's now", asOf: clock.Add(time.Hour).Format(time.RFC3339Nano)},
		{name: "an as_of that is no time", asOf: malformedAsOf},
	} {
		t.Run(pass.name, func(t *testing.T) {
			query := ""
			if pass.asOf != "" {
				query = "?" + url.Values{asOfParameter: {pass.asOf}}.Encode()
			}
			walked, answered, refused, readOnly, scopes := 0, 0, 0, 0, 0
			for _, route := range table {
				name := route.Method + " " + route.Pattern
				entry := declared(name).Read
				recorder := &cutRecorder{}
				route.Handler = recorder.handler
				if name == asOfCallerRow {
					route.Handler = func(w http.ResponseWriter, r *http.Request) {
						recorder.runs = append(recorder.runs, "")
						if seen, carried := asof.CutFrom(r.Context()); carried {
							recorder.runs[len(recorder.runs)-1] = seen.String()
						}
						rig.served.whoami(w, r)
					}
				}
				request := caller.presenting(t, route.Method, readOnlyProbePath(route.Pattern)+query, token, json.RawMessage(routeProbeBody(route)))
				request.Header.Set(contracts.IdempotencyKeyHeader, readOnlyProbeKey)
				response := httptest.NewRecorder()
				rig.served.routeHandler(route, replays[name]).ServeHTTP(response, request)

				missingScope := ""
				for _, scope := range route.OAuthScopes {
					if missingScope == "" && !credential.Holds(scope) {
						missingScope = scope
					}
				}
				switch {
				case route.Method != http.MethodGet || entry.Writes != readWritesNothing:
					if len(recorder.runs) != 0 {
						t.Errorf("%s is no read, and its handler ran %d times, want none", name, len(recorder.runs))
					}
					if why := notTheGatesRefusal(response, contracts.DispatchReadOnlyToken, ""); why != "" {
						t.Errorf("%s %s, want 403 %s", name, why, contracts.DispatchReadOnlyToken)
					}
					readOnly++
				case entry.AsOf == asOfAnswers && missingScope != "":
					if refusal := gateRefusalOf(response); len(recorder.runs) != 0 || response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired {
						t.Errorf("%s ran its handler %d times and answered %d %s, want its scopes' 403 %s (%s) and no handler",
							name, len(recorder.runs), response.Code, strings.TrimSpace(response.Body.String()), codeScopeRequired, missingScope)
					}
					scopes++
				case entry.AsOf == asOfAnswers:
					if response.Code != http.StatusNoContent || !slices.Equal(recorder.runs, []string{cut.String()}) {
						t.Errorf("%s answers as_of, and answered %d %s with its handler's runs seeing the cuts %q, want one run under the token's cut %s",
							name, response.Code, strings.TrimSpace(response.Body.String()), recorder.runs, cut)
					}
					if named := response.Header().Get(contracts.DispatchAsOfHeader); named != cut.String() {
						t.Errorf("%s answers as_of, and its %s header is %q, want the token's cut %s", name, contracts.DispatchAsOfHeader, named, cut)
					}
					answered++
				case name == asOfCallerRow:
					var answer struct {
						Cut *string `json:"cut"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || response.Code != http.StatusOK ||
						answer.Cut == nil || *answer.Cut != cutText(cut.Time()) || !slices.Equal(recorder.runs, []string{""}) {
						t.Errorf("%s answered %d %s with its handler's runs seeing the cuts %q, want one run with no cut and 200 naming the cut %s",
							name, response.Code, strings.TrimSpace(response.Body.String()), recorder.runs, cutText(cut.Time()))
					}
					answered++
				default:
					if len(recorder.runs) != 0 {
						t.Errorf("%s holds no past answer, and its handler ran %d times, want none", name, len(recorder.runs))
					}
					if why := cutTokenRefusalDifference(response, cut, name); why != "" {
						t.Errorf("%s %s, want 403 %s %q", name, why, contracts.DispatchAsOfTokenNotServed, cutTokenRefusal(cut, name))
					}
					refused++
				}
				walked++
			}
			if walked == 0 || walked != mounted {
				t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
			}
			t.Logf("walked the table's %d rows (test hooks in): %d answered at the token's cut %s or named it, %d refused %s, %d refused %s, %d refused by their scopes",
				walked, answered, cut, refused, contracts.DispatchAsOfTokenNotServed, readOnly, contracts.DispatchReadOnlyToken, scopes)
		})
	}
}

// TestTheLiveSocketRefusesACutTokenNamingTheCut: GET /ws/live under a read-only token that
// carries a cut is 403 AS_OF_TOKEN_NOT_SERVED naming the cut, not READ_ONLY_TOKEN, and no socket
// opens: a socket delivers the present as it changes, so it holds no past answer. The token is
// sent through the socket's own chain (liveSocketHandler), with the socket replaced by a counter,
// which shows the socket never opens, and through the mounted server, which shows the refusal is
// what a client is answered. The control is the same token with no cut, the machine login's read
// token: the same chain answers it 403 READ_ONLY_TOKEN, so the cut decides the code.
func TestTheLiveSocketRefusesACutTokenNamingTheCut(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	cut := asof.CutAt(databaseClock(t, rig).Add(-time.Hour))
	const token = "a-cut-token-at-the-live-socket"
	sayCutToken(t, rig, caller, token, cut)
	opened := 0
	live := rig.served.liveSocketHandler(func(w http.ResponseWriter, _ *http.Request) {
		opened++
		w.WriteHeader(http.StatusNoContent)
	})
	name := http.MethodGet + " " + liveSocketPattern

	response := httptest.NewRecorder()
	live.ServeHTTP(response, caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil))
	if opened != 0 {
		t.Fatalf("GET %s under a token cut at %s opened the socket %d times, want none", liveSocketPattern, cut, opened)
	}
	if why := cutTokenRefusalDifference(response, cut, name); why != "" {
		t.Errorf("GET %s under a token cut at %s %s, want 403 %s %q", liveSocketPattern, cut, why, contracts.DispatchAsOfTokenNotServed, cutTokenRefusal(cut, name))
	}
	if why := cutTokenRefusalDifference(rig.serve(caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil)), cut, name); why != "" {
		t.Errorf("GET %s on the mounted server under a token cut at %s %s, want 403 %s %q", liveSocketPattern, cut, why, contracts.DispatchAsOfTokenNotServed, cutTokenRefusal(cut, name))
	}

	const uncut = "the-same-token-with-no-cut-at-the-live-socket"
	control := ciphertokentest.NarrowedAnswers(t, caller.jkt, brokerTestSession, time.Now().Add(time.Hour).Unix())["machine-login"]
	control.Person = brokerTestPerson
	if control.AsOf != "" {
		t.Fatalf("the golden machine-login answer names the cut %q, want none", control.AsOf)
	}
	rig.broker.Says(uncut, control)
	response = httptest.NewRecorder()
	live.ServeHTTP(response, caller.presenting(t, http.MethodGet, liveSocketPattern, uncut, nil))
	if opened != 0 {
		t.Fatalf("GET %s under the token with no cut opened the socket %d times, want none", liveSocketPattern, opened)
	}
	if why := notTheGatesRefusal(response, contracts.DispatchReadOnlyToken, ""); why != "" {
		t.Errorf("GET %s under the token with no cut %s, want 403 %s", liveSocketPattern, why, contracts.DispatchReadOnlyToken)
	}
}
