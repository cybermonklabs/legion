package approval_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// retagOnAsk is a projectaccess.Access that, once armed, runs retag the first time it is asked
// about project, before it passes the question on: a policy refresh that lands while the page's
// pending list asks Dispatch about one of its rows. It runs on the broker's handler goroutine, so
// it keeps retag's error for the test to read rather than failing t there.
type retagOnAsk struct {
	inner   projectaccess.Access
	project string
	retag   func() error
	armed   atomic.Bool
	once    sync.Once
	fired   atomic.Bool
	err     error
}

func (r *retagOnAsk) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if project == r.project && r.armed.Load() {
		r.once.Do(func() {
			r.err = r.retag()
			r.fired.Store(true)
		})
	}
	return r.inner.Access(ctx, project, person)
}

func (r *retagOnAsk) MemberKind(ctx context.Context, person string) (string, error) {
	return r.inner.MemberKind(ctx, person)
}

func (r *retagOnAsk) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return r.inner.States(ctx, projects)
}

// TestThePagePrintsNoOtherRequestOnAPolicyItsRuleDidNotCheck is the post-merge review's second
// finding on cybermonklabs/legion#4135 at the page: two of ben's requests for SHARED_SECRET wait
// on his page, the other naming TEAM-1, and ben is on no list of PRIV's. While the pending list
// asks Dispatch about TEAM for that other request, the policy tags SHARED_SECRET with PRIV. The
// page then prints that request nowhere and says nothing of PRIV, since the record it would print
// is read on the policy its person rule checks (requests.Machine.ReadRecordFor), not on one read
// after the list's rule did.
func TestThePagePrintsNoOtherRequestOnAPolicyItsRuleDidNotCheck(t *testing.T) {
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privProject: {Visibility: "private", Owner: ada},
	}, nil)
	front := &retagOnAsk{inner: projectaccess.New(dispatch.URL, dispatch.Account, nil), project: "TEAM"}
	r := newRig(t, func(s *rigSetup) { s.projects = front })
	front.retag = func() error {
		tagged := policytest.Secret("SHARED_SECRET", policy.OwnerShared, policy.TierHuman, "shared-v1")
		tagged.Tags[policy.TagProject] = privProject
		r.machine.Secrets.(aws.Store).Client.(*awstest.Local).Put(tagged)
		return r.machine.Policy.Refresh(context.Background())
	}
	mainSession, mainKey := r.benSession(t, "ben-main-box")
	main := r.request(t, mainSession, mainKey, "rotate the wiki key", nil, "", "SHARED_SECRET")
	otherSession, otherKey := r.benSession(t, "ben-other-box")
	other := r.request(t, otherSession, otherKey, "TEAM-1: rotate the deploy key", nil, "", "SHARED_SECRET")

	b := r.browser(t)
	b.SignIn(t, main.recordID, person(ben))
	front.armed.Store(true)
	page := b.Page(t, main.recordID)
	if !front.fired.Load() || front.err != nil {
		t.Fatalf("the policy was retagged while the page's list asked about TEAM: %v, %v; want true, nil, or the test is blind", front.fired.Load(), front.err)
	}
	if strings.Contains(page.Body, other.recordID) {
		t.Errorf("ben's page prints %s, whose secret is PRIV's now, which ben may not see:\n%s", other.recordID, pageText(t, page.Body))
	}
	if text := pageText(t, page.Body); strings.Contains(text, privProject) {
		t.Errorf("ben's page says PRIV, a project nobody asked whether he may see:\n%s", text)
	}
}
