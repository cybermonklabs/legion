package graphstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The root's R19 makes an author the person a credential proved, so the credentials of one
// person are one author. A session is its own author only where the record says its credential
// proved it (model.Actor's Proven, which only a broker-issued token sets on a session): two such
// sessions of one workload stay two authors, because every session of that workload carries the
// same workload. A session id a request's body named, not proven, proves nothing of itself -
// anybody can type any id - so beside the workload a pod's
// token proved it is that workload's, the pod's own token's author, as every other id that pod
// names is, and another workload is no author of it; with no workload it proves nobody, as the
// shared bearer proves nobody. This test writes a node under each writer through Write, so its
// made_by is stored as every write stores it, and reads it back under each reader through
// AuthorIsSQL bound with ActingCredential, the pair every author-private read binds. It also
// asks the Go half (SameAuthor, ProvesAuthor and provedAuthor), so the two halves of the one rule
// cannot answer one pair differently. Last, it holds the write half of the rule: a node reserved
// for its author is refused on every credential that proves nobody, the shared bearer, a token
// naming an empty owner and a session only a body named among them, and granted on every
// credential that proves somebody (graphstoretest.VisibilityFor, handed ProvesAuthor's answer).
func TestTheAuthorIsThePersonACredentialProvedAndNothingElseJoinsTwo(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	named := func(name string) *string { return &name }

	cookie := model.Actor{Kind: "user", ID: "Alice"}
	// A personal token records the session its body names beside the person the token proves.
	token := model.Actor{Kind: "session", ID: "ses_typed_in_the_body", Owner: named("alice")}
	tokenRead := model.Actor{Kind: "session", ID: "", Owner: named("alice")}
	verified := model.Actor{Kind: "session", ID: "ses_verified_for_alice", Owner: named(" ALICE ")}
	bob := model.Actor{Kind: "user", ID: "bob"}
	workload := "system:serviceaccount:legion:legion-worker"
	podToken := model.Actor{Service: named(workload)}
	otherPodToken := model.Actor{Service: named("system:serviceaccount:legion:legion-reviewer")}
	// Two session ids a pod's body named beside the workload its token proved, not proven.
	workloadSession := model.Actor{Kind: "session", ID: "ses_of_the_workload_1", Service: named(workload)}
	otherWorkloadSession := model.Actor{Kind: "session", ID: "ses_of_the_workload_2", Service: named(workload)}
	// Two sessions of that workload the broker proved, and the first one's id named by a body.
	brokerSession := model.Actor{Kind: "session", ID: "ses_proved_1", Service: named(workload), Proven: true}
	otherBrokerSession := model.Actor{Kind: "session", ID: "ses_proved_2", Service: named(workload), Proven: true}
	brokerIDNamed := model.Actor{Kind: "session", ID: "ses_proved_1", Service: named(workload)}
	// A session the broker proved that names no person and no workload, and session ids only a
	// body named, with neither.
	brokerUnowned := model.Actor{Kind: "session", ID: "ses_nobody_owns_proved", Proven: true}
	unowned := model.Actor{Kind: "session", ID: "ses_nobody_owns_1"}
	otherUnowned := model.Actor{Kind: "session", ID: "ses_nobody_owns_2"}
	bearer := model.Actor{}

	cases := []struct {
		name           string
		writer, reader model.Actor
		want           bool
	}{
		{"her cookie, then her personal token", cookie, tokenRead, true},
		{"her personal token, then her cookie", token, cookie, true},
		{"her cookie, then a session the broker verified as hers", cookie, verified, true},
		{"a session the broker verified as hers, then her personal token", verified, tokenRead, true},
		{"her cookie, then another person's cookie", cookie, bob, false},
		{"her personal token, then another person's cookie", token, bob, false},
		{"a session a pod named, then the same session", workloadSession, workloadSession, true},
		{"a session a pod named, then another session that pod named", workloadSession, otherWorkloadSession, true},
		{"a session a pod named, then that workload's own token", workloadSession, podToken, true},
		{"that workload's own token, then a session a pod named", podToken, workloadSession, true},
		{"a session a pod named, then another workload's own token", workloadSession, otherPodToken, false},
		{"a session the broker proved, then the same session", brokerSession, brokerSession, true},
		{"a session the broker proved, then another it proved of that workload", brokerSession, otherBrokerSession, false},
		{"another session the broker proved of that workload, then the first", otherBrokerSession, brokerSession, false},
		{"a session the broker proved, then that workload's own token", brokerSession, podToken, false},
		{"a session the broker proved, then its id named by a body", brokerSession, brokerIDNamed, false},
		{"that workload's own token, then a session the broker proved", podToken, brokerSession, false},
		{"a workload's own token, then the same token", podToken, podToken, true},
		{"a workload's own token, then another workload's", podToken, otherPodToken, false},
		{"a session the broker proved owned by nobody, then the same session", brokerUnowned, brokerUnowned, true},
		{"a session the broker proved owned by nobody, then the same id named by a body", brokerUnowned,
			model.Actor{Kind: "session", ID: "ses_nobody_owns_proved"}, false},
		{"a session only a body named, then the same session", unowned, unowned, false},
		{"a session only a body named, then another such session", unowned, otherUnowned, false},
		{"a session only a body named, then her cookie", unowned, cookie, false},
		{"the shared bearer, then the shared bearer", bearer, bearer, false},
		{"her cookie, then the shared bearer", cookie, bearer, false},
	}
	for index, row := range cases {
		id := fmt.Sprintf("author-rule-%02d", index)
		writeNode(t, ctx, database, Node{
			Kind: KindPerson, ID: id, Supertag: "person",
			Visibility: VisibilityProject, State: StateLive,
			MadeAt: at, MadeBy: row.writer, SourceEvent: NoEvent,
		})
		var stored bool
		if err := database.Pool.QueryRow(ctx,
			`select `+AuthorIsSQL("made_by", "$3")+` from graph_nodes where kind = $1 and id = $2`,
			KindPerson, id, ActingCredential(row.reader)).Scan(&stored); err != nil {
			t.Fatalf("%s: read the node under the rule: %v", row.name, err)
		}
		if stored != row.want {
			t.Errorf("%s: AuthorIsSQL answers %t, want %t", row.name, stored, row.want)
		}
		writer, _ := provedAuthor(row.writer)
		reader, read := provedAuthor(row.reader)
		if inGo := SameAuthor(row.writer, row.reader); inGo != row.want {
			t.Errorf("%s: SameAuthor answers %t (writer %+v, reader %+v), want %t", row.name, inGo, writer, reader, row.want)
		}
		if ProvesAuthor(row.reader) != read {
			t.Errorf("%s: ProvesAuthor answers %t for the reader, and provedAuthor %t", row.name, ProvesAuthor(row.reader), read)
		}
	}

	emptyOwner := model.Actor{Kind: "session", Owner: named("")}
	for _, writer := range []struct {
		name   string
		actor  model.Actor
		proves bool
	}{
		{"the shared bearer", bearer, false},
		{"a token naming an empty owner", emptyOwner, false},
		{"her cookie", cookie, true},
		{"her personal token", tokenRead, true},
		{"a session the broker verified as hers", verified, true},
		{"a session a pod named", workloadSession, true},
		{"a session the broker proved", brokerSession, true},
		{"a workload's own token", podToken, true},
		{"a session the broker proved owned by nobody", brokerUnowned, true},
		{"a session only a body named", unowned, false},
	} {
		visibility, err := graphstoretest.VisibilityFor(VisibilityAuthor, ProvesAuthor(writer.actor))
		switch {
		case !writer.proves && !errors.Is(err, graphstoretest.ErrAuthorNamesNobody):
			t.Errorf("a node reserved for its author, written on %s, gets %q (err %v), want the refusal graphstoretest.ErrAuthorNamesNobody", writer.name, visibility, err)
		case writer.proves && (err != nil || visibility != VisibilityAuthor):
			t.Errorf("a node reserved for its author, written on %s, gets %q (err %v), want %q", writer.name, visibility, err, VisibilityAuthor)
		}
		if bound := ActingCredential(writer.actor); (bound != nil) != writer.proves {
			t.Errorf("%s binds %v as the acting author, want a value exactly when it proves somebody (%t)", writer.name, bound, writer.proves)
		}
	}

	// The authored_by edge is attribution and the rule is visibility, and they part on a session
	// that is not proven: a pod's write on a session its body named states its authored_by to that
	// session (AuthorOf, as this change has it), while the rule counts the workload its
	// author; a session only a body named, the shared bearer's writes, keeps its authored_by too,
	// while the rule counts it as nobody. A session the broker proved is its own author under both.
	for _, row := range []struct {
		name               string
		actor              model.Actor
		edgeKind, edgeID   string
		edgeNamed          bool
		ruleKind, ruleID   string
		ruleProvesSomebody bool
	}{
		{"a pod's write on a session its body named", workloadSession, KindSession, "ses_of_the_workload_1", true, authorService, workload, true},
		{"a session only a body named", unowned, KindSession, "ses_nobody_owns_1", true, "", "", false},
		{"a session the broker proved, of a workload", brokerSession, KindSession, "ses_proved_1", true, KindSession, "ses_proved_1", true},
		{"a session the broker proved, owned by nobody", brokerUnowned, KindSession, "ses_nobody_owns_proved", true, KindSession, "ses_nobody_owns_proved", true},
	} {
		kind, id, named := AuthorOf(row.actor)
		if kind != row.edgeKind || id != row.edgeID || named != row.edgeNamed {
			t.Errorf("%s: AuthorOf names %s:%s (named %t), want %s:%s (named %t)", row.name, kind, id, named, row.edgeKind, row.edgeID, row.edgeNamed)
		}
		if nodes, tuples, stated := AuthorFacts(KindComment, "11111111-1111-4111-8111-111111111111", row.actor, at, NoEvent); stated != row.edgeNamed ||
			(stated && (len(tuples) == 0 || tuples[0].ToKind != row.edgeKind || tuples[0].ToID != row.edgeID)) || (!stated && (len(nodes) != 0 || len(tuples) != 0)) {
			t.Errorf("%s: AuthorFacts states %d nodes and the tuples %+v (stated %t), want the authored_by to %s:%s only where AuthorOf names it",
				row.name, len(nodes), tuples, stated, row.edgeKind, row.edgeID)
		}
		proved, proves := provedAuthor(row.actor)
		if proved.Kind != row.ruleKind || proved.ID != row.ruleID || proves != row.ruleProvesSomebody {
			t.Errorf("%s: provedAuthor names %+v (proved %t), want %s:%s (proved %t)", row.name, proved, proves, row.ruleKind, row.ruleID, row.ruleProvesSomebody)
		}
	}
}

// A run is read by the credential that asked it, whatever its record proves under the one rule.
// Its predicate compares the whole credential (AuthorCredentialIsSQL), and the bound credential
// is nil unless the reader proves an author (ActingCredential), so the four-field equality holds
// the run to one credential; the record is asked only whether it names a credential at all
// (NamesCredential), as it was before model.Actor's Proven existed. A run a session that names no
// owner and no workload recorded before then carries no proven mark: the rule counts that record as
// nobody, and the session, proved by the broker now, reads its run all the same. No other session
// does, of no workload, nor that session's id named by a body, nor a workload's own token. A run
// whose author is the shared bearer's empty one is read by nobody, the bearer included.
func TestARunIsReadByTheCredentialThatAskedItWhateverItsRecordProves(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	named := func(name string) *string { return &name }
	workload := named("system:serviceaccount:legion:legion-worker")
	const asker = "ses_asked_before_the_proven_mark"

	oldShape := model.Actor{Kind: "session", ID: asker}
	bearer := model.Actor{}
	readers := []struct {
		name  string
		who   model.Actor
		reads bool
	}{
		{"that session, proved by the broker now", model.Actor{Kind: "session", ID: asker, Proven: true}, true},
		{"that session's id named by a body", model.Actor{Kind: "session", ID: asker}, false},
		{"another session the broker proved", model.Actor{Kind: "session", ID: "ses_another", Proven: true}, false},
		{"that id beside a workload, proved by the broker", model.Actor{Kind: "session", ID: asker, Service: workload, Proven: true}, false},
		{"that id beside a workload, named by a pod", model.Actor{Kind: "session", ID: asker, Service: workload}, false},
		{"another session of a workload, proved by the broker", model.Actor{Kind: "session", ID: "ses_of_a_workload", Service: workload, Proven: true}, false},
		{"a workload's own token", model.Actor{Service: workload}, false},
		{"the shared bearer", bearer, false},
	}
	if ProvesAuthor(oldShape) || !NamesCredential(oldShape) || NamesCredential(bearer) {
		t.Fatalf("the old-shape record proves an author %t and names a credential %t, and the shared bearer names one %t: "+
			"want no author, a credential, and none", ProvesAuthor(oldShape), NamesCredential(oldShape), NamesCredential(bearer))
	}
	for index, writer := range []struct {
		name   string
		actor  model.Actor
		readBy func(reads bool) bool
	}{
		{"a run that session recorded before the mark", oldShape, func(reads bool) bool { return reads }},
		{"a run of the shared bearer's empty author", bearer, func(bool) bool { return false }},
	} {
		id := fmt.Sprintf("credential-rule-%02d", index)
		writeNode(t, ctx, database, Node{
			Kind: KindPerson, ID: id, Supertag: "person",
			Visibility: VisibilityProject, State: StateLive,
			MadeAt: at, MadeBy: writer.actor, SourceEvent: NoEvent,
		})
		for _, reader := range readers {
			want := writer.readBy(reader.reads)
			var stored bool
			if err := database.Pool.QueryRow(ctx,
				`select `+AuthorCredentialIsSQL("made_by", "$3")+` from graph_nodes where kind = $1 and id = $2`,
				KindPerson, id, ActingCredential(reader.who)).Scan(&stored); err != nil {
				t.Fatalf("%s, read by %s: %v", writer.name, reader.name, err)
			}
			if stored != want {
				t.Errorf("%s, read by %s: AuthorCredentialIsSQL answers %t, want %t", writer.name, reader.name, stored, want)
			}
			// The Go twin a run's owner is held to (api's goingRunIsTheCallers), for every reader
			// a request resolves: no credential resolves to a session that proves nobody.
			if ProvesAuthor(reader.who) {
				if inGo := NamesCredential(writer.actor) && writer.actor.SameCredential(reader.who); inGo != want {
					t.Errorf("%s, read by %s: NamesCredential and SameCredential answer %t, want %t", writer.name, reader.name, inGo, want)
				}
			}
		}
	}
}
