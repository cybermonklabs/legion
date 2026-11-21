package api

import (
	"cmp"
	"context"
	"time"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// rewrite_define.go is Rewrite and Define for a person (INFRA-3231's R7, INFRA-3552's R1 to R4):
// the component's Rewrite and Define (internal/dispatch/reviewer) on the server, with the model
// group DISPATCH_REWRITE_DEFINE_MODEL_* names. A person's Rewrite of an ask is ask_rewrite.go, a
// person's Define of words definitions.go, and the `rewrite` every read of an ask, a comment, a
// message and an issue carries text_rewrites.go. Both are on whenever their model group is set,
// because a person chooses to use them; with it unset their routes answer 503.

// defaultSideQuestionWait is how long a rewrite waits for the asking session's answer to its side
// question, and how long a definition takes the session's answer (INFRA-3552's R2 and R4).
const defaultSideQuestionWait = 60 * time.Second

// RewriteDefineConfig is Rewrite and Define as cmd/dispatch configured them.
type RewriteDefineConfig struct {
	// Model is the model group's model, its share of the deployment's pacer and its bounds
	// (DISPATCH_REWRITE_DEFINE_MODEL_*).
	Model reviewer.Model
	// Rules are the rule sets a rewrite and a definition read (DISPATCH_REVIEWER_RULES).
	Rules []reviewer.RuleSet
	// SideQuestionWait is how long a rewrite waits for the asking session's answer to its side
	// question, and how long a definition takes it; zero is defaultSideQuestionWait.
	SideQuestionWait time.Duration
}

// RewriteDefine is Rewrite and Define as one server runs them; a nil *RewriteDefine is a
// deployment that sets no model group for them, whose routes answer 503.
type RewriteDefine struct {
	config RewriteDefineConfig
}

// NewRewriteDefine is Rewrite and Define as cfg describes them, every field but SideQuestionWait
// set by its caller; a zero SideQuestionWait is defaultSideQuestionWait.
func NewRewriteDefine(cfg RewriteDefineConfig) *RewriteDefine {
	cfg.SideQuestionWait = cmp.Or(cfg.SideQuestionWait, defaultSideQuestionWait)
	return &RewriteDefine{config: cfg}
}

// sideQuestionWait is the time a side question's answer is waited for, on a nil *RewriteDefine
// too.
func (rd *RewriteDefine) sideQuestionWait() time.Duration {
	if rd == nil {
		return defaultSideQuestionWait
	}
	return rd.config.SideQuestionWait
}

// sideQuestionSession answers whether writer, the session that asked a question, takes a side
// question now: Envoy holds its record, it has not gone, and its host advertises btw
// (INFRA-3552's R2 and R4). live asks, as a definition does, for a session Envoy reads live; a
// rewrite also asks an unresponsive one, whose delivery path still accepts a side question.
func (s *server) sideQuestionSession(ctx context.Context, writer string, live bool) bool {
	if writer == "" || s.deps.Envoy == nil {
		return false
	}
	// A session the listener holds no record of, or a listener that does not answer, takes no
	// side question: the rewrite or the definition reads the record alone.
	session, err := s.deps.Envoy.Session(ctx, writer)
	switch {
	case err != nil, session.State == dispatchenvoy.StateGone:
		return false
	case live && session.State != dispatchenvoy.StateLive:
		return false
	}
	return hasCapability(session.Capabilities, "btw")
}
