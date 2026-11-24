package supervise

import "context"

// Answer is the model that answered the claim's agent last, as `<provider>/<model>` (or the
// model alone when the agent's frame named no provider). The zero Answer is no answer: none since
// this machine was built, or none in the claim's current session; or one whose model no store can
// keep, which the stream reads as an answer that names no model (stream.Answered).
type Answer struct {
	Model string
}

// sessionAnswer is an answer with the session it was given in.
type sessionAnswer struct {
	Answer
	session string
}

// answered keeps the model that just answered, under the session the claim records now: the fence
// has dropped an answer of a launch before that session's spawn (Machine.fence, sessionFrom), so
// this one is of a launch of that session, the one that spawned it or a resume of it. It writes
// nothing to the store and leaves the claim exactly where it found it.
func answered(m *Machine, _ context.Context, ev Event) error {
	a := ev.(StreamAnswered)
	m.answer.Store(&sessionAnswer{Answer: Answer{Model: a.Model}, session: m.claim.Session})
	return nil
}

// LastAnswer is the model that answered last in the claim's session, as the machine last
// published that session (View): an answer given under another session, before a fresh session
// replaced it or before the session was abandoned, is none. So a reader that pairs the session of
// View with this model never names a model that did not serve that session. It never waits on
// the decision in flight, so a transaction may call it while the machine is deciding.
func (m *Machine) LastAnswer() Answer {
	last := m.answer.Load()
	if last == nil || last.session != m.view.Load().Session {
		return Answer{}
	}
	return last.Answer
}
