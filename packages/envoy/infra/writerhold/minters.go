package writerhold

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The accepted list (§3a, "Who can mint the operator's token"): the ConfigMap
// envoy-operator-accepted-minters in namespace envoy-operator, which the deployment repository's cluster-grants/
// renders (t-11, part 3, the deployment repository's pull request 4235), under one data key, AcceptedMintersKey, whose value is a
// JSON array of AcceptedMinter, [] until Sami answers plan F's items 8(d) and 8(f).
const (
	AcceptedMintersNamespace = "envoy-operator"
	AcceptedMintersConfigMap = "envoy-operator-accepted-minters"
	AcceptedMintersKey       = "accepted.json"
)

// minterReportAge bounds the report's age: it says who could mint the operator's token or write the
// hold's tables when it ran, so the preflight takes one run less than an hour before (§3a).
const minterReportAge = time.Hour

// MinterFinding is one holder the minter script reports: a principal and the right it holds, a
// right that mints the operator's token or writes the hold's tables.
type MinterFinding struct {
	Principal string `json:"principal"`
	Right     string `json:"right"`
}

// MinterUnknown is one step the minter script could not read, and why.
type MinterUnknown struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// minterReport is the report operator-token-minters.sh --report <file> writes (plan E line 2148):
// {"run_at": "<RFC 3339>", "findings": [{"principal", "right"}], "unknown": [{"step", "reason"}]}.
// Both lists are pointers so that a report missing one is told from a report whose list is empty.
type minterReport struct {
	RunAt    *time.Time       `json:"run_at"`
	Findings *[]MinterFinding `json:"findings"`
	Unknown  *[]MinterUnknown `json:"unknown"`
}

// AcceptedMinter is one entry of the accepted list, as t-11's cluster-grants/ renders it
// (the deployment repository's pull request 4235): a principal, the right it holds, the clause of plan F's go-live items it falls
// under (8(d) for a minter, 8(f) for a hold writer), and the link to Sami's answer that accepts it.
// Every field is required.
type AcceptedMinter struct {
	Principal string `json:"principal"`
	Right     string `json:"right"`
	Clause    string `json:"clause"`
	Answer    string `json:"answer"`
}

// minterPreflight is the preflight's minter check; skipReport is the row's control, a preflight
// that reads no report.
type minterPreflight struct {
	skipReport bool
}

// PreflightMinters is the preflight's minter check (§3a, "How the preflight reads it"): it reads
// reportPath, the --minters-report of `hold prepare` or `hold set`, and the accepted list through
// client (t-11's get on that one ConfigMap). It stops the hold unless the report ran under one hour
// before now, its unknown list is empty, and every finding, the hold writers included, is an
// accepted entry of the same principal and right. No report is a refusal naming it; a report or a
// list that does not read whole is UNKNOWN, and so is a report whose unknown list names a step.
func PreflightMinters(ctx context.Context, client kubernetes.Interface, reportPath string, now func() time.Time) error {
	return minterPreflight{}.run(ctx, client, reportPath, now)
}

func (p minterPreflight) run(ctx context.Context, client kubernetes.Interface, reportPath string, now func() time.Time) error {
	if p.skipReport {
		return nil
	}
	if reportPath == "" {
		return refusef(StepPreflight, "no minter report: the hold takes --minters-report <file>, the report operator-token-minters.sh --report writes")
	}
	report, err := readMinterReport(reportPath)
	if err != nil {
		return err
	}
	at := now()
	if age := at.Sub(*report.RunAt); age < 0 || age >= minterReportAge {
		return refusef(StepPreflight, "the minter report %s ran at %s, %s before now (%s); the hold takes a report under %s old", reportPath, report.RunAt.Format(time.RFC3339), age.Round(time.Second), at.UTC().Format(time.RFC3339), minterReportAge)
	}
	if unknown := *report.Unknown; len(unknown) > 0 {
		steps := make([]string, len(unknown))
		for i, u := range unknown {
			steps[i] = fmt.Sprintf("%s (%s)", u.Step, u.Reason)
		}
		return unknownf(StepPreflight, nil, "the minter report %s could not read: %s", reportPath, strings.Join(steps, "; "))
	}
	accepted, err := readAcceptedMinters(ctx, client)
	if err != nil {
		return err
	}
	ok := map[MinterFinding]bool{}
	for _, a := range accepted {
		ok[MinterFinding{Principal: a.Principal, Right: a.Right}] = true
	}
	var refused []string
	for _, finding := range *report.Findings {
		if !ok[finding] {
			refused = append(refused, finding.Principal+" ("+finding.Right+")")
		}
	}
	if len(refused) > 0 {
		return refusef(StepPreflight, "the minter report %s names holders that %s/%s does not accept: %s", reportPath, AcceptedMintersNamespace, AcceptedMintersConfigMap, strings.Join(refused, ", "))
	}
	return nil
}

// readMinterReport reads the report at path, strictly: one JSON object with run_at, findings and
// unknown, no other field, each finding a principal and a right. Anything else is UNKNOWN, naming
// the file.
func readMinterReport(path string) (minterReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return minterReport{}, unknownf(StepPreflight, err, "read the minter report %s", path)
	}
	var report minterReport
	if err := decodeStrict(data, &report); err != nil {
		return minterReport{}, unknownf(StepPreflight, err, "decode the minter report %s", path)
	}
	switch {
	case report.RunAt == nil:
		return minterReport{}, unknownf(StepPreflight, nil, "the minter report %s has no run_at", path)
	case report.Findings == nil || report.Unknown == nil:
		return minterReport{}, unknownf(StepPreflight, nil, "the minter report %s lacks its findings or its unknown list", path)
	}
	for i, f := range *report.Findings {
		if f.Principal == "" || f.Right == "" {
			return minterReport{}, unknownf(StepPreflight, nil, "the minter report %s's findings[%d] names no principal or no right", path, i)
		}
	}
	return report, nil
}

// readAcceptedMinters reads the accepted list: the ConfigMap's AcceptedMintersKey, a JSON array of
// entries each with a principal, a right, a clause and an answer. A ConfigMap that cannot be read, a
// missing key, or an entry that leaves out or empties a field is UNKNOWN, naming the entry's place
// in the list and each field it lacks (entryLacks).
func readAcceptedMinters(ctx context.Context, client kubernetes.Interface) ([]AcceptedMinter, error) {
	where := AcceptedMintersNamespace + "/" + AcceptedMintersConfigMap
	cm, err := client.CoreV1().ConfigMaps(AcceptedMintersNamespace).Get(ctx, AcceptedMintersConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, unknownf(StepPreflight, err, "read ConfigMap %s", where)
	}
	text, ok := cm.Data[AcceptedMintersKey]
	if !ok {
		return nil, unknownf(StepPreflight, nil, "ConfigMap %s holds no %s", where, AcceptedMintersKey)
	}
	var accepted []AcceptedMinter
	if err := decodeStrict([]byte(text), &accepted); err != nil {
		return nil, unknownf(StepPreflight, err, "decode ConfigMap %s's %s", where, AcceptedMintersKey)
	}
	if accepted == nil {
		return nil, unknownf(StepPreflight, nil, "ConfigMap %s's %s is not a list", where, AcceptedMintersKey)
	}
	for i, a := range accepted {
		if lacks := entryLacks(a); lacks != "" {
			return nil, unknownf(StepPreflight, nil, "ConfigMap %s's %s entry %d lacks its %s", where, AcceptedMintersKey, i, lacks)
		}
	}
	return accepted, nil
}

// entryLacks names the fields of a that are empty, in the entry's order ("clause", "principal,
// right, clause and answer"), or "" for a whole entry. A field left out of the JSON decodes empty,
// so it is named the same way.
func entryLacks(a AcceptedMinter) string {
	var lacks []string
	for _, f := range []struct{ name, value string }{
		{"principal", a.Principal}, {"right", a.Right}, {"clause", a.Clause}, {"answer", a.Answer},
	} {
		if f.value == "" {
			lacks = append(lacks, f.name)
		}
	}
	if len(lacks) < 2 {
		return strings.Join(lacks, "")
	}
	return strings.Join(lacks[:len(lacks)-1], ", ") + " and " + lacks[len(lacks)-1]
}

// decodeStrict decodes data, one JSON value, into into, refusing an unknown field and anything
// after the value.
func decodeStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON value")
	}
	return nil
}
