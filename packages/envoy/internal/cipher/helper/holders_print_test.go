//go:build linux

package helper

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type brokerErrorTwin struct {
	Status   int
	code     string
	said     string
	withheld bool
}

func (e *brokerErrorTwin) Error() string {
	return fmt.Sprintf("broker %d %s: %s", e.Status, e.code, e.said)
}

type brokerErrorExported struct {
	Status   int
	Code     string
	Said     string
	Withheld bool
}

type leaseReplyTwin struct {
	EnrollmentID   string    `json:"enrollment_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	lease := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	type doTwin = struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "helper.BrokerError",
			Kind:     "error",
			Boxed:    newBrokerError(403, code, words, false),
			Same:     &brokerErrorTwin{403, redacttest.Code, redacttest.Words, false},
			Exported: &brokerErrorExported{403, redacttest.Code, redacttest.Words, false},
		},
		{
			Name: "helper.Broker.do.envelope",
			Kind: "fields",
			Boxed: struct {
				Code  redact.RemoteText `json:"code"`
				Error redact.RemoteText `json:"error"`
			}{code, words},
			Same:     doTwin{redacttest.Code, redacttest.Words},
			Exported: doTwin{redacttest.Code, redacttest.Words},
		},
		{
			Name:     "helper.leaseReply",
			Kind:     "fields",
			Boxed:    leaseReply{EnrollmentID: words, LeaseExpiresAt: lease},
			Same:     leaseReplyTwin{redacttest.Words, lease},
			Exported: leaseReplyTwin{redacttest.Words, lease},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
