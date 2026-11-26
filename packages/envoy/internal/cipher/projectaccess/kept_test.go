package projectaccess_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The people the kept-kind rows read: ada and carol full members, bob a restricted one.
const (
	keptAda   = "ada@example.com"
	keptBob   = "bob@example.com"
	keptCarol = "carol@example.com"
	keptDave  = "dave@example.com"
)

// keepFailedLine opens the WARN line a client logs for an answer it could not keep.
const keepFailedLine = "broker: Dispatch's answer for a person could not be kept"

// keptKinds is what the stand-in Dispatch of these rows answers: ada and carol full, bob
// restricted.
func keptKinds() map[string]string {
	return map[string]string{keptAda: projectaccess.MemberFull, keptBob: projectaccess.MemberRestricted, keptCarol: projectaccess.MemberFull}
}

// keptRig is one broker process's Dispatch client, keeping its answers in a store (Keep): the
// clock it reads, the lines it logs and the registry it counts on.
type keptRig struct {
	client   *projectaccess.Client
	clock    *fakeClock
	logs     *logtest.Recorder
	registry *metrics.Registry
}

// newKeptRig is a client of the Dispatch at url, presenting account, keeping in keeper.
func newKeptRig(url string, account projectaccesstest.StaticAccount, keeper projectaccess.Keeper) *keptRig {
	r := &keptRig{
		client:   projectaccess.New(url, account, nil),
		clock:    &fakeClock{now: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)},
		logs:     &logtest.Recorder{},
		registry: metrics.New(),
	}
	r.client.UseClock(r.clock.read)
	r.client.Keep(keeper, slog.New(r.logs), r.registry)
	return r
}

// keepingClientOf is newKeptRig for the stand-in server.
func keepingClientOf(server *projectaccesstest.Server, keeper projectaccess.Keeper) *keptRig {
	return newKeptRig(server.URL, server.Account, keeper)
}

// unchecked is the broker_member_kind_unchecked_total sample labelled kept, 0 when there is none.
func (r *keptRig) unchecked(t *testing.T, kept string) int {
	t.Helper()
	return r.sample(t, "broker_member_kind_unchecked_total", kept)
}

// sample is metric's sample labelled kept on r's registry, 0 when there is none.
func (r *keptRig) sample(t *testing.T, metric, kept string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	series := metric + `{kept="` + kept + `"} `
	for sample := range strings.Lines(recorder.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(sample), series); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("the sample %q holds no count", sample)
			}
			return n
		}
	}
	return 0
}

// warned is the attributes of every line r logged at WARN whose message opens with message and
// whose person is person.
func (r *keptRig) warned(message, person string) []map[string]string {
	var found []map[string]string
	for _, record := range r.logs.Records() {
		attrs := map[string]string{}
		record.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		if record.Level == slog.LevelWarn && strings.HasPrefix(record.Message, message) && attrs["person"] == person {
			found = append(found, attrs)
		}
	}
	return found
}

// kindOf fails t unless r's client answers person kind with no error.
func (r *keptRig) kindOf(t *testing.T, person, kind, when string) {
	t.Helper()
	if got, err := r.client.MemberKind(t.Context(), person); err != nil || got != kind {
		t.Fatalf("MemberKind(%s) %s = %q, %v; want %s", person, when, got, err, kind)
	}
}

// refused fails t unless r's client refuses person's member-kind read as it refuses one Dispatch
// did not answer with no Keeper: the read's failure, which is Unanswered, and the
// *MemberKindUncheckable Restricted answers in uncheckableReason's words for it.
func (r *keptRig) refused(t *testing.T, person, when string) {
	t.Helper()
	if kind, err := r.client.MemberKind(t.Context(), person); err == nil || kind != "" || !projectaccess.Unanswered(err) {
		t.Fatalf("MemberKind(%s) %s = %q, %v; want the failure of a read Dispatch did not answer", person, when, kind, err)
	}
	_, err := projectaccess.Restricted(t.Context(), r.client, person)
	var uncheckable *projectaccess.MemberKindUncheckable
	if !errors.As(err, &uncheckable) || uncheckable.Person != person ||
		err.Error() != "whether Dispatch calls "+person+" a restricted member cannot be read: the broker could not ask Dispatch" {
		t.Fatalf("Restricted(%s) %s = %v; want the MemberKindUncheckable a read Dispatch did not answer is", person, when, err)
	}
}

// keptKind fails t unless st keeps kind for person; it answers the as_of the kept answer rests on.
func keptKind(t *testing.T, st *store.Store, person, kind, after string) time.Time {
	t.Helper()
	got, asOf, found, err := st.KeptMemberKind(t.Context(), person)
	if err != nil || !found || got != kind {
		t.Fatalf("KeptMemberKind(%s) after %s = %q (found %t), %v; want %q", person, after, got, found, err, kind)
	}
	return asOf
}

// openStore is a second pool on the schema url names, closed when t ends: a second broker
// process's, or a restarted one's.
func openStore(t *testing.T, url string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestAMemberKindReadDispatchDoesNotAnswerIsAnsweredFromTheKeptKind: a client that keeps its
// answers (Keep) writes each person answer it reads, and once Dispatch stops answering it reads
// the member kind it kept. ada, last answered full, is answered full, and ListAllowed lets her
// empty list through as a full member's, with one WARN line (KeptFullMessage) and one count
// labelled full a minute however often she is read, and a second of each a minute later. bob, last
// answered restricted, carol, of whom nothing is kept, and dave, whose kept kind the database
// cannot read, are each refused as a client with no keeper refuses them: the read's failure, the
// *MemberKindUncheckable Restricted answers for it in the same words, and ListAllowed's
// ErrUnchecked, counted once a minute under restricted, none and unreadable, with no WARN line.
//
// Control: MemberKind returning the failure without reading the kept kind (no keptKind); ada's
// first read while Dispatch is down fails.
func TestAMemberKindReadDispatchDoesNotAnswerIsAnsweredFromTheKeptKind(t *testing.T) {
	url := storetest.URL(t)
	st := openStore(t, url)
	dispatch := projectaccesstest.NewServer(t, nil, keptKinds())
	r := keepingClientOf(dispatch, st)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "while Dispatch answers")
	r.kindOf(t, keptBob, projectaccess.MemberRestricted, "while Dispatch answers")
	keptKind(t, st, keptAda, projectaccess.MemberFull, "the read of ada")
	keptKind(t, st, keptBob, projectaccess.MemberRestricted, "the read of bob")
	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)

	for range 3 {
		r.kindOf(t, keptAda, projectaccess.MemberFull, "while Dispatch is down, kept full")
	}
	if restricted, err := projectaccess.Restricted(t.Context(), r.client, keptAda); err != nil || restricted {
		t.Fatalf("Restricted(%s) while Dispatch is down, kept full = %t, %v; want false", keptAda, restricted, err)
	}
	if err := projectaccess.ListAllowed(t.Context(), r.client, keptAda, nil); err != nil {
		t.Fatalf("ListAllowed(%s, no projects) while Dispatch is down, kept full = %v; want nil, a full member's", keptAda, err)
	}
	if warned := r.warned(projectaccess.KeptFullMessage, keptAda); len(warned) != 1 || warned[0]["error"] == "" || warned[0]["as_of"] == "" {
		t.Fatalf("serving %s five times in a minute logged %v; want one WARN %q with as_of and the error", keptAda, warned, projectaccess.KeptFullMessage)
	}
	if got := r.unchecked(t, "full"); got != 1 {
		t.Fatalf(`broker_member_kind_unchecked_total{kept="full"} after five reads of %s in a minute = %d; want 1`, keptAda, got)
	}

	for _, refused := range []struct{ person, kept string }{{keptBob, "restricted"}, {keptCarol, "none"}} {
		for range 2 {
			r.refused(t, refused.person, "while Dispatch is down, kept "+refused.kept)
		}
		if err := projectaccess.ListAllowed(t.Context(), r.client, refused.person, nil); !errors.Is(err, projectaccess.ErrUnchecked) {
			t.Fatalf("ListAllowed(%s, no projects) while Dispatch is down, kept %s = %v; want ErrUnchecked, as with no keeper", refused.person, refused.kept, err)
		}
		if got := r.unchecked(t, refused.kept); got != 1 {
			t.Fatalf(`broker_member_kind_unchecked_total{kept=%q} after reading %s five times in a minute = %d; want 1`, refused.kept, refused.person, got)
		}
		if warned := r.warned(projectaccess.KeptFullMessage, refused.person); len(warned) != 0 {
			t.Fatalf("refusing %s logged %v; want no line saying they were served", refused.person, warned)
		}
	}

	r.clock.advance(time.Minute)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "a minute later")
	if warned, counted := len(r.warned(projectaccess.KeptFullMessage, keptAda)), r.unchecked(t, "full"); warned != 2 || counted != 2 {
		t.Fatalf("serving %s again a minute later left %d WARN lines and a count of %d; want 2 of each", keptAda, warned, counted)
	}

	gone := openStore(t, url)
	gone.Close()
	unreadable := keepingClientOf(dispatch, gone)
	unreadable.refused(t, keptDave, "while Dispatch is down and the store is closed")
	if got := unreadable.unchecked(t, "unreadable"); got != 1 {
		t.Fatalf(`broker_member_kind_unchecked_total{kept="unreadable"} with the store closed = %d; want 1`, got)
	}
}

// TestTheKeptKindSurvivesARestart: a client of a second broker process over the same database,
// or of this one restarted, whose Dispatch is down from its start, answers as the first: ada,
// whom the first process last read full, full, and bob, whom it last read restricted, refused.
//
// Control: a client with no keeper refuses ada, so the outage is real for a process that keeps
// nothing in memory. The fault: keptKind reading the stale answer the client's own cache holds
// rather than the Keeper; the first process still answers ada full, and this second one refuses
// her.
func TestTheKeptKindSurvivesARestart(t *testing.T) {
	url := storetest.URL(t)
	dispatch := projectaccesstest.NewServer(t, nil, keptKinds())
	first := keepingClientOf(dispatch, openStore(t, url))
	first.kindOf(t, keptAda, projectaccess.MemberFull, "at the first process")
	first.kindOf(t, keptBob, projectaccess.MemberRestricted, "at the first process")
	dispatch.SetDown(true)

	bare := projectaccess.New(dispatch.URL, dispatch.Account, nil)
	if kind, err := bare.MemberKind(t.Context(), keptAda); err == nil {
		t.Fatalf("MemberKind(%s) at a client with no keeper while Dispatch is down = %q; want a failure: the outage is not real", keptAda, kind)
	}
	second := keepingClientOf(dispatch, openStore(t, url))
	second.kindOf(t, keptAda, projectaccess.MemberFull, "at a second process, Dispatch down from its start")
	second.refused(t, keptBob, "at a second process, Dispatch down from its start")
	if full, restricted := second.unchecked(t, "full"), second.unchecked(t, "restricted"); full != 1 || restricted != 1 {
		t.Fatalf("the second process counted %d full and %d restricted; want 1 of each", full, restricted)
	}
}

// TestADemotionDispatchAnswersIsKeptAndRefusedOnceItIsDown: ada is kept full; Dispatch then
// answers her restricted, at a later as_of, and that answer is written over the full one; once
// Dispatch stops answering she is refused, counted restricted, and not served on the earlier full.
//
// The fault: KeepMemberKind's update leaving member_kind out of what it sets; the demotion's
// as_of is written with the full kind kept, and her read once Dispatch is down answers full.
func TestADemotionDispatchAnswersIsKeptAndRefusedOnceItIsDown(t *testing.T) {
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, keptKinds())
	r := keepingClientOf(dispatch, st)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "before the demotion")
	fullAsOf := keptKind(t, st, keptAda, projectaccess.MemberFull, "the read before the demotion")

	dispatch.SetMemberKind(keptAda, projectaccess.MemberRestricted)
	r.clock.advance(projectaccess.CacheTTL)
	r.kindOf(t, keptAda, projectaccess.MemberRestricted, "once Dispatch answers the demotion")
	if demotedAsOf := keptKind(t, st, keptAda, projectaccess.MemberRestricted, "the demotion's read"); !demotedAsOf.After(fullAsOf) {
		t.Fatalf("the demotion is kept as of %v; want later than the full answer's %v", demotedAsOf, fullAsOf)
	}

	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	r.refused(t, keptAda, "once Dispatch is down after the demotion")
	if full, restricted := r.unchecked(t, "full"), r.unchecked(t, "restricted"); full != 0 || restricted != 1 {
		t.Fatalf("the reads after the demotion counted %d full and %d restricted; want 0 and 1", full, restricted)
	}
}

// personDispatch is a Dispatch whose person route answers every question with one answer naming
// kind, not held, as of asOf, until t ends.
func personDispatch(t *testing.T, kind string, asOf time.Time) string {
	t.Helper()
	body := `{"person":"` + keptAda + `","member_kind":"` + kind + `","held":false,"as_of":"` + asOf.Format(time.RFC3339Nano) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// TestAnOlderAnswerDoesNotReplaceTheKeptKind: two broker processes read ada, one an answer naming
// her restricted as of t2 and one, written after it, an older answer naming her full as of t1.
// Each answers its own read as Dispatch answered it, and the database keeps the later answer,
// restricted, so a process whose Dispatch is down refuses her.
//
// The fault: KeepMemberKind writing every answer whatever the row holds (its where clause gone);
// the older full answer replaces the newer restricted one, and the down process serves her.
func TestAnOlderAnswerDoesNotReplaceTheKeptKind(t *testing.T) {
	url := storetest.URL(t)
	t1 := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	newer := newKeptRig(personDispatch(t, projectaccess.MemberRestricted, t2), "token", openStore(t, url))
	newer.kindOf(t, keptAda, projectaccess.MemberRestricted, "from the answer as of t2")
	older := newKeptRig(personDispatch(t, projectaccess.MemberFull, t1), "token", openStore(t, url))
	older.kindOf(t, keptAda, projectaccess.MemberFull, "from the answer as of t1, read after it")
	st := openStore(t, url)
	if asOf := keptKind(t, st, keptAda, projectaccess.MemberRestricted, "the older answer's write"); !asOf.Equal(t2) {
		t.Fatalf("the kept answer rests on %v; want t2 %v", asOf, t2)
	}
	down := projectaccesstest.NewServer(t, nil, nil)
	down.SetDown(true)
	keepingClientOf(down, st).refused(t, keptAda, "at a process whose Dispatch is down")
}

// refuseKeeps makes person_member_kind refuse every insert and update until the returned func
// runs, as a database that takes reads and no writes.
func refuseKeeps(t *testing.T, st *store.Store) (allow func()) {
	t.Helper()
	for _, statement := range []string{
		`create or replace function refuse_person_member_kind_writes() returns trigger language plpgsql as $$
			begin raise exception 'this test refuses every write to person_member_kind'; end $$`,
		`create trigger person_member_kind_refused before insert or update on person_member_kind
			for each row execute function refuse_person_member_kind_writes()`,
	} {
		if _, err := st.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		t.Helper()
		if _, err := st.Pool.Exec(context.Background(), `drop trigger person_member_kind_refused on person_member_kind`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestADemotionNoWriteKeptStandsOverTheKeptFull: ada is kept full; the database then refuses
// writes, and Dispatch answers her restricted. The read answers restricted, as Dispatch did, and
// logs that the answer could not be kept; once Dispatch stops answering, the client refuses her on
// that answer rather than serving her on the older full the database still keeps. Once a write
// keeps a later answer, the client forgets the one it could not keep: a promotion back to full,
// kept, serves her again while Dispatch is down.
//
// The fault: keepPerson remembering no answer it could not keep; her read once Dispatch is down
// answers full.
func TestADemotionNoWriteKeptStandsOverTheKeptFull(t *testing.T) {
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, keptKinds())
	r := keepingClientOf(dispatch, st)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "before the demotion")
	allow := refuseKeeps(t, st)
	dispatch.SetMemberKind(keptAda, projectaccess.MemberRestricted)
	r.clock.advance(projectaccess.CacheTTL)
	r.kindOf(t, keptAda, projectaccess.MemberRestricted, "once Dispatch answers the demotion the database refuses")
	if lines := r.warned(keepFailedLine, keptAda); len(lines) != 1 || lines[0]["member_kind"] != projectaccess.MemberRestricted || lines[0]["error"] == "" {
		t.Fatalf("the demotion's refused write logged %v; want one WARN %q naming the kind and the error", lines, keepFailedLine)
	}
	keptKind(t, st, keptAda, projectaccess.MemberFull, "the refused write of the demotion")

	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	r.refused(t, keptAda, "while Dispatch is down after a demotion no write kept")
	if full, restricted := r.unchecked(t, "full"), r.unchecked(t, "restricted"); full != 0 || restricted != 1 {
		t.Fatalf("the reads after the unkept demotion counted %d full and %d restricted; want 0 and 1", full, restricted)
	}

	allow()
	dispatch.SetDown(false)
	dispatch.SetMemberKind(keptAda, projectaccess.MemberFull)
	r.clock.advance(projectaccess.CacheTTL)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "once Dispatch answers her full again and the write is kept")
	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "while Dispatch is down after the kept promotion")
}

// TestADemotionAnsweredWithHeldLeftOutIsKeptAndRefusedOnceItIsDown: ada is kept full as of t1.
// Dispatch's person answer then leaves held out, as Dispatch in this tree leaves it out of every
// answer (access.PersonAnswer), while it names her restricted as of t2. The read answers
// restricted, and that answer's kind is kept as any other's, so once Dispatch is down she is
// refused, at this process and at a second one over the same database, which never read the
// demotion.
//
// The fault: keepPerson writing no answer whose held Dispatch left out; this process serves her
// full. Remembering such an answer in this process alone; the second process serves her full.
func TestADemotionAnsweredWithHeldLeftOutIsKeptAndRefusedOnceItIsDown(t *testing.T) {
	url := storetest.URL(t)
	st := openStore(t, url)
	t1 := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	var phase atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch phase.Load() {
		case 0:
			_, _ = w.Write([]byte(`{"person":"` + keptAda + `","member_kind":"full","held":false,"as_of":"` + t1.Format(time.RFC3339Nano) + `"}`))
		case 1:
			_, _ = w.Write([]byte(`{"person":"` + keptAda + `","member_kind":"restricted","as_of":"` + t2.Format(time.RFC3339Nano) + `"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
		}
	}))
	t.Cleanup(server.Close)
	r := newKeptRig(server.URL, "token", st)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "before the demotion")
	keptKind(t, st, keptAda, projectaccess.MemberFull, "the full answer")
	phase.Store(1)
	r.clock.advance(projectaccess.CacheTTL)
	r.kindOf(t, keptAda, projectaccess.MemberRestricted, "once Dispatch answers the demotion with held left out")
	phase.Store(2)
	r.clock.advance(projectaccess.CacheTTL)
	r.refused(t, keptAda, "while Dispatch is down after a demotion it answered with held left out")
	newKeptRig(server.URL, "token", openStore(t, url)).refused(t, keptAda,
		"at a second process while Dispatch is down after a demotion the first answered with held left out")
}

// TestOneAnswerIsKeptInBothRowsOrNeither: a client that keeps its answers (Keep) is the one writer
// of both rows of an answer of the person route, whichever read fetched it: a member-kind read
// alone keeps ada's kind in person_member_kind and her hold in person_standing, at the one as_of
// the answer rests on, so the hold gate, which writes nothing, answers from the hold this client
// kept. A later answer, a demotion that also holds her, whose hold the database refuses to write,
// keeps neither row: the kind kept stays the earlier full, at the as_of of the hold kept beside
// it. While Dispatch is then down, this client refuses her on the answer it could not keep, and
// its kept hold says she is held and that no write keeps it (KeptHold, Unkept).
//
// The fault: keepPerson writing the kind and the hold in two writes, outside one transaction; the
// demotion's kind is kept though its hold is not, and the test fails naming that answer. And
// keepPerson writing the kind alone, as before the one writer: no hold is kept after the first
// read.
func TestOneAnswerIsKeptInBothRowsOrNeither(t *testing.T) {
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, keptKinds())
	r := keepingClientOf(dispatch, st)
	r.kindOf(t, keptAda, projectaccess.MemberFull, "while Dispatch answers")
	asOf := keptKind(t, st, keptAda, projectaccess.MemberFull, "the first read")
	if standing, found, err := st.PersonStanding(t.Context(), keptAda); err != nil || !found || standing.Held || !standing.AsOf.Equal(asOf) {
		t.Fatalf("person_standing for %s after a member-kind read = %+v (found %t, %v); want not held as of the kind's as_of %v", keptAda, standing, found, err, asOf)
	}

	refuseStandingKeeps(t, st)
	dispatch.SetMemberKind(keptAda, projectaccess.MemberRestricted)
	dispatch.SetHeld(keptAda, true)
	r.clock.advance(projectaccess.CacheTTL)
	r.kindOf(t, keptAda, projectaccess.MemberRestricted, "once Dispatch answers her restricted and held")
	if again := keptKind(t, st, keptAda, projectaccess.MemberFull, "the demotion whose hold the database refused"); !again.Equal(asOf) {
		t.Fatalf("the kind kept for %s after the demotion whose hold the database refused rests on %v; want the earlier answer's %v", keptAda, again, asOf)
	}

	dispatch.SetDown(true)
	r.clock.advance(projectaccess.CacheTTL)
	r.refused(t, keptAda, "while Dispatch is down after the demotion no write kept")
	if kept, found, err := r.client.KeptHold(t.Context(), keptAda); err != nil || !found || !kept.Held || kept.Unkept == nil {
		t.Fatalf("KeptHold(%s) while Dispatch is down after the held answer no write kept = %+v (found %t, %v); want held and not kept", keptAda, kept, found, err)
	}
}

// refuseStandingKeeps makes person_standing refuse every insert and update while
// person_member_kind takes them, as a table whose writes fail beside one whose writes do not.
func refuseStandingKeeps(t *testing.T, st *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`create or replace function refuse_person_standing_writes() returns trigger language plpgsql as $$
			begin raise exception 'this test refuses every write to person_standing'; end $$`,
		`create trigger person_standing_refused before insert or update on person_standing
			for each row execute function refuse_person_standing_writes()`,
	} {
		if _, err := st.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

// stalledDispatch is a Dispatch that answers nothing until the caller hangs up or t ends, and the
// number of questions it was asked.
func stalledDispatch(t *testing.T) (url string, calls func() int) {
	t.Helper()
	var asked atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server.URL, func() int { return int(asked.Load()) }
}

// TestOnlyAReadDispatchDidNotAnswerReadsTheKeptKind: ada is kept full and bob restricted. A read
// its caller's own bound cut short while Dispatch stalls, as a route Dispatch relays bounds its
// read (RelayedReadBound), is one Dispatch did not answer: ada is answered full and bob refused.
// A read that fails for any other reason is refused as it is with no keeper, and counts nothing: a
// caller that hung up (context.Canceled), Dispatch refusing the broker's own account
// (dispatchread.ErrBrokerRefused), and Dispatch answering a kind this broker does not know
// (ErrUnknownMemberKind), which is an answer, kept as Dispatch spelled it, so once Dispatch is down
// she is refused and counted unreadable.
//
// Control: the bounded read of ada, answered full, so the kept kind is there to serve whenever a
// read reads it.
func TestOnlyAReadDispatchDidNotAnswerReadsTheKeptKind(t *testing.T) {
	st := storetest.Open(t)
	dispatch := projectaccesstest.NewServer(t, nil, keptKinds())
	answered := keepingClientOf(dispatch, st)
	answered.kindOf(t, keptAda, projectaccess.MemberFull, "while Dispatch answers")
	answered.kindOf(t, keptBob, projectaccess.MemberRestricted, "while Dispatch answers")
	stalled, stalledCalls := stalledDispatch(t)
	nothingCounted := func(r *keptRig, when string) {
		t.Helper()
		for _, kept := range []string{"full", "restricted", "none", "unreadable"} {
			if got := r.unchecked(t, kept); got != 0 {
				t.Fatalf(`broker_member_kind_unchecked_total{kept=%q} %s = %d; want nothing counted`, kept, when, got)
			}
		}
	}

	t.Run("a read its caller's own bound cut short is answered from the kept kind", func(t *testing.T) {
		r := newKeptRig(stalled, "token", st)
		for _, row := range []struct {
			person string
			served bool
		}{{keptAda, true}, {keptBob, false}} {
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			kind, err := r.client.MemberKind(ctx, row.person)
			cancel()
			switch {
			case row.served && (err != nil || kind != projectaccess.MemberFull):
				t.Fatalf("MemberKind(%s) cut short by its caller's bound = %q, %v; want full, kept", row.person, kind, err)
			case !row.served && (err == nil || !errors.Is(err, context.DeadlineExceeded)):
				t.Fatalf("MemberKind(%s) cut short by its caller's bound = %q, %v; want the deadline's failure, kept restricted", row.person, kind, err)
			}
		}
		if full, restricted := r.unchecked(t, "full"), r.unchecked(t, "restricted"); full != 1 || restricted != 1 {
			t.Fatalf("the two bounded reads counted %d full and %d restricted; want 1 of each", full, restricted)
		}
	})

	t.Run("a caller that hung up is refused, and nothing is counted", func(t *testing.T) {
		r := newKeptRig(stalled, "token", st)
		ctx, cancel := context.WithCancel(t.Context())
		asked := stalledCalls()
		done := make(chan error, 1)
		go func() {
			_, err := r.client.MemberKind(ctx, keptAda)
			done <- err
		}()
		if !testwait.Holds(time.Minute, 5*time.Millisecond, func() bool { return stalledCalls() > asked }) {
			t.Fatal("the read never reached Dispatch")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("MemberKind(%s) whose caller hung up = %v; want context.Canceled", keptAda, err)
		}
		nothingCounted(r, "after a read whose caller hung up")
	})

	t.Run("Dispatch refusing the broker's own account is refused, and nothing is counted", func(t *testing.T) {
		r := newKeptRig(dispatch.URL, "not-the-broker", st)
		if kind, err := r.client.MemberKind(t.Context(), keptAda); !errors.Is(err, dispatchread.ErrBrokerRefused) {
			t.Fatalf("MemberKind(%s) with Dispatch refusing the broker's account = %q, %v; want ErrBrokerRefused", keptAda, kind, err)
		}
		nothingCounted(r, "with Dispatch refusing the broker's account")
	})

	t.Run("a kind this broker does not know is an answer, kept and refused", func(t *testing.T) {
		r := keepingClientOf(dispatch, st)
		dispatch.SetMemberKind(keptAda, "guest-of-honour")
		if kind, err := r.client.MemberKind(t.Context(), keptAda); !errors.Is(err, projectaccess.ErrUnknownMemberKind) {
			t.Fatalf("MemberKind(%s) answered an unknown kind = %q, %v; want ErrUnknownMemberKind", keptAda, kind, err)
		}
		nothingCounted(r, "after Dispatch answered a kind this broker does not know")
		keptKind(t, st, keptAda, "guest-of-honour", "an answer naming a kind this broker does not know")
		dispatch.SetDown(true)
		r.clock.advance(projectaccess.CacheTTL)
		r.refused(t, keptAda, "while Dispatch is down, kept a kind this broker does not know")
		if got := r.unchecked(t, "unreadable"); got != 1 {
			t.Fatalf(`broker_member_kind_unchecked_total{kept="unreadable"} kept a kind this broker does not know = %d; want 1`, got)
		}
	})
}
