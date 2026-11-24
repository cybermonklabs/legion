package mcpbridge

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type replyTwin struct {
	result  json.RawMessage
	failed  bool
	code    int
	message string
}

type replyExported struct {
	Result  json.RawMessage
	Failed  bool
	Code    int
	Message string
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	refusal := reply{failed: true, code: -32600, message: redact.NewRemoteText(redacttest.Words)}
	pending := make(chan reply, 1)
	pending <- refusal
	sess := &session{serverName: "whatsapp", pending: map[int]chan reply{1: pending}}
	for _, h := range []redacttest.Holder{
		{
			Name:     "mcpbridge.reply",
			Kind:     "struct",
			Boxed:    refusal,
			Same:     replyTwin{failed: true, code: -32600, message: redacttest.Words},
			Exported: replyExported{Failed: true, Code: -32600, Message: redacttest.Words},
		},
		// A session holds its replies in its pending calls' channels, and each server holds its
		// session behind a pointer, below their top level, where no printer follows, so their
		// rows use the pointer kind's own twins. Each holds a lock, so its row prints it through a
		// pointer, never as a value.
		{Name: "mcpbridge.session", Kind: "pointer", Boxed: sess},
		{Name: "mcpbridge.ManagedServer", Kind: "pointer", Boxed: &ManagedServer{cfg: ServerConfig{Name: "whatsapp"}, session: sess}},
		{Name: "mcpbridge.HTTPServer", Kind: "pointer", Boxed: &HTTPServer{cfg: ServerConfig{Name: "whatsapp"}, session: sess}},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
