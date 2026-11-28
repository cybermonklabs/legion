package specgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// Digest is the lower-case hex SHA-256 of the statement's record: its type, then each field in a
// fixed order (id, kind, parent, serves, about), then its text. The position is not in the record,
// so a move to another file keeps the digest.
func (s Statement) Digest() string {
	return digest("requirement", s.ID, string(s.Kind), s.Parent, recordList(s.Serves), recordList(s.About), s.Text)
}

// Digest is the lower-case hex SHA-256 of the check's record: its type, then each field in a fixed
// order (id, serves, subject, procedure, runner, selector, job, time limit), then its claim,
// procedure and failing example. The position is not in the record.
// It is the procedure digest a result of the check carries.
func (c Check) Digest() string {
	var test Test
	if c.Test != nil {
		test = *c.Test
	}
	limit := ""
	if c.TimeLimit != 0 {
		limit = c.TimeLimit.String()
	}
	return digest("check", c.ID, c.Serves, string(c.Subject), string(c.Procedure), test.Runner, test.Selector, test.Job, limit,
		c.Claim, c.ProcedureText, c.FailingExample)
}

// Digest is the lower-case hex SHA-256 of the component's record: its type, then each field in a
// fixed order (id, name, kind, parent, joins), then its purpose. The position is not in the record.
func (c Component) Digest() string {
	kind := ""
	if c.Interface {
		kind = "interface"
	}
	return digest("component", c.ID, c.Name, kind, c.Parent, recordList(c.Joins), c.Purpose)
}

// Digest is the lower-case hex SHA-256 of the term's record: its type, its id and name, then
// its definition. The position is not in the record.
func (t Term) Digest() string {
	return digest("term", t.ID, t.Name, t.Definition)
}

// recordList is a list as the record writes it: its ids sorted, joined with one space.
func recordList(list []string) string {
	return strings.Join(slices.Sorted(slices.Values(list)), " ")
}

// digest is the SHA-256 of fields, each written as "<byte length>:<bytes>", so no two lists of
// fields write the same record.
func digest(fields ...string) string {
	h := sha256.New()
	for _, field := range fields {
		h.Write(strconv.AppendInt(nil, int64(len(field)), 10))
		h.Write([]byte{':'})
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}
