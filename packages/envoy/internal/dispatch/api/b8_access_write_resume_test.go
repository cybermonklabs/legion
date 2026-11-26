package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAResumeAcrossTheLastPrivateProjectMadePublicIsNotComplete: PRIV, the one private project, is
// made public. Each open caller that read PUB alone now sees every object (seesEveryObject), PRIV
// among them, which its page, read while PRIV was private, lacks; so its resume from before the
// change says replay_complete false, the frame on which the page reads everything again. A project
// made public is the one access write a reader that sees every object owes a refresh for.
//
// Control: the resume check asks nothing of a reader that sees every object; each resume is
// complete and the test fails.
func TestAResumeAcrossTheLastPrivateProjectMadePublicIsNotComplete(t *testing.T) {
	seed := seedPrivateProject(t)
	since := eventHead(t, seed.database)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice)); err != nil || !seesEveryObject(reader) {
		t.Fatalf("with PRIV public alice reads as %+v (%v), want a reader that sees every object", reader.Viewer, err)
	}
	url := liveURLOf(t, seed.handler)
	for _, caller := range []privateCaller{callerAlice, callerCarolToken, callerSharedToken} {
		requireResumeComplete(t, seed, url, caller, since, false, "it now sees PRIV, which its page, read while PRIV was private, lacks")
	}
}
