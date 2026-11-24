package policy_test

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// TestAPrivateProjectSecretTagOfAPublicProjectAnswersAsTheMergeBase is the head half of
// c-private-secrets' R9 check: basegrid_test.go's grid with every secret tagged PUB, a public
// project, loaded by a broker that checks projects, and every requester given PUB's state. Each
// cell must answer exactly as the merge base answered the untagged grid
// (testdata/evaluate-merge-base.json): a public project's tag changes no answer.
func TestAPrivateProjectSecretTagOfAPublicProjectAnswersAsTheMergeBase(t *testing.T) {
	store, names := gridSecrets(func(s *awstest.LocalSecret) { s.Tags[policy.TagProject] = pubProject })
	loader := policytest.Loader(store, gridService)
	loader.ProjectChecks = true
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The comparison is blind unless the tag was read: a grid the loader refused whole would
	// answer ErrUnknownSecret in every cell, and one it loaded without the tag would prove nothing
	// about the tag.
	if len(set.Secrets) == 0 {
		t.Fatalf("the tagged grid loaded no secret (refused: %+v)", set.Refused)
	}
	for name, s := range set.Secrets {
		if s.Project != pubProject {
			t.Fatalf("%s loaded with Project %q, want %s: the grid must carry the tag it checks", name, s.Project, pubProject)
		}
	}
	compareWithMergeBase(t, gridAnswers(set, names, func(r policy.Requester) policy.Requester {
		r.ProjectStates = map[string]policy.ProjectState{pubProject: policy.ProjectPublic}
		return r
	}))
}
