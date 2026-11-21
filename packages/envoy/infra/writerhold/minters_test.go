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

// minterNow is the preflight's clock in the minter rows.
var minterNow = time.Date(2026, 11, 21, 1, 30, 0, 0, time.UTC)

// The principals of the minter rows. legionCI and applier hold project-level roles/container.admin
// today (plan v62 t-11 part 4); the four hold writers are stubs of plan E line 2148's four rights.
const (
	legionCI  = "serviceAccount:legion-ci@example-root.iam.gserviceaccount.com"
	applier   = "serviceAccount:infra-applier@example-infra.iam.gserviceaccount.com"
	adminRole = "roles/container.admin"
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

// reportAt is a report run at runAt naming findings, with unknown steps.
func reportAt(runAt time.Time, findings []MinterFinding, unknown []MinterUnknown) map[string]any {
	if findings == nil {
		findings = []MinterFinding{}
	}
	if unknown == nil {
		unknown = []MinterUnknown{}
	}
	return map[string]any{"run_at": runAt.Format(time.RFC3339), "findings": findings, "unknown": unknown}
}

// TestHoldPreflightTheMinterReport shows the preflight's minter check (§3a "Who can mint the
// operator's token", plan v62 lines 427-431 and 879): a report that names only accepted principals,
// each for the right it was accepted for, run 10 minutes before, passes. Each of these stops the
// preflight, naming its cause: a principal not in envoy-operator-accepted-minters, a hold writer
// not in it (one stubbed holder of each of plan E's four rights), an accepted principal holding a
// right it was not accepted for, an UNKNOWN step, a report an hour old or more, one dated after
// now, a report that does not decode whole, an accepted list that cannot be read, an accepted entry
// that leaves out or empties any of its principal, right, clause and answer (UNKNOWN, naming the
// entry's place and the field: the deployment repository's pull request 4235's form), and no report. Control: a preflight that
// skips the report passes a report naming legion-ci@. Production change that would fail it: a check
// that compares principals alone, ignores unknown steps, reads an age from the file's time rather
// than run_at, takes an entry without its clause or its answer, or passes when no report is given.
func TestHoldPreflightTheMinterReport(t *testing.T) {
	ctx := t.Context()
	now := func() time.Time { return minterNow }
	accepted := []AcceptedMinter{{Principal: applier, Right: adminRole, Clause: "8(d)", Answer: "https://example.invalid/answers/1"}}
	client := func(objects ...runtime.Object) *fake.Clientset { return fake.NewClientset(objects...) }
	list := acceptedMinters(t, accepted)
	fresh := minterNow.Add(-10 * time.Minute)

	passing := minterReportFile(t, reportAt(fresh, []MinterFinding{{Principal: applier, Right: adminRole}}, nil))
	require.NoError(t, PreflightMinters(ctx, client(list), passing, now), "a report naming only accepted principals, 10 minutes old")

	withCI := minterReportFile(t, reportAt(fresh, []MinterFinding{{Principal: applier, Right: adminRole}, {Principal: legionCI, Right: adminRole}}, nil))
	t.Run("a principal not in the accepted list", func(t *testing.T) {
		requireRefusal(t, PreflightMinters(ctx, client(list), withCI, now), StepPreflight,
			"names holders that envoy-operator/envoy-operator-accepted-minters does not accept: "+legionCI+" ("+adminRole+")")
	})
	t.Run("the control: a preflight that skips the report", func(t *testing.T) {
		require.NoError(t, minterPreflight{skipReport: true}.run(ctx, client(list), withCI, now), "the danger: legion-ci@ passes")
	})

	for _, writer := range []MinterFinding{
		{Principal: "user:dba@example.com", Right: "cloudsql.users.update on instance example-sql"},
		{Principal: "serviceAccount:reader@example-infra.iam.gserviceaccount.com", Right: "get on Secret dispatch/envoy-broker-db"},
		{Principal: "group:infra@example.com", Right: "read of the broker/ stack's Pulumi state"},
		{Principal: "serviceAccount:box@example-infra.iam.gserviceaccount.com", Right: "secretmanager.versions.access on operator-only-envoy-hold-signing"},
	} {
		t.Run("a hold writer: "+writer.Right, func(t *testing.T) {
			report := minterReportFile(t, reportAt(fresh, []MinterFinding{writer}, nil))
			requireRefusal(t, PreflightMinters(ctx, client(list), report, now), StepPreflight, writer.Principal+" ("+writer.Right+")")
		})
	}
	t.Run("an accepted principal with another right", func(t *testing.T) {
		report := minterReportFile(t, reportAt(fresh, []MinterFinding{{Principal: applier, Right: "container.serviceAccounts.createToken"}}, nil))
		requireRefusal(t, PreflightMinters(ctx, client(list), report, now), StepPreflight, applier+" (container.serviceAccounts.createToken)")
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
		{"a report with no unknown list", map[string]any{"run_at": fresh.Format(time.RFC3339), "findings": []MinterFinding{}}, "lacks its findings or its unknown list"},
		{"a report with no run_at", map[string]any{"findings": []MinterFinding{}, "unknown": []MinterUnknown{}}, "has no run_at"},
		{"a report with a field the hold does not read", map[string]any{"run_at": fresh.Format(time.RFC3339), "findings": []MinterFinding{}, "unknown": []MinterUnknown{}, "skipped": []string{"iam"}}, "decode the minter report"},
		{"a finding with no right", reportAt(fresh, []MinterFinding{{Principal: legionCI}}, nil), "findings[0] names no principal or no right"},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireUnknown(t, PreflightMinters(ctx, client(list), minterReportFile(t, c.report), now), StepPreflight, c.what)
		})
	}
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
