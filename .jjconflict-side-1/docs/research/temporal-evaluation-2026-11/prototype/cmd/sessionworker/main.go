// Command sessionworker is the worker process experiment 3 kills.
//
// It is a separate process on purpose: the experiment asks what Temporal does when the
// host of a session dies, and a worker inside the driver could not be killed without
// taking the driver with it. It hosts a session worker on one task queue, prints its own
// process id so the driver knows exactly what to kill, and then serves until killed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp3"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// maxConcurrentSessions is how many sessions one of these workers holds at a time. The
// experiment needs one; a handful leaves room to see a second session arrive.
const maxConcurrentSessions = 4

func main() {
	address := flag.String("address", "127.0.0.1:7233", "the Temporal server's gRPC address")
	namespace := flag.String("namespace", "default", "the Temporal namespace to serve")
	taskQueue := flag.String("task-queue", "", "the task queue to poll (required)")
	flag.Parse()

	if *taskQueue == "" {
		fmt.Fprintln(os.Stderr, "sessionworker: -task-queue is required")
		os.Exit(2)
	}
	if err := serve(*address, *namespace, *taskQueue); err != nil {
		fmt.Fprintf(os.Stderr, "sessionworker: %v\n", err)
		os.Exit(1)
	}
}

func serve(address, namespace, taskQueue string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := harness.Dial(ctx, harness.Config{Address: address, Namespace: namespace}, 30*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{
		EnableSessionWorker:               true,
		MaxConcurrentSessionExecutionSize: maxConcurrentSessions,
	})
	w.RegisterActivityWithOptions(exp3.HoldSession, activity.RegisterOptions{Name: exp3.SessionActivityName})
	w.RegisterActivityWithOptions(exp3.Probe, activity.RegisterOptions{Name: exp3.ProbeActivityName})

	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker on %s: %w", taskQueue, err)
	}
	defer w.Stop()

	// The driver reads this line and kills this pid, so it is printed only once the
	// worker is polling. os.Stdout is unbuffered, so the write itself puts the line on
	// the driver's pipe; Sync then pushes it out of the kernel for a file, and reports
	// EINVAL for a pipe, which has nothing to sync.
	fmt.Printf("sessionworker: pid=%d\n", os.Getpid())
	_ = os.Stdout.Sync()

	<-worker.InterruptCh()
	return nil
}
