package going

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
)

// TestAPacerPacesItsShareOfTheQuota: a pacer paces its calls to its share of the deployment's
// tokens a minute: of a quota of 60,000 a minute, a share of 75% lets 45,000 tokens go at once,
// and the next 750 wait the second 750 tokens of 45,000 a minute take. A share of a quota of a
// few tokens a minute is one token, and a deployment that names no quota is not paced.
func TestAPacerPacesItsShareOfTheQuota(t *testing.T) {
	pacer := NewPacer(60_000, 0.75, nil)
	now := time.Unix(1_700_000_000, 0)
	if _, wait, ok := pacer.delay(now, 45_000); !ok || wait != 0 {
		t.Fatalf("45,000 tokens of a 60,000 quota wait %v (ok %t), want none", wait, ok)
	}
	if _, wait, ok := pacer.delay(now, 750); !ok || wait < 990*time.Millisecond || wait > 1010*time.Millisecond {
		t.Fatalf("750 tokens past the share wait %v (ok %t), want 1 s", wait, ok)
	}
	if burst := NewPacer(1, 0.25, nil).limiter.Burst(); burst != 1 {
		t.Errorf("a quarter of a quota of 1 token a minute is a share of %d tokens, want 1", burst)
	}
	if NewPacer(0, 0.75, nil) != nil {
		t.Error("a deployment that names no quota is paced")
	}
}

// TestEachAttemptIsToldWhatThePacerChargedAndHeldIt: every HTTP attempt of a paced model call, the
// retry among them, is charged the call's estimate, drawn from the share, and told how long the
// pacer held it. Of a quota of 60,000 a minute, whose share is 45,000, a first attempt of 22,575
// tokens is held for nothing and a second waits the 150 its share lacks, 0.2 s; a third, with
// 10 ms left, is refused at once, charged nothing and not made. A deployment that names no quota
// holds no attempt and still charges each its estimate, which the agent's reply frames then say.
func TestEachAttemptIsToldWhatThePacerChargedAndHeldIt(t *testing.T) {
	const estimate = 22_575
	for _, c := range []struct {
		name  string
		pacer *Pacer
	}{
		{name: "a deployment that names no quota"},
		{name: "a quota of 60,000 a minute", pacer: NewPacer(60_000, 0.75, nil)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var charges []Charge
			read := 0
			token := func(context.Context) (string, error) {
				read++
				return "a token", nil
			}
			model := c.pacer.Paced(agentloop.Model{Endpoint: agentloop.Endpoint{Token: token}}, func() int { return estimate },
				func(charge Charge) { charges = append(charges, charge) })
			for attempt := range 2 {
				if _, err := model.Endpoint.Token(t.Context()); err != nil {
					t.Fatalf("attempt %d: %v", attempt+1, err)
				}
			}
			short, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			_, err := model.Endpoint.Token(short)
			if len(charges) != 3 {
				t.Fatalf("three attempts were told %d charges, %v, want one each", len(charges), charges)
			}
			drawn := Charge{Tokens: estimate, Drawn: estimate}
			if c.pacer == nil {
				if err != nil || read != 3 || charges[0] != drawn || charges[1] != drawn || charges[2] != drawn {
					t.Errorf("with no quota the attempts were told %v and %d were made (%v), want each made, charged %d and held for nothing", charges, read, err, estimate)
				}
				return
			}
			if charges[0] != drawn {
				t.Errorf("the first attempt was told %+v, want %d tokens drawn and held for nothing", charges[0], estimate)
			}
			if second := charges[1]; second.Tokens != estimate || second.Drawn != estimate || second.Held < 150*time.Millisecond || second.Held > 2*time.Second || second.Err != nil {
				t.Errorf("the second attempt was told %+v, want %d tokens drawn and held about 0.2 s", second, estimate)
			}
			if refused := charges[2]; !errors.Is(err, ErrQuotaPastDeadline) || read != 2 || refused.Tokens != 0 || refused.Drawn != 0 || !errors.Is(refused.Err, ErrQuotaPastDeadline) {
				t.Errorf("the attempt with 10 ms left answered %v, was told %+v, and %d attempts were made; want it refused, charged nothing and not made", err, refused, read)
			}
		})
	}
}

// TestAPacerChargesWhatACallSpentPastItsDraw: a model call spends more than the pacer drew for it
// where its prompt holds more tokens than a quarter of its characters, or it reasons past what
// its estimate allows; the pacer charges the rest as the call ends (Owe), so the next call waits
// for it. Calls of an agent's Fast run, each estimated at its prompt's tokens and its level's
// 1,000 output and 353 reasoning tokens, whose prompts the real model read at 1.21x to 1.65x the
// estimate, and which wrote up to 222 output and 984 reasoning tokens (the preview's measures),
// each spend more than they drew; made one after another, each as soon as its share allows, under
// a share of 45,000 a minute, the calls sent in each 60 s after one whose draw emptied the share
// spend at most the share and what the last of them spent past its draw, which the pacer charges
// only as that call ends. Charged nothing past their draws, the same calls spend more than that.
// A debt past a minute's share is charged whole, and a nil pacer owes nothing.
func TestAPacerChargesWhatACallSpentPastItsDraw(t *testing.T) {
	const callTime = 4 * time.Second
	type call struct{ estimate, spent int }
	var calls []call
	for _, c := range []struct {
		prompt         int
		read           float64
		wrote, reasons int
	}{
		{prompt: 8000, read: 1.55, wrote: 111},
		{prompt: 12_000, read: 1.65, wrote: 504},
		{prompt: 10_000, read: 1.21, wrote: 222, reasons: 984},
	} {
		calls = append(calls, call{estimate: c.prompt + 1000 + 353, spent: int(float64(c.prompt)*c.read) + c.wrote + c.reasons})
	}
	type sent struct {
		at           time.Time
		spent, owed  int
		emptiedShare bool
	}
	// paced makes 90 calls one after another, each held as the pacer's wait holds it, and
	// charges each what it spent past its draw where owing is set.
	paced := func(owing bool) ([]sent, int) {
		clock := time.Unix(1_700_000_000, 0)
		p := NewPacer(60_000, 0.75, func() time.Time { return clock })
		var sends []sent
		for i := range 90 {
			c := calls[i%len(calls)]
			_, wait, ok := p.delay(clock, c.estimate)
			if !ok {
				t.Fatalf("call %d of %d tokens was refused", i+1, c.estimate)
			}
			clock = clock.Add(wait)
			owed := c.spent - p.draws(c.estimate)
			if owed <= 0 {
				t.Fatalf("call %d spends %d tokens of the %d it drew, want more: the row is about calls that pass their draw", i+1, c.spent, c.estimate)
			}
			sends = append(sends, sent{at: clock, spent: c.spent, owed: owed, emptiedShare: wait > 0})
			clock = clock.Add(callTime)
			if owing {
				p.Owe(owed)
			}
		}
		return sends, p.limiter.Burst()
	}
	// most is the most the calls sent in the 60 s after one whose draw emptied the share spend,
	// less what the last of them spent past its draw.
	most := func(sends []sent) int {
		worst := 0
		for j, from := range sends {
			if !from.emptiedShare {
				continue
			}
			spent, last := 0, 0
			for _, s := range sends[j+1:] {
				if s.at.Sub(from.at) > time.Minute {
					break
				}
				spent, last = spent+s.spent, s.owed
			}
			worst = max(worst, spent-last)
		}
		return worst
	}
	owing, share := paced(true)
	if got := most(owing); got == 0 || got > share {
		t.Errorf("the calls sent in 60 s spent %d tokens past the debt the pacer had not charged yet, want at most the share, %d", got, share)
	}
	free, _ := paced(false)
	if got := most(free); got <= share {
		t.Errorf("with nothing charged past their draws the calls sent in 60 s spent %d tokens, want more than the share, %d: the row's calls do not spend past their draws", got, share)
	}

	now := time.Unix(1_700_000_000, 0)
	p := NewPacer(60_000, 0.75, func() time.Time { return now })
	p.Owe(100_000)
	if _, wait, ok := p.delay(now, 750); !ok || wait < 74*time.Second || wait > 75*time.Second {
		t.Errorf("750 tokens after a debt of 100,000 on a full share of 45,000 wait %v (ok %t), want the 55,750 tokens past the share at 750 a second, 74.3 s", wait, ok)
	}
	var none *Pacer
	none.Owe(100_000)
}
