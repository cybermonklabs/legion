package enroll

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// TestWhileDispatchCannotBeAskedAPersonKeptFullEnrolls is Create's member-kind rule on a Dispatch
// client that keeps each answer in the service's own store (projectaccess.Client.Keep), as the
// broker's does. While Dispatch answers, ines's machine login enrolls and outsider's, a restricted
// member's, enrolls nothing; that client keeps both answers. Dispatch then goes down, and a client
// of a broker process restarted since reads the kept kinds: ines's login enrolls again, with one
// WARN line and one count labelled full, while outsider's, kept restricted, and uma's, never read,
// enroll nothing (*projectaccess.MemberKindUncheckable, which the API answers 503
// MEMBER_KIND_UNCHECKABLE), counted restricted and none.
//
// Control: a client with no keeper refuses ines's enrollment while Dispatch is down, so the outage
// is real. The fault: MemberKind not reading the kept kind; ines's enrollment is refused.
func TestWhileDispatchCannotBeAskedAPersonKeptFullEnrolls(t *testing.T) {
	const (
		outsider = "outsider@outside.example"
		ines     = "ines@example.com"
		unasked  = "uma@example.com"
	)
	ctx := context.Background()
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
		outsider: projectaccess.MemberRestricted,
		ines:     projectaccess.MemberFull,
		unasked:  projectaccess.MemberFull,
	})
	svc := newService(t)
	// keeping is a client of a broker process keeping in svc's store, logging to logs and counting
	// on registry; its clock stands still, so every read it makes falls in one minute whatever the
	// box's load.
	keeping := func(logs *logtest.Recorder, registry *metrics.Registry) *projectaccess.Client {
		client := projectaccess.New(dispatch.URL, dispatch.Account, nil)
		now := time.Now()
		client.UseClock(func() time.Time { return now })
		client.Keep(svc.Store, slog.New(logs), registry)
		return client
	}
	logs, registry := &logtest.Recorder{}, metrics.New()
	enrol := func(cred Credential, runtimeID string) error {
		_, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtimeID, Thumbprint: "tp-" + runtimeID})
		return err
	}
	enrolled := func(t *testing.T, cred Credential) int {
		t.Helper()
		var n int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments where launcher_credential_id=$1`, cred.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	unchecked := func(t *testing.T, kept string) int {
		t.Helper()
		recorder := httptest.NewRecorder()
		registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		series := `broker_member_kind_unchecked_total{kept="` + kept + `"} `
		for sample := range strings.Lines(recorder.Body.String()) {
			if value, ok := strings.CutPrefix(strings.TrimSpace(sample), series); ok {
				n, err := strconv.Atoi(value)
				if err != nil {
					t.Fatalf("the sample %q holds no count", sample)
				}
				return n
			}
		}
		return 0
	}
	inesLogin := mintCredential(t, svc, str(ines), nil, "ines-devbox")
	outsiderLogin := mintCredential(t, svc, str(outsider), nil, "outsider-laptop")
	umaLogin := mintCredential(t, svc, str(unasked), nil, "uma-devbox")

	svc.Projects = keeping(&logtest.Recorder{}, metrics.New())
	if err := enrol(inesLogin, "ines-devbox-1"); err != nil || enrolled(t, inesLogin) != 1 {
		t.Fatalf("Create under %s's machine login while Dispatch answers = %v with %d enrollments, want one", ines, err, enrolled(t, inesLogin))
	}
	if err := enrol(outsiderLogin, "outsider-laptop-1"); !errors.Is(err, ErrRestrictedMember) {
		t.Fatalf("Create under %s's machine login while Dispatch answers = %v, want ErrRestrictedMember", outsider, err)
	}

	dispatch.SetDown(true)
	t.Cleanup(func() { dispatch.SetDown(false) })
	svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	if err := enrol(inesLogin, "ines-devbox-2"); err == nil {
		t.Fatal("a client with no keeper enrolled ines while Dispatch is down: the outage is not real")
	}
	svc.Projects = keeping(logs, registry)

	if err := enrol(inesLogin, "ines-devbox-2"); err != nil || enrolled(t, inesLogin) != 2 {
		t.Fatalf("Create under %s's machine login while Dispatch is down, kept full = %v with %d enrollments, want a second", ines, err, enrolled(t, inesLogin))
	}
	var lines int
	for _, r := range logs.Records() {
		if r.Message == projectaccess.KeptFullMessage {
			lines++
		}
	}
	if counted := unchecked(t, "full"); counted != 1 || lines != 1 {
		t.Fatalf("enrolling %s while Dispatch is down counted %d and logged %d WARN lines; want 1 of each", ines, counted, lines)
	}
	for _, refused := range []struct {
		person string
		login  Credential
		kept   string
	}{{outsider, outsiderLogin, "restricted"}, {unasked, umaLogin, "none"}} {
		err := enrol(refused.login, refused.person+"-down-1")
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != refused.person || enrolled(t, refused.login) != 0 ||
			err.Error() != "whether Dispatch calls "+refused.person+" a restricted member cannot be read: the broker could not ask Dispatch" {
			t.Fatalf("Create under %s's machine login while Dispatch is down, kept %s = %v with %d enrollments, want MemberKindUncheckable naming them and none",
				refused.person, refused.kept, err, enrolled(t, refused.login))
		}
		if got := unchecked(t, refused.kept); got != 1 {
			t.Fatalf(`broker_member_kind_unchecked_total{kept=%q} after refusing %s = %d; want 1`, refused.kept, refused.person, got)
		}
	}
}
