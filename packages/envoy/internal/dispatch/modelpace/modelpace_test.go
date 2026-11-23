package modelpace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// clock drives a pacer on a time of its own: the test sets it, and Sleep records the wait the
// pacer asked for rather than waiting it, so a run of minutes takes no time.
type clock struct {
	now   time.Time
	slept time.Duration
}

func (c *clock) Now() time.Time { return c.now }

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.slept = d
	return nil
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) config(quotas map[string]int) Config {
	return Config{Quotas: quotas, Now: c.Now, Sleep: c.Sleep}
}

const (
	// sharedDeployment is the one stand-in deployment both callers call.
	sharedDeployment = "shared-deployment"
	// quota is sharedDeployment's tokens a minute (DISPATCH_MODEL_DEPLOYMENT_QUOTAS).
	quota = 6000
	// ceiling is each caller's own tokens a minute: under the quota, so either caller alone
	// stays inside it, and together over it, so only a shared limiter keeps the two inside.
	ceiling = 4800
	// callTokens is what one call is charged.
	callTokens = 300
	// span is how long the callers run, on the clock.
	span = 10 * time.Minute
)

// callerHeader and atHeader carry, on each call to the stand-in, the caller that made it and the
// time on the clock it was made at, so the stand-in's record is the measure.
const (
	callerHeader = "X-Test-Caller"
	atHeader     = "X-Test-At"
)

// drive runs each caller for span on c, every one asking for its next call as soon as its last
// was made, as a caller with more to do than its share does, and makes each call to the
// stand-in's deployment once the pacer lets it go. Each caller is named by its index in callers.
func drive(t *testing.T, c *clock, stand *modeltest.Model, deployment string, callers ...*Caller) {
	t.Helper()
	start := c.now
	end := start.Add(span)
	next := make([]time.Time, len(callers))
	for i := range next {
		next[i] = start
	}
	words := strings.Repeat("w", callTokens*4)
	for {
		// The caller that asks next on the clock, the lowest index first, so the clock only
		// goes forward and the run is the same every time.
		i := slices.IndexFunc(next, func(at time.Time) bool { return !at.IsZero() })
		if i < 0 {
			return
		}
		for j := range next {
			if !next[j].IsZero() && next[j].Before(next[i]) {
				i = j
			}
		}
		c.now, c.slept = next[i], 0
		if err := callers[i].Wait(t.Context(), callTokens); err != nil {
			t.Fatalf("caller %d at %s: Wait: %v", i, c.now.Sub(start), err)
		}
		at := c.now.Add(c.slept)
		if !at.Before(end) {
			next[i] = time.Time{}
			continue
		}
		call(t, stand, deployment, i, at, words)
		next[i] = at
	}
}

// call makes one Chat Completions call to the stand-in, as a caller of deployment would.
func call(t *testing.T, stand *modeltest.Model, deployment string, caller int, at time.Time, words string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    deployment,
		"messages": []any{map[string]any{"role": "user", "content": words}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, stand.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(callerHeader, fmt.Sprint(caller))
	request.Header.Set(atHeader, at.Format(time.RFC3339Nano))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the stand-in answered %d", response.StatusCode)
	}
}

// overspends reads what the stand-in's deployment received and names each bound it broke: the
// callers' tokens together past the deployment's quota, and one caller's past its own ceiling,
// each over span plus the minute's tokens a limiter holds as its burst. It also names a caller
// that made no call, and a run that spent less than the quota allows, since either would make a
// pass prove nothing about the bound.
func overspends(t *testing.T, stand *modeltest.Model, deployment string, callers int) []string {
	t.Helper()
	minutes := int(span / time.Minute)
	spent := make([]int, callers)
	total := 0
	for _, request := range stand.Requests() {
		if request.Model != deployment {
			t.Fatalf("a call named %q, want the one deployment %q", request.Model, deployment)
		}
		var caller int
		if _, err := fmt.Sscan(request.Header.Get(callerHeader), &caller); err != nil {
			t.Fatalf("a call with no caller: %v", err)
		}
		tokens := utf8.RuneCountInString(request.Text) / 4
		spent[caller] += tokens
		total += tokens
	}
	var broken []string
	if allowed := quota * (minutes + 1); total > allowed {
		broken = append(broken, fmt.Sprintf("the callers spent %d tokens of %s in %s, past its quota's %d", total, deployment, span, allowed))
	}
	for caller, tokens := range spent {
		if allowed := ceiling * (minutes + 1); tokens > allowed {
			broken = append(broken, fmt.Sprintf("caller %d spent %d tokens in %s, past its ceiling's %d", caller, tokens, span, allowed))
		}
		if tokens == 0 {
			broken = append(broken, fmt.Sprintf("caller %d made no call", caller))
		}
	}
	if total < quota*minutes {
		broken = append(broken, fmt.Sprintf("the callers spent %d tokens in %s, under the %d the quota allows: the run does not reach the bound", total, span, quota*minutes))
	}
	return broken
}

// Two callers of one deployment share its limiter: their calls together stay inside the
// deployment's quota while each stays inside its own ceiling, though either ceiling alone is
// under the quota and the two together are over it.
func TestTwoCallersOfOneDeploymentStayInsideItsQuota(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Model: sharedDeployment, Reply: modeltest.Reply{Text: "ok"}})
	c := newClock()
	pacer := New(c.config(map[string]int{sharedDeployment: quota}))
	deployment := Deployment{Kind: "azure", Host: hostOf(t, stand.URL), Name: sharedDeployment}
	first := pacer.Join(Share{Deployment: deployment, TokensPerMinute: ceiling})
	second := pacer.Join(Share{Deployment: deployment, TokensPerMinute: ceiling})
	drive(t, c, stand, sharedDeployment, first, second)
	for _, broken := range overspends(t, stand, sharedDeployment, 2) {
		t.Error(broken)
	}
}

// The control (INFRA-3552's c2): one caller given a limiter of its own on the same deployment -
// what a caller that builds a pacer of its own gets, as the answerer's private one did - takes
// its whole ceiling beside the other's, and the check above reads the deployment's quota broken.
func TestACallerWithALimiterOfItsOwnBreaksTheDeploymentsQuota(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Model: sharedDeployment, Reply: modeltest.Reply{Text: "ok"}})
	c := newClock()
	quotas := map[string]int{sharedDeployment: quota}
	deployment := Deployment{Kind: "azure", Host: hostOf(t, stand.URL), Name: sharedDeployment}
	shared := New(c.config(quotas)).Join(Share{Deployment: deployment, TokensPerMinute: ceiling})
	own := New(c.config(quotas)).Join(Share{Deployment: deployment, TokensPerMinute: ceiling})
	drive(t, c, stand, sharedDeployment, shared, own)
	broken := overspends(t, stand, sharedDeployment, 2)
	if !slices.ContainsFunc(broken, func(b string) bool { return strings.Contains(b, "past its quota") }) {
		t.Fatalf("with a limiter of its own for one caller, the check reads %q: it cannot tell a shared limiter from two", broken)
	}
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

// A deployment's rate is its quota from DISPATCH_MODEL_DEPLOYMENT_QUOTAS, else the quota a
// caller's setting states for it (the answerer's), else the largest ceiling any caller sets; a
// deployment nothing sets a rate for is not paced.
func TestADeploymentsRateIsItsQuotaElseTheStatedElseTheLargestCeiling(t *testing.T) {
	deployment := Deployment{Kind: "azure", Host: "models.example", Name: "chat"}
	for name, row := range map[string]struct {
		quotas map[string]int
		shares []Share
		want   rate.Limit
	}{
		"the quota": {
			quotas: map[string]int{"chat": 6000},
			shares: []Share{{TokensPerMinute: 9000, StatesQuota: true}, {TokensPerMinute: 12000}},
			want:   100,
		},
		"the stated quota, the answerer's": {
			shares: []Share{{TokensPerMinute: 3000}, {TokensPerMinute: 6000, StatesQuota: true}, {TokensPerMinute: 12000}},
			want:   100,
		},
		"the largest ceiling": {
			shares: []Share{{TokensPerMinute: 3000}, {TokensPerMinute: 6000}},
			want:   100,
		},
		"a quota of another deployment": {
			quotas: map[string]int{"other": 60},
			shares: []Share{{TokensPerMinute: 6000}},
			want:   100,
		},
		"nothing": {
			shares: []Share{{}},
			want:   rate.Inf,
		},
	} {
		t.Run(name, func(t *testing.T) {
			pacer := New(newClock().config(row.quotas))
			for _, share := range row.shares {
				share.Deployment = deployment
				pacer.Join(share)
			}
			if got := pacer.deployments[deployment].limiter.Limit(); got != row.want {
				t.Errorf("rate = %v tokens a second, want %v", got, row.want)
			}
			if len(pacer.deployments) != 1 {
				t.Errorf("%d limiters, want the deployment's one", len(pacer.deployments))
			}
		})
	}
}

// Two deployments are two limiters: a caller of one spends nothing of the other's quota.
func TestEachDeploymentHasALimiterOfItsOwn(t *testing.T) {
	pacer := New(newClock().config(nil))
	one := pacer.Join(Share{Deployment: Deployment{Kind: "azure", Host: "models.example", Name: "chat"}, TokensPerMinute: 600})
	other := pacer.Join(Share{Deployment: Deployment{Kind: "anthropic", Host: "models.example", Name: "chat"}, TokensPerMinute: 600})
	if one.deployment == other.deployment {
		t.Fatal("two kinds of one name share a limiter, want one each")
	}
	if err := one.Wait(t.Context(), 600); err != nil {
		t.Fatal(err)
	}
	if !other.deployment.AllowN(newClock().now, 600) {
		t.Error("a call to the other deployment waits on the first's spending")
	}
}

// A call the quota cannot serve inside the time its context has left is refused, not waited on,
// and gives its tokens back, so the next call that fits is served at once; a call past a minute's
// whole quota is refused too.
func TestWaitRefusesACallPastItsDeadlineAndGivesItsTokensBack(t *testing.T) {
	c := newClock()
	pacer := New(c.config(map[string]int{"chat": 600}))
	caller := pacer.Join(Share{Deployment: Deployment{Name: "chat"}, TokensPerMinute: 600})
	if err := caller.Wait(t.Context(), 400); err != nil || c.slept != 0 {
		t.Fatalf("the first call: %v, slept %s, want served at once from the burst", err, c.slept)
	}
	ctx, cancel := context.WithDeadline(t.Context(), c.now.Add(time.Second))
	defer cancel()
	if err := caller.Wait(ctx, 400); !errors.Is(err, ErrPastDeadline) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a call that would wait 20s with 1s left: %v, want ErrPastDeadline", err)
	}
	if err := caller.Wait(t.Context(), 200); err != nil || c.slept != 0 {
		t.Errorf("a call inside what is left: %v, slept %s, want served at once", err, c.slept)
	}
	if err := caller.Wait(t.Context(), 601); !errors.Is(err, ErrPastDeadline) {
		t.Errorf("a call past a minute's quota: %v, want ErrPastDeadline", err)
	}
}
