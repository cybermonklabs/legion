package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// Client makes the calls of agent/v1 at the server's socket, under the secret the server gave the
// agent. The agent process and the server's tests use the same client. Each call dials a
// connection of its own and closes it once answered, or once its context ends: a claim or a renew
// that waits holds only its own connection, and the server reads the closed connection as the
// agent no longer waiting.
type Client struct {
	// Path is the server's Unix socket.
	Path string
	// Secret is what the server wrote to the agent's standard input, SecretSize bytes.
	Secret []byte
}

// Hello says the agent started under request's instance id.
func (c Client) Hello(ctx context.Context, request HelloRequest) (HelloAnswer, error) {
	var answer HelloAnswer
	err := c.do(ctx, CallHello, request, &answer)
	return answer, err
}

// Claim waits for a ready run and takes it; a nil Run is none within the server's wait.
func (c Client) Claim(ctx context.Context, request ClaimRequest) (ClaimAnswer, error) {
	var answer ClaimAnswer
	err := c.do(ctx, CallClaim, request, &answer)
	return answer, err
}

// Renew renews the instance's leases, and is answered once the server's wait ends or a run of
// them is stopped.
func (c Client) Renew(ctx context.Context, request RenewRequest) (RenewAnswer, error) {
	var answer RenewAnswer
	err := c.do(ctx, CallRenew, request, &answer)
	return answer, err
}

// Step saves one step of a run.
func (c Client) Step(ctx context.Context, request StepRequest) (StepAnswer, error) {
	var answer StepAnswer
	err := c.do(ctx, CallStep, request, &answer)
	return answer, err
}

// Call makes one call of a run.
func (c Client) Call(ctx context.Context, request CallRequest) (CallAnswer, error) {
	var answer CallAnswer
	err := c.do(ctx, CallCall, request, &answer)
	return answer, err
}

// End ends a run's turn.
func (c Client) End(ctx context.Context, request EndRequest) error {
	var answer EndAnswer
	return c.do(ctx, CallEnd, request, &answer)
}

// do makes one call: it writes the request frame within the call's request limit and reads the
// answer frame within its answer limit, decoding the answer's body into answer. A refusal of the
// server's is an *Error.
func (c Client) do(ctx context.Context, call string, request, answer any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode the %s request: %w", call, err)
	}
	frame, err := json.Marshal(Request{Version: Version, Secret: c.Secret, Call: call, Body: body})
	if err != nil {
		return fmt.Errorf("encode the %s request: %w", call, err)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return fmt.Errorf("dial the server for %s: %w", call, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, bounded := ctx.Deadline(); bounded {
		_ = conn.SetDeadline(deadline)
	}
	if err := WriteFrame(conn, frame, RequestLimit(call)); err != nil {
		return c.cut(ctx, call, "send", err)
	}
	payload, err := ReadFrame(conn, AnswerLimit(call))
	if err != nil {
		return c.cut(ctx, call, "read the answer of", err)
	}
	var envelope Answer
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode the %s answer: %w", call, err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if err := json.Unmarshal(envelope.Body, answer); err != nil {
		return fmt.Errorf("decode the %s answer: %w", call, err)
	}
	return nil
}

// cut is the error of a call whose frame could not be sent or read: the context's own error where
// it ended the call, so a caller that gave up reads its own cancellation.
func (c Client) cut(ctx context.Context, call, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, fmt.Errorf("%s %s: %w", what, call, err))
	}
	return fmt.Errorf("%s %s: %w", what, call, err)
}
