// Command approval-message prints the daemon's approval message for each approval it reads: the
// words POST /legion/v1/claims/approval answers, filled by prompts.ApprovalMessage from
// approval.md. The end-to-end capture of the approval message (scripts/e2e/approval-message.sh)
// and its CI copy (packages/pi-legion/test/approval-message-omp.test.ts) answer their stand-in for
// that route with these words, so the words a session is given are the daemon's own and never a
// copy of them.
//
// It reads one JSON array on stdin, each element prompts.Approval's fields
// ({"tree", "spec", "document", "version", "carried"}), and writes one JSON array on stdout, the
// message of each element in the same order:
//
//	go run ./internal/prompts/cmd/approval-message <<<'[{"tree":"ACME-1","spec":"ACME-1","document":"doc","version":3}]'
//
// It is a test fixture, not a product: nothing deployed runs it, and it lives under internal so
// nothing outside this module can import it.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sjawhar/legion/daemon/internal/prompts"
)

func main() {
	var approvals []prompts.Approval
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&approvals); err != nil {
		fmt.Fprintf(os.Stderr, "approval-message: read the approvals on stdin: %v\n", err)
		os.Exit(2)
	}
	messages := make([]string, 0, len(approvals))
	for _, approval := range approvals {
		message, err := prompts.ApprovalMessage(approval)
		if err != nil {
			fmt.Fprintf(os.Stderr, "approval-message: %v\n", err)
			os.Exit(1)
		}
		messages = append(messages, message)
	}
	if err := json.NewEncoder(os.Stdout).Encode(messages); err != nil {
		fmt.Fprintf(os.Stderr, "approval-message: write the messages: %v\n", err)
		os.Exit(1)
	}
}
