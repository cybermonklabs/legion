package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A run recorded before model.Actor's Proven existed is its own credential's still: the stream
// holds its owner to goingRunIsTheCallers, which asks the recorded author only whether it names a
// credential at all (graphstore.NamesCredential) and then compares the whole credential
// (model.Actor.SameCredential), never the one author rule.
//
// Each row is a recorded author as an older pod stored it, with no mark, and the caller as a
// request resolves it now (optionalActor): a broker session carries the mark from its resolution
// on (brokerTokenActor). A session that names no owner and no workload, which the rule now counts
// as nobody, is read by that session and by no other; so is a broker session beside a person or a
// workload, recorded before the mark. A session of another workload, a workload's own token and
// the shared bearer read none of them, and the shared bearer's empty author is nobody's.
func TestARunRecordedBeforeTheMarkIsItsOwnCredentialsStill(t *testing.T) {
	named := func(name string) *string { return &name }
	workload := named("system:serviceaccount:legion:legion-worker")
	otherWorkload := named("system:serviceaccount:legion:legion-reviewer")
	person := named("alice")
	const asker = "ses_asked_before_the_proof"
	proved := func(actor model.Actor) model.Actor {
		actor.Proven = true
		actor.Credential = &model.Credential{Session: actor.ID}
		return actor
	}
	ownerless := model.Actor{Kind: "session", ID: asker}
	beside := func(actor model.Actor, owner, service *string) model.Actor {
		actor.Owner, actor.Service = owner, service
		return actor
	}
	for _, row := range []struct {
		name           string
		author, caller model.Actor
		want           bool
	}{
		{"an ownerless session's run, then that session", ownerless, proved(ownerless), true},
		{"an ownerless session's run, then another session", ownerless, proved(model.Actor{Kind: "session", ID: "ses_another"}), false},
		{"an ownerless session's run, then that id beside a workload", ownerless, proved(beside(ownerless, nil, workload)), false},
		{"an ownerless session's run, then a workload's own token", ownerless, model.Actor{Service: workload}, false},
		{"an ownerless session's run, then the shared bearer", ownerless, model.Actor{}, false},
		{"a person's broker session's run, then that session", beside(ownerless, person, nil), proved(beside(ownerless, person, nil)), true},
		{"a workload's broker session's run, then that session", beside(ownerless, nil, workload), proved(beside(ownerless, nil, workload)), true},
		{"a workload's broker session's run, then that id of another workload", beside(ownerless, nil, workload), proved(beside(ownerless, nil, otherWorkload)), false},
		{"a workload's broker session's run, then the workload's own token", beside(ownerless, nil, workload), model.Actor{Service: workload}, false},
		{"the shared bearer's run, then the shared bearer", model.Actor{}, model.Actor{}, false},
	} {
		if got := goingRunIsTheCallers(row.author, row.caller); got != row.want {
			t.Errorf("%s: goingRunIsTheCallers answers %t, want %t (author %+v, caller %+v)", row.name, got, row.want, row.author, row.caller)
		}
	}
}
