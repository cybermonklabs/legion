package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/eval"
)

// personHeader is the header the eval's server names its person by (DISPATCH_IDENTITY).
const personHeader = "X-Dispatch-User"

// person is the person of a try at the try's server: every request names them in personHeader,
// so each run they start acts with their sign-in.
type person struct {
	origin string
	login  string
	client *http.Client
}

// call sends one request as the person and decodes its JSON answer into out, refusing any status
// but want.
func (p *person) call(ctx context.Context, method, path string, body, out any, want int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.origin+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set(personHeader, p.login)
	request.Header.Set("Origin", p.origin)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if response.StatusCode != want {
		return fmt.Errorf("%s %s answered %d: %s", method, path, response.StatusCode, clipText(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// runRead is a run as GET /api/v1/going/runs/{id} answers it.
type runRead struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	StopReason *string `json:"stop_reason"`
	Used       struct {
		Calls     int   `json:"calls"`
		WorkingMS int64 `json:"working_ms"`
		PaceMS    int64 `json:"pace_ms"`
		Tokens    int   `json:"tokens"`
	} `json:"used"`
}

// planRead is a run's plan as GET /api/v1/going/runs/{id}/plan answers it.
type planRead struct {
	Plan  int    `json:"plan"`
	State string `json:"state"`
	Rows  []struct {
		Row   int             `json:"row"`
		Tool  string          `json:"tool"`
		State string          `json:"state"`
		Args  json.RawMessage `json:"arguments"`
	} `json:"rows"`
}

// contextOf is the context chip of a page: its path, and the project and the issue it shows.
func contextOf(page string) map[string]string {
	chip := map[string]string{"path": page}
	parts := strings.Split(strings.TrimPrefix(page, "/"), "/")
	switch {
	case len(parts) >= 2 && parts[0] == "issues":
		chip["issue"] = parts[1]
		chip["project"], _, _ = strings.Cut(parts[1], "-")
	case len(parts) >= 2 && parts[0] == "projects":
		chip["project"] = parts[1]
	}
	return chip
}

// tryOutcome is what the person met in one run: how it ended, the plans they answered, what the
// run spent, its answer's prose and the blocks it drew. Answer is the text of the run's answer
// frames, the reply frames that name no tool, which the bar shows as the answer; HeaderText is the
// text of its other reply frames, the step headers the bar draws over the calls they name and
// never as the answer (readFrames); and joined is the text of every reply frame in its order,
// which the eval read as the answer before it read the answer frames alone (gradeOldRule).
type tryOutcome struct {
	Run        string   `json:"run"`
	State      string   `json:"state"`
	Reason     string   `json:"reason,omitempty"`
	Plans      []string `json:"plans,omitempty"`
	Calls      int      `json:"calls"`
	Seconds    float64  `json:"seconds"`
	Answer     string   `json:"answer"`
	HeaderText string   `json:"header_text,omitempty"`
	joined     string
	Blocks     []json.RawMessage `json:"blocks,omitempty"`
	Timedout   bool              `json:"timed_out,omitempty"`
	// WorkingMS is the run's working time over all its turns, and PaceMS how much of it the
	// agent's pacer held the run's model calls before they were sent, as the run's budget_used
	// holds them: a run stopped at its level's time (agent.ReasonTimeBudget) says by PaceMS how
	// much of that time it waited for the model's quota.
	WorkingMS int64 `json:"working_ms"`
	PaceMS    int64 `json:"pace_ms"`
	// Turns is how many turns of the run ended (its end frames); ModelCalls is each model call of
	// the run, in order, and Checks each decision the no-plan guard made on a reply in prose, a
	// check or a skip, each read off the reply frame of the model call it belongs to (readFrame).
	Turns      int          `json:"turns"`
	ModelCalls []modelCall  `json:"model_calls,omitempty"`
	Checks     []proseCheck `json:"checks,omitempty"`
}

// modelCall is one model call of a run as its reply frame says it: the tokens the provider
// generated for its reply, its reasoning among them (output_tokens), and of those the tokens that
// were reasoning (reasoning_tokens).
type modelCall struct {
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
}

// proseCheck is one decision the no-plan guard made on a reply in prose before the reply could end
// its turn (going/agent's endProse), as the reply's frame says it: the turn of the run it was made
// in, counted from 1; for a check (checkProse), its milliseconds (check_ms), which the run's
// working time counts, and the tokens it spent, its prompt's and its reply's (check_tokens); for a
// check the guard skipped, why (check_skipped), with no time; and what the decision model Jev
// read the turn's words as when the guard decided (jev_line): ask, do, file, none for no line,
// late for a line call still out, "" where the guard asked Jev nothing.
type proseCheck struct {
	Turn    int    `json:"turn"`
	MS      int64  `json:"ms"`
	Tokens  int    `json:"tokens"`
	Skipped string `json:"skipped,omitempty"`
	Jev     string `json:"jev,omitempty"`
}

// mostPlans is how many plans the person answers in one run; a run that proposes more is stopped.
const mostPlans = 6

// drive is the person's side of one try: they start a run with the task's words on its page, in
// its mode and at its level, answer each plan the run shows them as the task's grade says
// (approve it whole, or reject it where Grade.Approve is false), and read the run to its end,
// then read every frame it stored. A run still going past twice its level's working time and five
// minutes more is stopped.
func (p *person) drive(ctx context.Context, task eval.Task) (tryOutcome, error) {
	started := time.Now()
	var run runRead
	if err := p.call(ctx, http.MethodPost, "/api/v1/going/runs", map[string]any{
		"words": task.Words, "context": contextOf(task.Page), "level": string(task.Level), "mode": string(task.Mode),
	}, &run, http.StatusCreated); err != nil {
		return tryOutcome{}, fmt.Errorf("start the run: %w", err)
	}
	outcome := tryOutcome{Run: run.ID}
	deadline := started.Add(2*agent.Levels[string(task.Level)].Time + 5*time.Minute)
	answered := map[int]bool{}
	for {
		if err := p.call(ctx, http.MethodGet, "/api/v1/going/runs/"+run.ID, nil, &run, http.StatusOK); err != nil {
			return outcome, fmt.Errorf("read the run: %w", err)
		}
		if run.State == "done" || run.State == "stopped" || run.State == "failed" {
			break
		}
		if time.Now().After(deadline) || len(answered) > mostPlans {
			outcome.Timedout = true
			if err := p.call(ctx, http.MethodPost, "/api/v1/going/runs/"+run.ID+"/stop", nil, &run, http.StatusOK); err != nil {
				return outcome, fmt.Errorf("stop the run: %w", err)
			}
			continue
		}
		if run.State == "waiting" {
			if err := p.answerPlan(ctx, task, run.ID, answered, &outcome); err != nil {
				return outcome, err
			}
		}
		select {
		case <-ctx.Done():
			return outcome, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	outcome.State = run.State
	if run.StopReason != nil {
		outcome.Reason = *run.StopReason
	}
	outcome.Calls = run.Used.Calls
	outcome.WorkingMS, outcome.PaceMS = run.Used.WorkingMS, run.Used.PaceMS
	outcome.Seconds = time.Since(started).Seconds()
	if err := p.readFrames(ctx, run.ID, &outcome); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// answerPlan answers the run's waiting plan as the task's grade says, once a plan.
func (p *person) answerPlan(ctx context.Context, task eval.Task, run string, answered map[int]bool, outcome *tryOutcome) error {
	var plan planRead
	if err := p.call(ctx, http.MethodGet, "/api/v1/going/runs/"+run+"/plan", nil, &plan, http.StatusOK); err != nil {
		return fmt.Errorf("read the plan: %w", err)
	}
	if plan.State != "waiting" {
		return nil
	}
	approve := task.Grade.Approve == nil || *task.Grade.Approve
	verb := "approve"
	if !approve {
		verb = "reject"
	}
	var tools []string
	for _, row := range plan.Rows {
		tools = append(tools, row.Tool)
	}
	outcome.Plans = append(outcome.Plans, fmt.Sprintf("%s plan %d: %s", verb, plan.Plan, strings.Join(tools, ", ")))
	answered[plan.Plan] = true
	err := p.call(ctx, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/"+verb, map[string]any{"plan": plan.Plan}, nil, http.StatusOK)
	if err != nil && strings.Contains(err.Error(), "GOING_PLAN_NOT_WAITING") {
		return nil
	}
	return err
}

// readFrames reads every frame the run stored from its events stream, reading it again after the
// last frame it holds when the stream closes before the run's end, each as readFrame takes it: the
// prose of each answer frame, joined in order, is the answer, and that of each step header, joined
// in order, the header text (tryOutcome).
func (p *person) readFrames(ctx context.Context, run string, outcome *tryOutcome) error {
	last := ""
	var answers, headers, replies []string
	for range 20 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.origin+"/api/v1/going/runs/"+run+"/events", nil)
		if err != nil {
			return err
		}
		request.Header.Set(personHeader, p.login)
		request.Header.Set("Accept", "text/event-stream")
		if last != "" {
			request.Header.Set("Last-Event-ID", last)
		}
		response, err := p.client.Do(request)
		if err != nil {
			return fmt.Errorf("read the run's events: %w", err)
		}
		closedIdle := false
		err = sseFrames(response.Body, func(id, name string, data []byte) {
			if id != "" {
				last = id
			}
			closedIdle = name == "keepalive"
			if prose, header := outcome.readFrame(name, data); prose != "" {
				replies = append(replies, prose)
				if header {
					headers = append(headers, prose)
				} else {
					answers = append(answers, prose)
				}
			}
		})
		_ = response.Body.Close()
		if err != nil {
			return fmt.Errorf("read the run's events: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("read the run's events: %d", response.StatusCode)
		}
		// The stream of an ended run closes once it has written every frame; one that closed
		// idle, at a keepalive, is read again after the last frame it wrote.
		if !closedIdle {
			break
		}
	}
	outcome.Answer, outcome.HeaderText, outcome.joined = strings.Join(answers, "\n\n"), strings.Join(headers, "\n\n"), strings.Join(replies, "\n\n")
	return nil
}

// readFrame takes one frame the run stored into the outcome and answers the prose of a reply
// frame, "" for none, and whether that frame names tools: a step header, which the bar draws over
// the calls it names and never as the answer, where a reply frame that names none is an answer
// frame (the server's goingKeepLinkWords, the web's features/going/run-frames.ts). Each reply
// frame is one model call of the run, whose tokens go to ModelCalls, and whose check by the
// no-plan guard, where its frame says one (check_ms), or whose skipped check (check_skipped), goes
// to Checks, under the turn it was made in, with what Jev read the words as; each block frame is a
// block the answer drew; and each end frame ends a turn.
func (o *tryOutcome) readFrame(name string, data []byte) (string, bool) {
	switch name {
	case "reply":
		var reply struct {
			Text            string   `json:"text"`
			Tools           []string `json:"tools"`
			OutputTokens    int      `json:"output_tokens"`
			ReasoningTokens int      `json:"reasoning_tokens"`
			CheckMS         *int64   `json:"check_ms"`
			CheckTokens     int      `json:"check_tokens"`
			CheckSkipped    string   `json:"check_skipped"`
			JevLine         string   `json:"jev_line"`
		}
		if json.Unmarshal(data, &reply) != nil {
			return "", false
		}
		o.ModelCalls = append(o.ModelCalls, modelCall{OutputTokens: reply.OutputTokens, ReasoningTokens: reply.ReasoningTokens})
		switch {
		case reply.CheckMS != nil:
			o.Checks = append(o.Checks, proseCheck{Turn: o.Turns + 1, MS: *reply.CheckMS, Tokens: reply.CheckTokens, Jev: reply.JevLine})
		case reply.CheckSkipped != "":
			o.Checks = append(o.Checks, proseCheck{Turn: o.Turns + 1, Skipped: reply.CheckSkipped, Jev: reply.JevLine})
		}
		return strings.TrimSpace(reply.Text), len(reply.Tools) > 0
	case "block":
		o.Blocks = append(o.Blocks, json.RawMessage(bytes.Clone(data)))
	case agentwire.FrameEnd:
		o.Turns++
	}
	return "", false
}
