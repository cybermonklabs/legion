package llm

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/envoy/agentloop"
)

// The measures of the pacer.
const (
	// paceWindow is the span the pacer's limit holds over: any one minute.
	paceWindow = time.Minute
	// bytesPerToken is how the harness estimates a prompt's tokens from its bytes, as
	// writing-check's reader budget counts them.
	bytesPerToken = 4
)

// Pacer holds the uncached input of the model calls that pass it to a limit of tokens in any one
// minute, across every Runner that shares it. A call's uncached input is the prompt tokens the
// provider bills at the full input price: its input tokens less those it read from the cache. A
// cache write counts. Before each attempt of a call, the pacer charges the attempt's estimate. It
// waits while the charges of the last minute leave no room for that estimate. After the reply, it
// corrects the charge to the tokens the provider reported. Calls go through in the order they
// came. A call larger than the limit goes when the last minute holds no charge, and then it is
// the only call of its minute. A Pacer of limit 0 holds no call back: it only counts.
type Pacer struct {
	limit int
	// turn lets one call at a time wait for room, so the calls go through in the order they came.
	turn chan struct{}

	mu sync.Mutex
	// charges are every charge of the process, oldest first. window is the index of the first
	// charge less than a minute old, and inWindow the tokens of the charges from there on.
	charges  []charge
	window   int
	inWindow int
	// freed is closed, and replaced, when a correction gives tokens of the last minute back.
	freed chan struct{}
}

// charge is one attempt's uncached input: the estimate until its reply, then what the reply
// reported.
type charge struct {
	at     time.Time
	tokens int
}

// NewPacer is a pacer of perMinute uncached input tokens a minute; 0 holds no call back.
func NewPacer(perMinute int) *Pacer {
	return &Pacer{
		limit: max(perMinute, 0),
		turn:  make(chan struct{}, 1),
		freed: make(chan struct{}),
	}
}

// tokensOf is n bytes of prompt in tokens, at four bytes a token.
func tokensOf(n int) int { return (n + bytesPerToken - 1) / bytesPerToken }

// estimateOf is the estimate of req's prompt in tokens: its system prompt, its schema and its
// user message, at four bytes a token. The harness sets no cache mark, so the pacer charges
// every prompt whole.
func estimateOf(req Request) int {
	return tokensOf(len(req.System) + len(req.Schema) + len(req.User))
}

// paced is one attempt's charge, which settle corrects after the reply. A paced of no pacer
// corrects nothing.
type paced struct {
	p     *Pacer
	index int
}

// admit waits until the charges of the last minute leave room for the attempt's estimate of
// tokens, or until ctx ends, and charges it. A nil Pacer admits at once and charges nothing.
func (p *Pacer) admit(ctx context.Context, tokens int) (paced, error) {
	if p == nil {
		return paced{}, nil
	}
	if p.limit > 0 {
		select {
		case p.turn <- struct{}{}:
		case <-ctx.Done():
			return paced{}, ctx.Err()
		}
		defer func() { <-p.turn }()
	}
	for {
		p.mu.Lock()
		now := time.Now()
		p.slide(now)
		if p.limit == 0 || p.inWindow == 0 || p.inWindow+max(tokens, 1) <= p.limit {
			p.charges = append(p.charges, charge{at: now, tokens: tokens})
			p.inWindow += tokens
			p.mu.Unlock()
			return paced{p: p, index: len(p.charges) - 1}, nil
		}
		// The last minute holds tokens, so it holds a charge: wait until its oldest charge leaves
		// it, or until a reply gives tokens back.
		timer := time.NewTimer(p.charges[p.window].at.Add(paceWindow).Sub(now))
		freed := p.freed
		p.mu.Unlock()
		select {
		case <-timer.C:
		case <-freed:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return paced{}, ctx.Err()
		}
	}
}

// slide drops from the last minute's tokens the charges a minute old or older at now.
func (p *Pacer) slide(now time.Time) {
	for p.window < len(p.charges) && !now.Before(p.charges[p.window].at.Add(paceWindow)) {
		p.inWindow -= p.charges[p.window].tokens
		p.window++
	}
}

// settle corrects the attempt's charge to the uncached input its reply reported in used: the
// input tokens and the cache write tokens. A reply that reported no counts keeps the estimate.
func (c paced) settle(used agentloop.Usage) {
	p := c.p
	if p == nil || !used.Metered {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := &p.charges[c.index]
	tokens := used.Input + used.CacheWrite
	if c.index >= p.window {
		p.inWindow += tokens - ch.tokens
		if tokens < ch.tokens && p.limit > 0 {
			close(p.freed)
			p.freed = make(chan struct{})
		}
	}
	ch.tokens = tokens
}

// PaceUse is the uncached input of the calls that passed a pacer: each attempt's tokens as its
// reply reported them, or its estimate where the reply reported none.
type PaceUse struct {
	// Tokens is the uncached input of every attempt, and BusiestMinute the most of it in any one
	// minute, each attempt counted at its start.
	Tokens        int `json:"tokens"`
	BusiestMinute int `json:"busiest_minute_tokens"`
}

// Use is the uncached input of the calls that passed the pacer so far; none for a nil Pacer.
func (p *Pacer) Use() PaceUse {
	if p == nil {
		return PaceUse{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var u PaceUse
	first, minute := 0, 0
	for _, c := range p.charges {
		u.Tokens += c.tokens
		minute += c.tokens
		for !c.at.Before(p.charges[first].at.Add(paceWindow)) {
			minute -= p.charges[first].tokens
			first++
		}
		u.BusiestMinute = max(u.BusiestMinute, minute)
	}
	return u
}
