package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// decidePath is the path a decision is posted to, under the endpoint's base URL.
const decidePath = "/v1/systemone"

// answerLimit is the most of a decision's answer that is read. An answer is a handful of small
// objects, so a body this long is an endpoint misbehaving rather than an answer, and the caller
// is told the answer is malformed — or, where the status failed, what that status was — rather
// than made to hold whatever the endpoint sends.
const answerLimit = 8 << 20

// Question is one question put to System One. Type is the question's shape and decides what
// Criteria holds: an object of choice names and what each means for a choice question, an ordered
// array of the score levels for a score question, an object of a true and a false description for
// a noul question, and nothing for a box question. A question is asked under a name the caller
// chooses, and the answer comes back under that same name.
type Question struct {
	// Type is the question's shape: noul for a yes/no question or a statement, choice for a
	// selection among named choices, score for a rating against an ordered rubric, box for a
	// location in an image.
	Type string `json:"type"`
	// Instructions is what the model is asked of the state, and may be left empty for a
	// question its criteria state in full.
	Instructions string `json:"instructions,omitempty"`
	// Criteria is the question's criteria in the shape its Type takes, as JSON. It is
	// required for a choice and a score question and empty for a box question.
	Criteria json.RawMessage `json:"criteria,omitempty"`
}

// DecideRequest is one call to System One: the model it is put to, the content the questions are
// about, the questions themselves and the time the call has.
type DecideRequest struct {
	// Model is the model the questions are put to, at an endpoint of kind SystemOne, with
	// what it charges.
	Model Model
	// State is the content every question of the request is about, as JSON: a string, an
	// object or an array. A nil State is sent as JSON null, which the API refuses; any other
	// State rides to the API exactly as it is written here.
	State json.RawMessage
	// Questions are the questions, each under the name its answer comes back under. A
	// request asks at least one.
	Questions map[string]Question
	// Bounds is the limit of the call, of which only Time is read: a decision makes one
	// request, with no loop to count and no stream to cut. Reaching it ends the call with
	// FaultTime rather than an error.
	Bounds Bounds
}

// Decision is one answer, its fields the API's own properties. Type says which of them carry
// meaning, and is the type of the question the answer answers: a noul answer uses Noul, the
// probability of yes; a choice answer uses Choice, Confidence and Probabilities; a score answer
// uses Score, Confidence, Legend and Probabilities; and a box answer uses Box and Confidence.
// Legend is kept as the JSON the API sent, since its levels are the question's criteria in
// whatever shape the caller wrote them, to be read by a caller that knows the question it asked.
type Decision struct {
	// Type is the answer's shape: noul, choice, score or box.
	Type string `json:"type"`
	// Choice is the name of the choice a choice answer selected.
	Choice string `json:"choice,omitempty"`
	// Score is a score answer's expected score, the probability-weighted average of the
	// rubric's levels, which can fall between two of them: from 0 to the top level's number.
	Score float64 `json:"score,omitempty"`
	// Noul is a noul answer's probability that the answer is yes, or the statement true,
	// from 0 to 1.
	Noul float64 `json:"noul,omitempty"`
	// Box is a box answer's edges, and nil for an answer of any other type.
	Box *Box `json:"box,omitempty"`
	// Legend is a score answer's rubric: the question's criteria keyed by the score level
	// each one stands for, each level a string, an object or an array, as the criteria wrote
	// it. It is the answer's data (readDecisions), which this package never prints.
	Legend json.RawMessage `json:"legend,omitempty"`
	// Confidence is how sure the model is of a choice, a score or a box, from 0 to 1. It is
	// zero where the model reported none.
	Confidence float64 `json:"confidence,omitempty"`
	// Probabilities is how likely each alternative was: keyed by choice name for a choice
	// answer and by score level for a score answer.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Box is where a box answer found what its question asked for: its four edges, each a fraction
// of the image from 0 to 1, Top and Bottom of its height from its top and Left and Right of its
// width from its left, and each the expected value of the position the model predicts for it.
type Box struct {
	Top    float64 `json:"top"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
	Right  float64 `json:"right"`
}

// DecideResult is the outcome of one DecideRequest, and every outcome is one: a call that ended
// on a fault reports that fault rather than an error.
type DecideResult struct {
	// Answers are the answers, keyed by the names the questions were asked under, and are
	// empty on a call that ended on a fault.
	Answers map[string]Decision
	// Usage is what the call consumed and what it cost at Model.Price.
	Usage Usage
	// Fault is why the call ended short of an answer, and is empty on a call that answered.
	// Detail says what happened, for a log rather than for a wire.
	Fault  Fault
	Detail string
}

// systemOneRequest is the body of a decision: the model, the state the questions are about and
// the questions themselves. This first cut sends no assets, so it sends neither of the API's
// danger-zone headers either, and a box question has no image to find anything in.
type systemOneRequest struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// systemOneResponse is the body of an answered decision as Answer.Decode reads it: every member
// but each score answer's legend, which readDecisions reads as data. Usage is a pointer so that a
// body without the usage the API always sends is read as the wrong shape rather than as a free
// call.
type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   *systemOneUsage            `json:"usage"`
}

// systemOneAnswer is one answer of the body as Answer.Decode reads it: a Decision's members but
// its Legend.
type systemOneAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Noul          float64            `json:"noul"`
	Box           *Box               `json:"box"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// decision is the Decision a is, with legend, the answer's legend as readDecisions read it.
func (a systemOneAnswer) decision(legend json.RawMessage) Decision {
	return Decision{Type: a.Type, Choice: a.Choice, Score: a.Score, Noul: a.Noul, Box: a.Box,
		Legend: legend, Confidence: a.Confidence, Probabilities: a.Probabilities}
}

// systemOneUsage is what the API reports one decision consumed. It counts neither cached nor
// reasoning tokens, so neither is priced.
type systemOneUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Decide puts one set of questions to System One and reads the answers back: one POST, with no
// history, no tools and no stream, through the same transport every model call of this package
// goes through, so the bearer header, the endpoint's query parameters and the one retry are the
// same here as in a run. Every outcome of the request is a DecideResult: the answers and what
// they cost, or a Fault saying why no answer came. The error is for a request that cannot be made
// at all and for the caller's context ending.
func Decide(ctx context.Context, req DecideRequest) (DecideResult, error) {
	if err := req.validate(); err != nil {
		return DecideResult{}, err
	}
	body, err := json.Marshal(systemOneRequest{Model: req.Model.ID, State: req.State, Questions: req.Questions})
	if err != nil {
		return DecideResult{}, fmt.Errorf("agentloop: the request's state and questions: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, req.Bounds.Time)
	defer cancel()
	url := strings.TrimSuffix(req.Model.Endpoint.BaseURL, "/") + decidePath
	post, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return DecideResult{}, fmt.Errorf("agentloop: the %s endpoint: %w", SystemOne, err)
	}
	post.Header.Set("Content-Type", "application/json")
	client := newModelTransport(req.Model.Endpoint).client()

	resp, err := client.Do(post)
	var answer redact.Answer
	tooLarge := false
	if err != nil {
		// net/http's error can quote the answering party's words, which can echo the bearer: a
		// Location that does not parse, read before the redirect check, and a refused redirect's
		// Location in full. So the call's failure is said as the endpoint's address allows
		// (redact.Cause): a transport failure's own text, the redirect's status, or its class.
		err = redact.Cause(err, req.Model.Endpoint.BaseURL)
	} else {
		answer, err = redact.ReadAnswer(resp, answerLimit)
		_ = resp.Body.Close()
		tooLarge = errors.Is(err, redact.ErrAnswerTooLarge)
		if tooLarge {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("reading the answer: %w", err)
		}
	}
	var result DecideResult
	switch {
	case ctx.Err() != nil:
		return result, fmt.Errorf("agentloop: %w", ctx.Err())
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		result.Fault, result.Detail = FaultTime, fmt.Sprintf("the decision went past %s", req.Bounds.Time)
	case err != nil:
		result.Fault, result.Detail = FaultProviderError, err.Error()
	case resp.StatusCode/100 != 2:
		result.Fault, result.Detail = FaultProviderError, fmt.Sprintf("%s: %.500q", redact.StatusLine(resp.StatusCode), answer.Words().String())
	case tooLarge:
		result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("the answer went past the %d MiB a decision reads", answerLimit>>20)
	default:
		switch answers, usage, err := readDecisions(answer); {
		case err != nil:
			result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("%v: %.500q", err, answer.Words().String())
		case len(answers) == 0 || usage == nil:
			result.Fault, result.Detail = FaultMalformed, fmt.Sprintf("the answer holds no answers or no usage: %.500q", answer.Words().String())
		default:
			result.Answers = answers
			result.Usage = usage.priced(req.Model.Price)
		}
	}
	return result, nil
}

// readDecisions is the answers and the usage of an answered decision. Every member is read through
// Answer.Decode but each score answer's legend, whose levels are the question's criteria in
// whatever shape the caller wrote them, which only the JSON itself holds: the legends are read as
// the answer's data (Answer.StreamTo), and a body they do not decode from is named in fixed text
// (redact.DecodeError) that quotes no value of it.
func readDecisions(answer redact.Answer) (map[string]Decision, *systemOneUsage, error) {
	var decided systemOneResponse
	if err := answer.Decode(&decided); err != nil {
		return nil, nil, err
	}
	var body bytes.Buffer
	if _, err := answer.StreamTo(&body); err != nil {
		return nil, nil, fmt.Errorf("reading the answer's legends: %w", err)
	}
	var legends struct {
		Answers map[string]struct {
			Legend json.RawMessage `json:"legend"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body.Bytes(), &legends); err != nil {
		return nil, nil, redact.DecodeError(err, body.Bytes(), &legends)
	}
	answers := make(map[string]Decision, len(decided.Answers))
	for name, a := range decided.Answers {
		answers[name] = a.decision(legends.Answers[name].Legend)
	}
	return answers, decided.Usage, nil
}

// validate is whether the request is one that can be made at all: it has its time bound, an
// endpoint of the one kind that answers it and reads a token, a model, and questions that are
// each of some type.
func (req DecideRequest) validate() error {
	switch {
	case req.Bounds.Time <= 0:
		return errors.New("agentloop: the request has no time bound")
	case req.Model.Endpoint.Kind != SystemOne:
		return fmt.Errorf("agentloop: no decision is made on the %s protocol", req.Model.Endpoint.Kind)
	}
	if err := req.Model.validate(); err != nil {
		return err
	}
	if len(req.Questions) == 0 {
		return errors.New("agentloop: the request asks no question")
	}
	// Sorted, so a request with more than one such question names the same one every time.
	for _, name := range slices.Sorted(maps.Keys(req.Questions)) {
		if req.Questions[name].Type == "" {
			return fmt.Errorf("agentloop: question %s has no type", name)
		}
	}
	return nil
}

// priced is one decision's usage and its cost at price, USD per million tokens. The API reports
// no cache and no reasoning counts, so neither is counted, and its usage is part of every answer,
// so the usage is always metered.
func (u systemOneUsage) priced(price Price) Usage {
	counts := Usage{
		Input:   u.InputTokens,
		Output:  u.OutputTokens,
		Metered: true,
	}
	counts.CostUSD = price.cost(counts)
	return counts
}
