package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// debugMessagesHandler is the process's log handler with the DEBUG records whose message is one of
// messages let through as well: what DISPATCH_TEST_DEBUG_MESSAGES names, under the test hooks, for
// an end-to-end test that waits on those lines, such as a loop's line for each pass it makes. Every
// other DEBUG record is dropped, as the handler beneath drops it, so the test turns on the lines it
// reads and no others.
type debugMessagesHandler struct {
	slog.Handler
	messages map[string]bool
}

// Enabled is true at DEBUG, whose records Handle sorts by message, since the message is not known
// yet here, and the handler beneath's answer at every level above it.
func (h debugMessagesHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < slog.LevelInfo {
		return level >= slog.LevelDebug
	}
	return h.Handler.Enabled(ctx, level)
}

// Handle passes record to the handler beneath unless it is a DEBUG record messages does not name.
func (h debugMessagesHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level < slog.LevelInfo && !h.messages[record.Message] {
		return nil
	}
	return h.Handler.Handle(ctx, record)
}

func (h debugMessagesHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return debugMessagesHandler{Handler: h.Handler.WithAttrs(attrs), messages: h.messages}
}

func (h debugMessagesHandler) WithGroup(name string) slog.Handler {
	return debugMessagesHandler{Handler: h.Handler.WithGroup(name), messages: h.messages}
}

// testHookMessages reads name, a list of log messages separated by "|", as the set of DEBUG
// messages a test process writes (debugMessagesHandler): nil when it is unset, and an error when
// it is set without the test hooks.
func testHookMessages(env settingValues, name string, testHooksEnabled bool) (map[string]bool, error) {
	raw := strings.TrimSpace(env.get(name))
	if raw == "" {
		return nil, nil
	}
	if !testHooksEnabled {
		return nil, fmt.Errorf("DISPATCH_TEST_HOOKS=1 required with %s", name)
	}
	messages := map[string]bool{}
	for message := range strings.SplitSeq(raw, "|") {
		if message = strings.TrimSpace(message); message != "" {
			messages[message] = true
		}
	}
	return messages, nil
}
