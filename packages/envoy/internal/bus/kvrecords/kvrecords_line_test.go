package kvrecords

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A records line is the bytes the format's first encoder wrote, which held each field in its own
// json.Marshal and joined them by hand: the two lines below are that encoder's, printed at the
// commit that held it, for a record with headers, HTML-escaped text, a non-ASCII payload and a time
// in another zone, and for one with no headers and no data. The one wire struct, line, writes the
// same bytes, and reads them back as the same record.
//
// It fails when line's fields change order or name, when a field changes its encoding (the time's
// nine digits, the data's base64), or when the headers stop being an object in their own order.
func TestARecordsLineKeepsTheFormatsBytes(t *testing.T) {
	for _, row := range []struct {
		record Record
		golden string
	}{
		{
			Record{
				Seq:     7,
				Subject: "$KV.envoy_session_consumers.s-ses_<a>&b",
				Key:     "s-ses_<a>&b",
				Time:    time.Date(2026, 11, 21, 10, 0, 0, 123456789, time.FixedZone("x", 3600)),
				Headers: Headers{{Name: "KV-Operation", Values: []string{"DEL"}}, {Name: "Nats-Rollup", Values: []string{"sub", "<two>"}}},
				Data:    []byte("{\"name\":\"consumer \u00e9\"}\n"),
			},
			`{"seq":7,"subject":"$KV.envoy_session_consumers.s-ses_\u003ca\u003e\u0026b","key":"s-ses_\u003ca\u003e\u0026b",` +
				`"time":"2026-11-21T09:00:00.123456789Z","headers":{"KV-Operation":["DEL"],"Nats-Rollup":["sub","\u003ctwo\u003e"]},` +
				`"data":"eyJuYW1lIjoiY29uc3VtZXIgw6kifQo="}`,
		},
		{
			Record{Seq: 8, Subject: "$KV.envoy_session_consumers.k", Key: "k", Time: time.Unix(0, 5).UTC()},
			`{"seq":8,"subject":"$KV.envoy_session_consumers.k","key":"k","time":"1970-01-01T00:00:00.000000005Z","headers":{},"data":""}`,
		},
	} {
		encoded, err := encodeLine(row.record)
		require.NoError(t, err)
		require.Equal(t, row.golden, string(encoded), "sequence %d's line", row.record.Seq)
		decoded, err := decodeLine([]byte(row.golden + "\n"))
		require.NoError(t, err)
		require.Equal(t, row.record.Seq, decoded.Seq)
		require.Equal(t, row.record.Subject, decoded.Subject)
		require.Equal(t, row.record.Key, decoded.Key)
		require.True(t, row.record.Time.Equal(decoded.Time), "sequence %d's time read back as %s", row.record.Seq, decoded.Time)
		require.Equal(t, string(row.record.Headers.Block()), string(decoded.Headers.Block()), "sequence %d's headers", row.record.Seq)
		require.Equal(t, string(row.record.Data), string(decoded.Data), "sequence %d's data", row.record.Seq)
	}
}

// A restore writes a stored message's headers back except those that bind the message to the
// stream it was first written to: every Nats-Expected-* header (an expected last sequence, subject
// sequence, stream or message id), Nats-Msg-Id (a duplicate window's id) and every Nats-Batch-*
// header (an atomic batch's id, sequence and commit), each by name, whatever its case. KV-Operation,
// Nats-Rollup and every other header are kept, in their order.
//
// It fails when a binding header is kept, which a fresh stream refuses ("wrong last sequence"), or
// when a KV marker's header is dropped, which turns a delete into an empty put.
func TestARestoreKeepsOnlyTheHeadersAFreshStreamTakes(t *testing.T) {
	headers := Headers{
		{Name: "Nats-Expected-Last-Subject-Sequence", Values: []string{"4"}},
		{Name: "KV-Operation", Values: []string{"DEL"}},
		{Name: "nats-expected-stream", Values: []string{"KV_old"}},
		{Name: "Nats-Expected-Last-Sequence", Values: []string{"9"}},
		{Name: "Nats-Expected-Last-Msg-Id", Values: []string{"m1"}},
		{Name: "Nats-Msg-Id", Values: []string{"m2"}},
		{Name: "Nats-Batch-Id", Values: []string{"b1"}},
		{Name: "Nats-Batch-Sequence", Values: []string{"1"}},
		{Name: "Nats-Batch-Commit", Values: []string{"1"}},
		{Name: "Nats-Rollup", Values: []string{"sub"}},
		{Name: "X-Envoy-Trace", Values: []string{"t1", "t2"}},
	}
	require.Equal(t, Headers{
		{Name: "KV-Operation", Values: []string{"DEL"}},
		{Name: "Nats-Rollup", Values: []string{"sub"}},
		{Name: "X-Envoy-Trace", Values: []string{"t1", "t2"}},
	}, restorable(headers))
	require.Len(t, headers, 11, "restorable changed the headers it was given")
}

// An error of kvrecords names the line, the record or the field and the kind of failure, never a
// value read: a line that does not decode, and a header block that does not parse, each with a
// canary where its value would be, give an error that holds no part of the canary. A records line
// is the record's data and headers, and a header block is a stored message's, so an error that
// carried a value would carry a record's payload into whatever logs it.
//
// It fails when an error prints the value: run by hand, today's decoder prints the seq's digits
// (json's message for a number out of range holds it), the time (time.Parse's message holds the
// text), and parseBlock the header line and the opening line. The data and header value rows do
// not fail today, since json's message for a value of the wrong type names only its JSON type; they
// pin that.
func TestAnErrorNamesNoValue(t *testing.T) {
	const line = `{"seq":1,"subject":"$KV.b.k","key":"k","time":"2026-11-21T10:00:00.000000000Z","headers":{},"data":""}`
	for _, row := range []struct {
		name, canary, from, to, names string
	}{
		{"a seq out of range", "1844674407370955161600123456789", `"seq":1`, `"seq":1844674407370955161600123456789`, `"seq"`},
		{"a negative seq", "-987654321987", `"seq":1`, `"seq":-987654321987`, `"seq"`},
		{"a time that is no time", "CANARY-TIME", `"time":"2026-11-21T10:00:00.000000000Z"`, `"time":"CANARY-TIME"`, `"time"`},
		{"data that is a number", "987654321987", `"data":""`, `"data":987654321987`, `"data"`},
		{"data that is not base64", "CANARYDATA", `"data":""`, `"data":"CANARYDATA!!"`, "data"},
		{"a header value that is a number", "987654321987", `"headers":{}`, `"headers":{"X":[987654321987]}`, `header "X"`},
		{"a header value that is an object", "CANARY-HEADER", `"headers":{}`, `"headers":{"X":[{"CANARY-HEADER":1}]}`, `header "X"`},
		{"a line that is a number", "1844674407370955161600123456789", line, `1844674407370955161600123456789`, "JSON object"},
		{"a line that is not JSON", "ANARY-BARE", `"data":""`, `"data":CANARY-BARE`, "byte"},
	} {
		t.Run(row.name, func(t *testing.T) {
			raw := strings.Replace(line, row.from, row.to, 1)
			require.NotEqual(t, line, raw, "the row changed nothing")
			_, err := decodeLine([]byte(raw + "\n"))
			require.Error(t, err)
			require.NotContains(t, err.Error(), row.canary, "the error prints the value")
			require.Contains(t, err.Error(), row.names, "the error names no field")
		})
	}
	for _, row := range []struct{ name, block, canary, names string }{
		{"an opening line that is not NATS/1.0", "XNATS CANARY-OPEN\r\n\r\n", "CANARY-OPEN", "NATS/1.0"},
		{"a header line with no name", "NATS/1.0\r\nX: a\r\nCANARY-LINE\r\n\r\n", "CANARY-LINE", "line 3"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := parseBlock([]byte(row.block))
			require.Error(t, err)
			require.NotContains(t, err.Error(), row.canary, "the error prints the header block's text")
			require.Contains(t, err.Error(), row.names)
		})
	}
}
