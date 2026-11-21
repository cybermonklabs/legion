package writerhold

// The t-11 tester's red test of finding 2, as the tester wrote it, with three adaptations for this
// public repository and CI: the two fixtures are read from testdata (the tester read them from
// T11_ACCEPTED_CM and T11_SCRIPT_REPORT, which CI does not set), and in both the private project
// names are example-root and example-infra and the accepted list's answers example.invalid links
// (testdata/minters-accepted-cm.json is the deployment repository's pull request 4235's rendered list at 9b2dfb0d23, as kind
// served it; testdata/minters-script-report-row-m.json is operator-token-minters.sh's row-m report
// at #4679's e171cc291a, as the script wrote it, for a project-level roles/container.admin
// holder).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const t11BrokerCI = "serviceAccount:broker-ci@example-root.iam.gserviceaccount.com"

func t11List(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "minters-accepted-cm.json"))
	require.NoError(t, err)
	var cm corev1.ConfigMap
	require.NoError(t, json.Unmarshal(raw, &cm))
	return &cm
}

// t11ScriptReport is the script's row-m report as it wrote it, every field kept, with its CI
// account renamed to broker-ci@ and its project to example-infra, and run_at set to now.
func t11ScriptReport(t *testing.T, now time.Time) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "minters-script-report-row-m.json"))
	require.NoError(t, err)
	text := strings.ReplaceAll(string(raw), "serviceAccount:ci-runner@example-project.iam.gserviceaccount.com", t11BrokerCI)
	text = strings.ReplaceAll(text, "example-project", "example-infra")
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &report))
	require.NotEmpty(t, report["findings"], "the script's report named no finding; this row would prove nothing")
	report["run_at"] = now.Add(-time.Minute).Format(time.RFC3339)
	report["unknown"] = []any{}
	return report
}

func TestT11TheRenderedListReadsWhole(t *testing.T) {
	accepted, err := readAcceptedMinters(context.Background(), fake.NewSimpleClientset(t11List(t)))
	require.NoError(t, err)
	require.Len(t, accepted, 17)
}

// The requirement: broker-ci@'s roles/container.admin is on the rendered list, so the report the
// script writes for exactly that holding is accepted.
func TestT11AnAcceptedHoldingAsTheScriptReportsItIsAccepted(t *testing.T) {
	now := time.Now().UTC()
	err := PreflightMinters(context.Background(), fake.NewSimpleClientset(t11List(t)),
		minterReportFile(t, t11ScriptReport(t, now)), func() time.Time { return now })
	require.NoError(t, err, "broker-ci@ holding roles/container.admin is accepted by the deployment repository's pull request 4235's list")
}

// Control: the same findings in the reader's own shape (no right, granted_at a time) are accepted,
// so the list and the role match work, and a principal the list does not hold is refused.
func TestT11ControlTheReadersShapeIsAccepted(t *testing.T) {
	now := time.Now().UTC()
	report := t11ScriptReport(t, now)
	var findings []map[string]any
	for _, f := range report["findings"].([]any) {
		f := f.(map[string]any)
		delete(f, "right")
		f["granted_at"] = now.Add(-time.Hour).Format(time.RFC3339)
		findings = append(findings, f)
	}
	ok := PreflightMinters(context.Background(), fake.NewSimpleClientset(t11List(t)),
		minterReportFile(t, reportAt(now.Add(-time.Minute), findings, nil)), func() time.Time { return now })
	require.NoError(t, ok)
	findings[0]["principal"] = "serviceAccount:stranger@example-infra.iam.gserviceaccount.com"
	refused := PreflightMinters(context.Background(), fake.NewSimpleClientset(t11List(t)),
		minterReportFile(t, reportAt(now.Add(-time.Minute), findings, nil)), func() time.Time { return now })
	require.ErrorContains(t, refused, "stranger@")
}

// The second difference: the script's report with its right field dropped and its granted_at kept
// as the script writes it.
func TestT11TheScriptsGrantedAtAlone(t *testing.T) {
	now := time.Now().UTC()
	report := t11ScriptReport(t, now)
	for _, f := range report["findings"].([]any) {
		delete(f.(map[string]any), "right")
	}
	err := PreflightMinters(context.Background(), fake.NewSimpleClientset(t11List(t)),
		minterReportFile(t, report), func() time.Time { return now })
	t.Logf("result: %v", err)
	require.NoError(t, err)
}

// Kept beside the tester's rows: a field the script does not write is still refused. The
// script's report with one extra field in a finding is UNKNOWN, naming the field, so the reading
// stays strict while it follows the script.
func TestT11AFieldTheScriptDoesNotWriteIsRefused(t *testing.T) {
	now := time.Now().UTC()
	report := t11ScriptReport(t, now)
	report["findings"].([]any)[0].(map[string]any)["scope"] = "the whole project"
	requireUnknown(t, PreflightMinters(context.Background(), fake.NewSimpleClientset(t11List(t)),
		minterReportFile(t, report), func() time.Time { return now }), StepPreflight, `unknown field "scope"`)
}
