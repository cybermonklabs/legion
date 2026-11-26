package going

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/agentloop"
)

// ErrQuotaPastDeadline is a model call a Pacer cannot pace inside the time its context has left.
// It wraps context.DeadlineExceeded because that is what it is - the call reaching its deadline:
// a short answer ends with the deadline's one code (Answerer.faultCode), and the agent's step
// waits and is made again (going/agent).
var ErrQuotaPastDeadline = fmt.Errorf("going: the quota cannot pace this call inside the time left: %w", context.DeadlineExceeded)

// Pacer paces one process's model calls to its share of the chat deployment's tokens a minute
// (DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE): one rate.Limiter of the share's tokens a second with a
// burst of a minute's share. Each HTTP attempt draws the estimate of what it sends before it is
// made (Paced), and each model call that ended is charged the tokens it spent past what its
// attempts drew (Owe), so the calls stay within the share where they spend more than their
// estimates. The agent's runs (going/agent) and the server's short answers (Answerer) each pace
// their own share with one. A nil Pacer paces nothing.
type Pacer struct {
	limiter *rate.Limiter
	now     func() time.Time
}

// NewPacer is the pacer of share, a fraction, of a deployment whose quota is tokensPerMinute,
// which reads the time from now, time.Now where now is nil; nil for a quota of none (the endpoint
// form names none). The share is at least one token a minute: a quota of a few tokens a minute
// paces each call to one a minute rather than refusing every call.
func NewPacer(tokensPerMinute int, share float64, now func() time.Time) *Pacer {
	if tokensPerMinute <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	tokens := max(int(float64(tokensPerMinute)*share), 1)
	return &Pacer{limiter: rate.NewLimiter(rate.Limit(float64(tokens)/60), tokens), now: now}
}

// Charge is what a Pacer did with one HTTP attempt of a model call (Paced): the estimate it
// charged it, the tokens it drew from the share for it, at most a minute's share, how long it
// held it, and the error it refused it with, where the attempt is not made, keeps no reservation,
// and is charged and draws nothing.
type Charge struct {
	Tokens, Drawn int
	Held          time.Duration
	Err           error
}

// Paced is model with the pacing taken in its endpoint's Token, which agentloop calls before
// every HTTP attempt, the retry included: each attempt is charged estimate's tokens, of which the
// pacer draws at most a minute's share, and charged is told what the pacer did with it (Charge).
// A nil Pacer holds no attempt, and still tells charged each attempt's estimate, as charged and as
// drawn, so a caller whose deployment names no quota still says what the pacer would have
// reserved.
func (p *Pacer) Paced(model agentloop.Model, estimate func() int, charged func(Charge)) agentloop.Model {
	token := model.Endpoint.Token
	model.Endpoint.Token = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		tokens := estimate()
		held, err := p.wait(ctx, tokens)
		if err != nil {
			charged(Charge{Held: held, Err: err})
			return "", err
		}
		charged(Charge{Tokens: tokens, Drawn: p.draws(tokens), Held: held})
		return token(ctx)
	}
	return model
}

// Owe charges the share, as a model call ends, the tokens the call spent past those its HTTP
// attempts drew: the limiter's tokens go below none, a minute's share at most a reservation, and
// the next calls wait the debt out. A call that spent no more than it drew, tokens of none or
// fewer, owes nothing, and so does a nil Pacer.
func (p *Pacer) Owe(tokens int) {
	if p == nil {
		return
	}
	now := p.now()
	for burst := p.limiter.Burst(); tokens > 0; tokens -= burst {
		p.limiter.ReserveN(now, min(tokens, burst))
	}
}

// draws is the tokens an attempt charged tokens draws from the share: all of them, at most a
// minute's share (delay), and all of them for a nil Pacer, which draws nothing but counts them.
func (p *Pacer) draws(tokens int) int {
	if p == nil {
		return tokens
	}
	return min(tokens, p.limiter.Burst())
}

// delay reserves tokens at now, at most a minute's share, and answers how long the call waits for
// them; false where no wait serves them. A call estimated past a minute's whole share reserves
// the share: it waits for a minute's share and is made, rather than being refused for a cost no
// wait could serve.
func (p *Pacer) delay(now time.Time, tokens int) (*rate.Reservation, time.Duration, bool) {
	reservation := p.limiter.ReserveN(now, p.draws(tokens))
	if !reservation.OK() {
		return nil, 0, false
	}
	return reservation, reservation.DelayFrom(now), true
}

// wait waits out the pacing of tokens and answers how long it held the call. A reservation that
// cannot be served inside the time ctx has left is refused rather than waited on, because waiting
// it out would spend the call's time to make no call at all. A nil Pacer holds nothing.
func (p *Pacer) wait(ctx context.Context, tokens int) (time.Duration, error) {
	if p == nil {
		return 0, nil
	}
	now := p.now()
	reservation, wait, ok := p.delay(now, tokens)
	if !ok {
		return 0, ErrQuotaPastDeadline
	}
	if wait <= 0 {
		return 0, nil
	}
	if deadline, has := ctx.Deadline(); has && now.Add(wait).After(deadline) {
		reservation.CancelAt(now)
		return 0, ErrQuotaPastDeadline
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		ended := p.now()
		reservation.CancelAt(ended)
		return ended.Sub(now), ctx.Err()
	case <-timer.C:
		return p.now().Sub(now), nil
	}
}

// PromptChars is the characters a model call sends the model, which a pacer's estimate counts a
// token for each four of: its system prompt; the text, tool calls' arguments and tool results of
// its messages; and each tool's name, description and argument schema, the schema as the request
// carries it, without the whitespace between its tokens (compactLen). A call sends its tools even
// where it offers none, as a run's last call does with a tool choice of none, so they count on
// every call that sends them; a rewrite of a reply the bar held back as long (going/agent's
// rewrite) sends none, and its caller passes none.
func PromptChars(system string, messages []agentloop.Message, tools []agentloop.Tool) int {
	chars := len(system)
	for _, message := range messages {
		for _, block := range message.Blocks {
			chars += len(block.Text) + len(block.Input)
			for _, part := range block.Content {
				chars += len(part.Text)
			}
		}
	}
	for _, tool := range tools {
		chars += len(tool.Name) + len(tool.Description) + compactLen(tool.Schema)
	}
	return chars
}

// compactLen is the length of the JSON value raw without the whitespace between its tokens, as a
// request carries it: encoding/json compacts a json.RawMessage it encodes.
func compactLen(raw []byte) int {
	n := 0
	inString, escaped := false, false
	for _, b := range raw {
		switch {
		case escaped:
			escaped = false
		case inString && b == '\\':
			escaped = true
		case b == '"':
			inString = !inString
		case !inString && (b == ' ' || b == '\t' || b == '\n' || b == '\r'):
			continue
		}
		n++
	}
	return n
}

// SpentTokens is the tokens a model call spent as its provider counts them: its prompt, what the
// provider's cache served and took among it, and its output, its reasoning among it
// (agentloop.Usage).
func SpentTokens(usage agentloop.Usage) int {
	return usage.Input + usage.CacheRead + usage.CacheWrite + usage.Output
}
