package exp19

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// The environment a helper process is started with. A helper is this same experiments
// binary invoked again with the same -address, -namespace and -work-dir: the role says
// which of the two it is, and the rest is what that role needs. They are environment
// variables rather than flags because cmd/experiments' flags belong to the experiments
// themselves, not to the processes one experiment starts for its own measurement.
const (
	roleEnv   = "EXP19_ROLE"
	queueEnv  = "EXP19_TASK_QUEUE"
	tokenEnv  = "EXP19_TASK_TOKEN"
	resultEnv = "EXP19_RESULT"
	answerEnv = "EXP19_ANSWER_FILE"

	// workerRole hosts the handout activity and is the process the driver kills.
	workerRole = "worker"
	// completerRole holds nothing but a task token: no worker, no registered
	// activity, no workflow. It is the outside process of case B.
	completerRole = "completer"

	// workerPIDLine is how a worker process tells the driver which pid to kill.
	workerPIDPrefix = "exp19worker"
)

// Completion is what a completer process did, written to its answer file because the
// driver is another process. A refusal is not a failure of the helper: it is the
// reading case C exists to take, so it is reported rather than raised.
type Completion struct {
	PID          int    `json:"pid"`
	Accepted     bool   `json:"accepted"`
	ErrorType    string `json:"errorType"`
	ErrorMessage string `json:"errorMessage"`
}

// helper runs this process as one of the experiment's helper processes and returns its
// exit code. It is not a run of the experiment, so nothing it does reaches a verdict
// line: the driver that started it reads what it did from its pid line and its answer
// file.
func helper(ctx context.Context, cfg harness.Config, role string) int {
	var err error
	switch role {
	case workerRole:
		err = serveWorker(ctx, cfg)
	case completerRole:
		err = completeFromToken(ctx, cfg)
	default:
		err = fmt.Errorf("%s=%q is neither %q nor %q", roleEnv, role, workerRole, completerRole)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "exp19 %s: %v\n", role, err)
		return 1
	}
	return 0
}

// serveWorker hosts the handout activity on one task queue and serves until this
// process is killed or its context ends. It is a separate process on purpose: the
// experiment asks what the server does when the worker that took a handed-out activity
// dies, and a worker inside the driver could not be killed without taking the driver
// with it.
func serveWorker(ctx context.Context, cfg harness.Config) error {
	queue := os.Getenv(queueEnv)
	if queue == "" {
		return fmt.Errorf("%s is required", queueEnv)
	}
	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	w := worker.New(c, queue, worker.Options{})
	RegisterActivity(w)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker on %s: %w", queue, err)
	}
	defer w.Stop()

	// The driver reads this line and kills this pid, so it is printed only once the
	// worker is polling. os.Stdout is unbuffered, so the write itself puts the line on
	// the driver's pipe; Sync then pushes it out of the kernel for a file, and reports
	// EINVAL for a pipe, which has nothing to sync.
	fmt.Printf("%s: pid=%d\n", workerPIDPrefix, os.Getpid())
	_ = os.Stdout.Sync()

	<-ctx.Done()
	return nil
}

// completeFromToken completes one Activity Execution with the token it was given, from
// a process that holds nothing else: it registers no activity and runs no worker, so
// what it does is what any program with the token could do.
func completeFromToken(ctx context.Context, cfg harness.Config) error {
	encoded := os.Getenv(tokenEnv)
	if encoded == "" {
		return fmt.Errorf("%s is required", tokenEnv)
	}
	token, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("%s is not base64: %w", tokenEnv, err)
	}
	answerFile := os.Getenv(answerEnv)
	if answerFile == "" {
		return fmt.Errorf("%s is required", answerEnv)
	}

	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	answer := Completion{PID: os.Getpid()}
	if err := c.CompleteActivity(ctx, token, os.Getenv(resultEnv), nil); err != nil {
		answer.ErrorType = fmt.Sprintf("%T", err)
		answer.ErrorMessage = err.Error()
	} else {
		answer.Accepted = true
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return fmt.Errorf("encode the answer: %w", err)
	}
	if err := os.WriteFile(answerFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", answerFile, err)
	}
	return nil
}
