package enroll

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// TestARestrictedMembersLauncherEnrollsNothing is Create's member-kind rule against a
// stand-in of Dispatch's person route. A machine login whose person Dispatch calls a restricted
// member enrolls nothing, ErrRestrictedMember, and writes no row, and so does a person's pod login
// (PersonPodLogin) whose person is one; a full member's login and pod login, and a
// login of a person Dispatch holds no row for, enroll as before (the control). While Dispatch
// cannot be asked, an operator's login enrolls nothing (*projectaccess.MemberKindUncheckable), and
// the same login enrolls once Dispatch answers again; a broker whose own account Dispatch refuses
// enrolls nothing for a person either. A person's sign-in on a machine is not held to this check,
// since the broker checked that person's domain itself when it minted the credential: while
// Dispatch is down it enrolls a session naming no project as checkProjects does, unchecked. A
// broker with no client of Dispatch asks nothing and enrolls as before, which is why the check
// needs BROKER_PROJECT_ACCESS_URL.
func TestARestrictedMembersLauncherEnrollsNothing(t *testing.T) {
	const (
		outsider = "outsider@outside.example"
		full     = "ines@example.com"
		unlisted = "nobody-yet@example.com"
		unasked  = "uma@example.com"
	)
	ctx := context.Background()
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{
		outsider: projectaccess.MemberRestricted,
		full:     projectaccess.MemberFull,
		unasked:  projectaccess.MemberFull,
	})
	svc := newService(t)
	svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
	enrol := func(t *testing.T, svc *Service, cred Credential, runtimeID string) error {
		t.Helper()
		_, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: runtimeID, Thumbprint: "tp-" + runtimeID})
		return err
	}
	enrolled := func(t *testing.T, svc *Service, cred Credential) int {
		t.Helper()
		var n int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments where launcher_credential_id=$1`, cred.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a full member's machine login enrolls", func(t *testing.T) {
		cred := mintCredential(t, svc, str(full), nil, "ines-devbox")
		if err := enrol(t, svc, cred, "ines-devbox-1"); err != nil || enrolled(t, svc, cred) != 1 {
			t.Fatalf("Create under %s's machine login = %v with %d enrollments, want one enrollment", full, err, enrolled(t, svc, cred))
		}
	})

	t.Run("a login of a person Dispatch holds no row for enrolls as before", func(t *testing.T) {
		cred := mintCredential(t, svc, str(unlisted), nil, "nobody-devbox")
		if err := enrol(t, svc, cred, "nobody-devbox-1"); err != nil || enrolled(t, svc, cred) != 1 {
			t.Fatalf("Create under %s's machine login = %v with %d enrollments, want one enrollment", unlisted, err, enrolled(t, svc, cred))
		}
	})

	t.Run("a restricted member's machine login enrolls nothing, naming a project or not", func(t *testing.T) {
		cred := mintCredential(t, svc, str(outsider), nil, "outsider-laptop")
		for _, projects := range [][]string{nil, {"PRIV"}} {
			_, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "outsider-laptop-1", Thumbprint: "tp-outsider-laptop-1", Projects: projects})
			if !errors.Is(err, ErrRestrictedMember) {
				t.Fatalf("Create under %s's machine login naming %v = %v, want ErrRestrictedMember", outsider, projects, err)
			}
		}
		if n := enrolled(t, svc, cred); n != 0 {
			t.Fatalf("the refused enrollments left %d rows under %s's machine login, want none", n, outsider)
		}
	})

	for _, schema := range personPodSchemas() {
		t.Run("on "+schema.name+" a restricted member's pod login enrolls nothing, naming a project or not, and a full member's enrolls", func(t *testing.T) {
			for _, row := range []struct {
				kind     string
				projects []string
				want     error
			}{
				{projectaccess.MemberFull, nil, nil},
				{projectaccess.MemberRestricted, nil, ErrRestrictedMember},
				{projectaccess.MemberRestricted, []string{"PRIV"}, ErrRestrictedMember},
			} {
				r := newPersonPodRig(t, schema)
				stand := projectaccesstest.NewServer(t, nil, map[string]string{adaPerson: row.kind})
				r.svc.Projects = projectaccess.New(stand.URL, stand.Account, nil)
				cred := personLogin(t, r, adaPodAccount, "pod-uid-ada", adaPerson)
				if !PersonPodLogin(cred) {
					t.Fatalf("%s's pod login = %+v, want a person's pod login", adaPerson, cred)
				}
				_, err := r.enrollNaming(t, cred, adaPodAccount, "pod-uid-ada", row.projects...)
				n := enrolled(t, r.svc, cred)
				switch {
				case row.want == nil && (err != nil || n != 1):
					t.Fatalf("Create(ada's pod) under the pod login of %s, a %s member, = %v with %d enrollments, want one enrollment", adaPerson, row.kind, err, n)
				case row.want != nil && (!errors.Is(err, row.want) || errors.Is(err, ErrProjectsRequired) || n != 0):
					t.Fatalf("Create(ada's pod) naming %v under the pod login of %s, a %s member, = %v with %d enrollments, want ErrRestrictedMember and none", row.projects, adaPerson, row.kind, err, n)
				}
			}
		})
	}

	t.Run("while Dispatch cannot be asked an operator's login enrolls nothing", func(t *testing.T) {
		dispatch.SetDown(true)
		t.Cleanup(func() { dispatch.SetDown(false) })
		cred := mintCredential(t, svc, str(unasked), nil, "uma-devbox")
		err := enrol(t, svc, cred, "uma-devbox-1")
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || uncheckable.Person != unasked || enrolled(t, svc, cred) != 0 ||
			err.Error() != "whether Dispatch calls "+unasked+" a restricted member cannot be read: the broker could not ask Dispatch" {
			t.Fatalf("Create under %s's machine login while Dispatch is down = %v with %d enrollments, want MemberKindUncheckable naming %s and none", unasked, err, enrolled(t, svc, cred), unasked)
		}

		signIn := personsSignIn(t, svc, unasked)
		if err := enrol(t, svc, signIn, "uma-shared-box-1"); err != nil || enrolled(t, svc, signIn) != 1 {
			t.Fatalf("Create under %s's sign-in while Dispatch is down = %v with %d enrollments, want one enrollment: checkMember holds no sign-in, and checkProjects enrolls an empty list unchecked", unasked, err, enrolled(t, svc, signIn))
		}

		// The control: the same login enrolls once Dispatch answers, so the refusal was the
		// outage's and nothing else's. A client keeps a person's failed read 2 s and then asks
		// again (projectaccess's own test proves that on a fake clock); a fresh client stands in
		// for that wait, as a broker reads Dispatch once the failure has aged out.
		dispatch.SetDown(false)
		svc.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
		if err := enrol(t, svc, cred, "uma-devbox-1"); err != nil || enrolled(t, svc, cred) != 1 {
			t.Fatalf("Create under %s's machine login once Dispatch answers = %v with %d enrollments, want one enrollment", unasked, err, enrolled(t, svc, cred))
		}
	})

	t.Run("a broker whose own account Dispatch refuses enrolls nothing for a person", func(t *testing.T) {
		refused := newService(t)
		refused.Projects = projectaccess.New(dispatch.URL, projectaccesstest.StaticAccount("not-the-broker"), nil)
		cred := mintCredential(t, refused, str(full), nil, "ines-laptop")
		err := enrol(t, refused, cred, "ines-laptop-1")
		var uncheckable *projectaccess.MemberKindUncheckable
		if !errors.As(err, &uncheckable) || !errors.Is(err, dispatchread.ErrBrokerRefused) || enrolled(t, refused, cred) != 0 ||
			!strings.HasSuffix(err.Error(), "Dispatch refused the broker's own account") {
			t.Fatalf("Create on a broker Dispatch refuses = %v with %d enrollments, want MemberKindUncheckable naming the refused account and none", err, enrolled(t, refused, cred))
		}
	})

	t.Run("a broker with no client of Dispatch asks nothing", func(t *testing.T) {
		unchecked := newService(t)
		cred := mintCredential(t, unchecked, str(outsider), nil, "outsider-desktop")
		if err := enrol(t, unchecked, cred, "outsider-desktop-1"); err != nil || enrolled(t, unchecked, cred) != 1 {
			t.Fatalf("Create on a broker with no project-access client = %v with %d enrollments, want one enrollment", err, enrolled(t, unchecked, cred))
		}
	})
}
