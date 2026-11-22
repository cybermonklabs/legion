package machine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAMachineLoginsReasonNamesAtMostSixteenProjects is round 4's B9 on cybermonklabs/legion#4135
// for the machine login: a new login is held to the reason rule a new secret request is
// (record.CheckReasonProjects), so no request of either kind is created whose reason names the
// issues of more than record.MaxReasonProjects projects. One at the bound opens as before.
func TestAMachineLoginsReasonNamesAtMostSixteenProjects(t *testing.T) {
	svc := newFixture(t)
	for _, c := range []struct {
		projects int
		refused  bool
	}{
		{record.MaxReasonProjects + 1, true},
		{record.MaxReasonProjects, false},
	} {
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
		if refused := errors.Is(err, record.ErrRequestInvalid); refused != c.refused {
			t.Errorf("LoginDevice(a reason naming %d projects) = %v; want refused %v", c.projects, err, c.refused)
		}
	}
}
