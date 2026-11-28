package legionagents_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/legionagents"
)

// A record the bucket holds that does not decode, bytes that are not JSON or JSON that is not an
// entry or an answering record, is ErrMalformedRecord on each read that meets it: a session's key,
// a key a listing reads (the role token's listing a session read makes included), and a project's
// answering record. So is each write of that session, which reads its stored entry before it
// writes, a batch's as a *PartialWriteError that wrote none. The bucket answered each of them, so
// none is a read that failed, and each error names the session, the key or the project. The
// records are left as they were.
func TestEachRecordThatDoesNotDecodeIsAMalformedRecord(t *testing.T) {
	for _, record := range []struct{ name, value string }{
		{"bytes that are not JSON", "not a registry record"},
		{"JSON that is not a record", `{"session_id":7,"answering":"yes"}`},
	} {
		t.Run(record.name, func(t *testing.T) {
			r := openRegistry(t)
			c := ctx(t)
			if _, err := r.store.Put(c, agent("infra", "INFRA-448", "tester", "ses-tester", "2026-10-26T03:15:00Z")); err != nil {
				t.Fatalf("put: %v", err)
			}
			malformed := []string{"session.ses-bad", "issue.INFRA-449.ses-bad", "role.legion-infra-infra-448-tester.ses-bad", "project.infra"}
			for _, key := range malformed {
				if _, err := r.raw.Put(key, []byte(record.value)); err != nil {
					t.Fatalf("put %s: %v", key, err)
				}
			}
			bad := agent("infra", "INFRA-448", "tester", "ses-bad", "2026-10-26T04:00:00Z")
			for _, tc := range []struct {
				name  string
				call  func() error
				names string
			}{
				{"Get of a session whose key does not decode", func() error { _, err := r.store.Get(c, "ses-bad"); return err }, "legion agent ses-bad"},
				{"Get of a session whose role token lists one", func() error { _, err := r.store.Get(c, "ses-tester"); return err }, "key role.legion-infra-infra-448-tester.ses-bad"},
				{"ByIssue", func() error { _, err := r.store.ByIssue(c, "INFRA-449"); return err }, "key issue.INFRA-449.ses-bad"},
				{"ByRoleToken", func() error { _, err := r.store.ByRoleToken(c, "legion-infra-infra-448-tester"); return err }, "key role.legion-infra-infra-448-tester.ses-bad"},
				{"Project", func() error { _, _, err := r.store.Project(c, "infra"); return err }, "legion project infra"},
				{"Put", func() error { _, err := r.store.Put(c, bad); return err }, "legion agent ses-bad"},
			} {
				if err := tc.call(); !errors.Is(err, legionagents.ErrMalformedRecord) || !strings.Contains(err.Error(), tc.names) {
					t.Errorf("%s = %v, want ErrMalformedRecord naming %q", tc.name, err, tc.names)
				}
			}
			_, err := r.store.PutAll(c, []legionagents.Entry{bad})
			var partial *legionagents.PartialWriteError
			if !errors.As(err, &partial) || partial.Written != 0 || !errors.Is(err, legionagents.ErrMalformedRecord) || !strings.Contains(err.Error(), "legion agent ses-bad") {
				t.Errorf("PutAll = %v, want a *PartialWriteError that wrote none, wrapping ErrMalformedRecord naming legion agent ses-bad", err)
			}
			held := r.contents(t)
			for _, key := range malformed {
				if held[key] != record.value {
					t.Errorf("%s holds %q after the calls, want %q, untouched", key, held[key], record.value)
				}
			}
		})
	}
}
