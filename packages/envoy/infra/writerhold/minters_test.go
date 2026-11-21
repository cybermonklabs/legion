package writerhold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// minterNow is the preflight's clock in the minter rows, and minterGrantedAt when their grants were
// made.
var (
	minterNow       = time.Date(2026, 11, 21, 1, 30, 0, 0, time.UTC)
	minterGrantedAt = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
)

// The principals of the minter rows. legionCI and applier hold project-level roles/container.admin
// today (plan v62 t-11 part 4), on the projects rootProject and infraProject. None is a person: the
// minter script's report names no person.
const (
	legionCI     = "serviceAccount:legion-ci@example-root.iam.gserviceaccount.com"
	applier      = "serviceAccount:infra-applier@example-infra.iam.gserviceaccount.com"
	adminRole    = "roles/container.admin"
	rootProject  = "projects/example-root"
	infraProject = "projects/example-infra"
	createToken  = "container.serviceAccounts.createToken"
)

// acceptedMinters is ConfigMap envoy-operator-accepted-minters holding entries, in the form
// PreflightMinters reads (AcceptedMintersKey, a JSON array of principal, right, clause and answer):
// a []AcceptedMinter, or a []map[string]string for an entry that leaves a field out.
func acceptedMinters(t *testing.T, entries any) *corev1.ConfigMap {
	t.Helper()
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AcceptedMintersConfigMap, Namespace: AcceptedMintersNamespace},
		Data:       map[string]string{AcceptedMintersKey: string(data)},
	}
}

// minterReportFile writes report, the JSON operator-token-minters.sh --report writes, and returns
// its path.
func minterReportFile(t *testing.T, report any) string {
	t.Helper()
	data, err := json.Marshal(report)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "minters.json")
	require.NoError(t, os.WriteFile(file, data, 0o600))
	return file
}

// finding is one finding as the minter script's --report writes it (#4679): its principal, role,
// resource, permissions and granted_at, as JSON fields, so a case can leave one out or empty it.
func finding(principal, role, resource string, permissions ...string) map[string]any {
	return map[string]any{
		"principal": principal, "role": role, "resource": resource,
		"permissions": permissions, "granted_at": minterGrantedAt.Format(time.RFC3339),
	}
}

// reportAt is a report run at runAt naming findings, with unknown steps.
func reportAt(runAt time.Time, findings []map[string]any, unknown []MinterUnknown) map[string]any {
	if findings == nil {
		findings = []map[string]any{}
	}
	if unknown == nil {
		unknown = []MinterUnknown{}
	}
	return map[string]any{"run_at": runAt.Format(time.RFC3339), "findings": findings, "unknown": unknown}
}

// TestHoldPreflightTheMinterReport shows the preflight's minter check (§3a "Who can mint the
// operator's token", plan v62 lines 427-431 and 879), on the report's structured findings
// (principal, role, resource, permissions, granted_at; #4679). A report whose every finding matches
// an accepted entry, principal for principal and role for right, run 10 minutes before, passes.
// Each of these stops the preflight, naming its cause: a principal not in
// envoy-operator-accepted-minters (named with its role and resource), a hold writer not in it (one
// stubbed holder of each of plan E's four rights), the same principal with another role, an UNKNOWN
// step, a report an hour old or more, one dated after now, a report that does not decode whole, a
// report in the old shape (principal and right), a finding that leaves out or empties any of its
// five fields (UNKNOWN, naming the finding's index and the fields), an accepted list that cannot be
// read, an accepted entry that leaves out or empties any of its principal, right, clause and answer
// (the deployment repository's pull request 4235's form), and no report. No principal of the rows is a person, because the
// report names none. Control: a preflight that skips the report passes a report naming legion-ci@.
// Production change that would fail it: a check that compares principals alone, takes the old
// shape, takes a finding without its resource, permissions or grant time, ignores unknown steps,
// reads an age from the file's time rather than run_at, takes an entry without its clause or its
// answer, or passes when no report is given.
func TestHoldPreflightTheMinterReport(t *testing.T) {
	ctx := t.Context()
	now := func() time.Time { return minterNow }
	accepted := []AcceptedMinter{{Principal: applier, Right: adminRole, Clause: "8(d)", Answer: "https://example.invalid/answers/1"}}
	client := func(objects ...runtime.Object) *fake.Clientset { return fake.NewClientset(objects...) }
	list := acceptedMinters(t, accepted)
	fresh := minterNow.Add(-10 * time.Minute)

	passing := minterReportFile(t, reportAt(fresh, []map[string]any{finding(applier, adminRole, infraProject, createToken)}, nil))
	t.Run("a match passes", func(t *testing.T) {
		require.NoError(t, PreflightMinters(ctx, client(list), passing, now), "a report whose one finding is accepted, 10 minutes old")
	})

	withCI := minterReportFile(t, reportAt(fresh, []map[string]any{
		finding(applier, adminRole, infraProject, createToken), finding(legionCI, adminRole, rootProject, createToken),
	}, nil))
	t.Run("a principal not in the accepted list", func(t *testing.T) {
		requireRefusal(t, PreflightMinters(ctx, client(list), withCI, now), StepPreflight,
			"names holders that envoy-operator/envoy-operator-accepted-minters does not accept: "+legionCI+" (role "+adminRole+" on "+rootProject+")")
	})
	t.Run("the control: a preflight that skips the report", func(t *testing.T) {
		require.NoError(t, minterPreflight{skipReport: true}.run(ctx, client(list), withCI, now), "the danger: legion-ci@ passes")
	})

	for _, writer := range []struct{ principal, role, resource, permission string }{
		{"serviceAccount:dba@example-infra.iam.gserviceaccount.com", "roles/cloudsql.admin", infraProject + "/instances/example-sql", "cloudsql.users.update"},
		{"serviceAccount:reader@example-infra.iam.gserviceaccount.com", "Role dispatch/secret-reader", "namespaces/dispatch/secrets/envoy-broker-db", "get"},
		{"serviceAccount:state@example-infra.iam.gserviceaccount.com", "roles/storage.objectViewer", "buckets/example-state/objects/broker", "storage.objects.get"},
		{"serviceAccount:box@example-infra.iam.gserviceaccount.com", "roles/secretmanager.secretAccessor", infraProject + "/secrets/operator-only-envoy-hold-signing", "secretmanager.versions.access"},
	} {
		t.Run("a hold writer: "+writer.permission, func(t *testing.T) {
			report := minterReportFile(t, reportAt(fresh, []map[string]any{finding(writer.principal, writer.role, writer.resource, writer.permission)}, nil))
			requireRefusal(t, PreflightMinters(ctx, client(list), report, now), StepPreflight, writer.principal+" (role "+writer.role+" on "+writer.resource+")")
		})
	}
	t.Run("the same principal with another role", func(t *testing.T) {
		report := minterReportFile(t, reportAt(fresh, []map[string]any{finding(applier, "roles/container.developer", infraProject, createToken)}, nil))
		requireRefusal(t, PreflightMinters(ctx, client(list), report, now), StepPreflight, applier+" (role roles/container.developer on "+infraProject+")")
	})
	t.Run("a report in the old shape", func(t *testing.T) {
		// The accepted list accepts the old finding's principal and right, so only the shape stops it.
		old := minterReportFile(t, reportAt(fresh, []map[string]any{{"principal": applier, "right": adminRole}}, nil))
		unknown := requireUnknown(t, PreflightMinters(ctx, client(list), old, now), StepPreflight, "decode the minter report")
		require.ErrorContains(t, unknown.Err, `unknown field "right"`)
	})
	t.Run("an UNKNOWN step", func(t *testing.T) {
		report := minterReportFile(t, reportAt(fresh, nil, []MinterUnknown{{Step: "project IAM policy", Reason: "legion-reader lacks resourcemanager.projects.getIamPolicy"}}))
		requireUnknown(t, PreflightMinters(ctx, client(list), report, now), StepPreflight, "could not read: project IAM policy (legion-reader lacks resourcemanager.projects.getIamPolicy)")
	})
	for _, c := range []struct {
		name  string
		runAt time.Time
		age   string
	}{
		{"a report an hour old", minterNow.Add(-time.Hour), "1h0m0s before now"},
		{"a report over an hour old", minterNow.Add(-61 * time.Minute), "1h1m0s before now"},
		{"a report dated after now", minterNow.Add(time.Minute), "-1m0s before now"},
	} {
		t.Run(c.name, func(t *testing.T) {
			report := minterReportFile(t, reportAt(c.runAt, nil, nil))
			requireRefusal(t, PreflightMinters(ctx, client(list), report, now), StepPreflight, c.age, "the hold takes a report under 1h0m0s old")
		})
	}
	t.Run("no report", func(t *testing.T) {
		requireRefusal(t, PreflightMinters(ctx, client(list), "", now), StepPreflight, "no minter report")
	})
	t.Run("a report that cannot be read", func(t *testing.T) {
		requireUnknown(t, PreflightMinters(ctx, client(list), filepath.Join(t.TempDir(), "absent.json"), now), StepPreflight, "read the minter report")
	})
	for _, c := range []struct {
		name   string
		report any
		what   string
	}{
		{"a report with no unknown list", map[string]any{"run_at": fresh.Format(time.RFC3339), "findings": []any{}}, "lacks its findings or its unknown list"},
		{"a report with no run_at", map[string]any{"findings": []any{}, "unknown": []MinterUnknown{}}, "has no run_at"},
		{"a report with a field the hold does not read", map[string]any{"run_at": fresh.Format(time.RFC3339), "findings": []any{}, "unknown": []MinterUnknown{}, "skipped": []string{"iam"}}, "decode the minter report"},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireUnknown(t, PreflightMinters(ctx, client(list), minterReportFile(t, c.report), now), StepPreflight, c.what)
		})
	}
	// requireLacks requires err to be UNKNOWN whose finding ends with what, naming the finding's
	// index and exactly the fields it lacks, so a message that names every field passes none.
	requireLacks := func(t *testing.T, err error, what string) {
		t.Helper()
		unknown := requireUnknown(t, err, StepPreflight, "the minter report")
		require.True(t, strings.HasSuffix(unknown.What, what), "the finding ends %q: %q", what, unknown.What)
	}
	for _, field := range []string{"principal", "role", "resource", "permissions", "granted_at"} {
		// The report's first finding is whole and accepted, so the second must be named by its index.
		empty := map[string]any{"principal": "", "role": "", "resource": "", "permissions": []string{}, "granted_at": ""}[field]
		t.Run("a finding with no "+field, func(t *testing.T) {
			other := finding(legionCI, adminRole, rootProject, createToken)
			delete(other, field)
			report := minterReportFile(t, reportAt(fresh, []map[string]any{finding(applier, adminRole, infraProject, createToken), other}, nil))
			requireLacks(t, PreflightMinters(ctx, client(list), report, now), "findings[1] lacks its "+field)
		})
		t.Run("a finding with an empty "+field, func(t *testing.T) {
			other := finding(legionCI, adminRole, rootProject, createToken)
			other[field] = empty
			report := minterReportFile(t, reportAt(fresh, []map[string]any{finding(applier, adminRole, infraProject, createToken), other}, nil))
			requireLacks(t, PreflightMinters(ctx, client(list), report, now), "findings[1] lacks its "+field)
		})
	}
	t.Run("a finding with no field at all", func(t *testing.T) {
		report := minterReportFile(t, reportAt(fresh, []map[string]any{{}}, nil))
		requireLacks(t, PreflightMinters(ctx, client(list), report, now), "findings[0] lacks its principal, role, resource, permissions and granted_at")
	})
	t.Run("a finding whose permissions hold an empty entry", func(t *testing.T) {
		report := minterReportFile(t, reportAt(fresh, []map[string]any{finding(legionCI, adminRole, rootProject, createToken, "")}, nil))
		requireLacks(t, PreflightMinters(ctx, client(list), report, now), "findings[0] lacks its permissions")
	})
	t.Run("a finding whose granted_at is not RFC 3339", func(t *testing.T) {
		bad := finding(legionCI, adminRole, rootProject, createToken)
		bad["granted_at"] = "last month"
		report := minterReportFile(t, reportAt(fresh, []map[string]any{bad}, nil))
		requireLacks(t, PreflightMinters(ctx, client(list), report, now), `findings[0]'s granted_at "last month" is not RFC 3339`)
	})
	t.Run("no accepted list", func(t *testing.T) {
		requireUnknown(t, PreflightMinters(ctx, client(), passing, now), StepPreflight, "read ConfigMap envoy-operator/envoy-operator-accepted-minters")
	})
	// requireEntryFinding requires err to be UNKNOWN whose finding ends naming entry i and exactly
	// the fields it lacks, so a message that names every field at once does not pass.
	requireEntryFinding := func(t *testing.T, err error, i int, lacks string) {
		t.Helper()
		unknown := requireUnknown(t, err, StepPreflight, AcceptedMintersKey)
		require.True(t, strings.HasSuffix(unknown.What, fmt.Sprintf("%s entry %d lacks its %s", AcceptedMintersKey, i, lacks)),
			"the finding names entry %d and lacks its %s alone: %q", i, lacks, unknown.What)
	}
	for _, field := range []string{"principal", "right", "clause", "answer"} {
		// The list's first entry is whole, so the finding must name the second by its place.
		whole := map[string]string{"principal": applier, "right": adminRole, "clause": "8(d)", "answer": accepted[0].Answer}
		other := map[string]string{"principal": legionCI, "right": adminRole, "clause": "8(d)", "answer": accepted[0].Answer}
		t.Run("an accepted entry with no "+field, func(t *testing.T) {
			delete(other, field)
			list := acceptedMinters(t, []map[string]string{whole, other})
			requireEntryFinding(t, PreflightMinters(ctx, client(list), passing, now), 1, field)
		})
		t.Run("an accepted entry with an empty "+field, func(t *testing.T) {
			other[field] = ""
			list := acceptedMinters(t, []map[string]string{whole, other})
			requireEntryFinding(t, PreflightMinters(ctx, client(list), passing, now), 1, field)
		})
	}
	t.Run("an accepted entry with no field at all", func(t *testing.T) {
		list := acceptedMinters(t, []map[string]string{{}})
		requireEntryFinding(t, PreflightMinters(ctx, client(list), passing, now), 0, "principal, right, clause and answer")
	})
	t.Run("the empty list Sami has not yet answered", func(t *testing.T) {
		empty := acceptedMinters(t, []AcceptedMinter{})
		requireRefusal(t, PreflightMinters(ctx, client(empty), passing, now), StepPreflight, applier)
		none := minterReportFile(t, reportAt(fresh, nil, nil))
		require.NoError(t, PreflightMinters(ctx, client(empty), none, now), "a report with no finding")
	})
}
