package machine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAMachineLoginsReasonNamesAtMostSixteenProjects is round 4's B9 on cybermonklabs/legion#4135
// for the machine login: on a broker that holds a project-access client, a new login is held to
// the reason rule a new secret request is (record.CheckReasonProjects), so no request of either
// kind is created whose reason names the issues of more than record.MaxReasonProjects projects.
// One at the bound opens as before. A broker with no such client reads no reason's projects, and
// opens a login whose reason names more, as it did before the rule (R9).
func TestAMachineLoginsReasonNamesAtMostSixteenProjects(t *testing.T) {
	for _, c := range []struct {
		name     string
		checks   bool
		projects int
		refused  bool
	}{
		{"a broker that checks projects, past the bound", true, record.MaxReasonProjects + 1, true},
		{"a broker that checks projects, at the bound", true, record.MaxReasonProjects, false},
		{"a broker with no project-access client, past the bound", false, record.MaxReasonProjects + 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := newFixture(t)
			if c.checks {
				dispatch := projectaccesstest.NewServer(t, nil, nil)
				svc.Enroll.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
			}
			keys := make([]string, c.projects)
			for i := range keys {
				keys[i] = fmt.Sprintf("P%02d-1", i)
			}
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			compact, err := record.Sign(key, testAudience, []record.AuthorizationDetail{
				{Type: "launcher_credential", Identifier: "example-host-devbox"},
			}, strings.Join(keys, " "), testApprover, nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			_, err = openLogin(t, svc, compact)
			if refused := errors.Is(err, record.ErrRequestInvalid); refused != c.refused || (!c.refused && err != nil) {
				t.Errorf("LoginDevice(a reason naming %d projects) = %v; want refused %v", c.projects, err, c.refused)
			}
		})
	}
}
