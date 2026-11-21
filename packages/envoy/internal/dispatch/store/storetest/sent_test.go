package storetest

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// encoded is the wire bytes of messages, in order.
func encoded[M interface{ Encode([]byte) ([]byte, error) }](t *testing.T, messages ...M) []byte {
	t.Helper()
	var bytes []byte
	for _, message := range messages {
		var err error
		if bytes, err = message.Encode(bytes); err != nil {
			t.Fatalf("encode %T: %v", message, err)
		}
	}
	return bytes
}

// fed is a sendingConn on no network connection that has read written as the connection's writes
// and then answered as the server's answers, each in pieces of width bytes.
func fed(written, answered []byte, width int) (*SentStatements, *sendingConn) {
	sent := &SentStatements{}
	conn := newSendingConn(nil, sent)
	for start := 0; start < len(written); start += width {
		conn.readWritten(written[start:min(start+width, len(written))])
	}
	for start := 0; start < len(answered); start += width {
		conn.readAnswered(answered[start:min(start+width, len(answered))])
	}
	return sent, conn
}

// startup is a startup packet of protocol 3.0 with no parameters.
func startup() []byte {
	packet := make([]byte, startupHeaderBytes)
	binary.BigEndian.PutUint32(packet, startupHeaderBytes)
	binary.BigEndian.PutUint32(packet[4:], 196608)
	return packet
}

// The recorder reads a statement whatever the reads and writes cut its bytes into: here a startup,
// a statement prepared by name, a portal bound to it and a second bound and never run, an Execute
// of the first, a Sync and a ping, and the server's answers to each, a few bytes at a time at every
// width. It records the Execute, with the values its portal was bound with, once the server says
// it ran it, and the ping - not the Bind that nothing ran. An answer nothing waits for, and a run
// of a portal the server never acknowledged binding, are each something it cannot read, which it
// reports rather than passing over; an Execute the server refuses is recorded as failed.
func TestSentStatementsReadEveryWriteWhateverItsCut(t *testing.T) {
	const query = "select $1::bigint, $2::text"
	written := append(startup(), encoded[pgproto3.FrontendMessage](t,
		&pgproto3.Parse{Name: "rule", Query: query},
		&pgproto3.Bind{DestinationPortal: "named", PreparedStatement: "rule", Parameters: [][]byte{[]byte("42"), []byte("destination")}},
		&pgproto3.Bind{DestinationPortal: "unused", PreparedStatement: "rule", Parameters: [][]byte{[]byte("7"), []byte("elsewhere")}},
		&pgproto3.Execute{Portal: "named"},
		&pgproto3.Sync{},
		&pgproto3.Query{String: PoolPing},
	)...)
	answered := encoded[pgproto3.BackendMessage](t,
		&pgproto3.AuthenticationOk{},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("42"), []byte("destination")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
		&pgproto3.EmptyQueryResponse{},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	for width := 1; width <= max(len(written), len(answered)); width++ {
		sent, _ := fed(written, answered, width)
		got := sent.Statements()
		if len(got) != 2 || got[0].SQL != query || got[0].Failed || got[1].SQL != PoolPing || len(sent.Blind()) != 0 {
			t.Fatalf("read %d bytes at a time: recorded %+v, blind %v; want the Execute and the ping", width, got, sent.Blind())
		}
		event, readEvent := got[0].Int8(0)
		destination, readDestination := got[0].Text(1)
		if !readEvent || !readDestination || event != 42 || destination != "destination" {
			t.Fatalf("read %d bytes at a time: the Execute's values read %d %q, want 42 and destination", width, event, destination)
		}
	}

	greeted := encoded[pgproto3.BackendMessage](t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	sent, _ := fed(startup(), append(greeted, encoded[pgproto3.BackendMessage](t, &pgproto3.BindComplete{})...), 1)
	if blind := sent.Blind(); len(blind) != 1 {
		t.Fatalf("an answer nothing sent waits for: blind %v, want it reported", blind)
	}
	sent, _ = fed(append(startup(), encoded[pgproto3.FrontendMessage](t, &pgproto3.Execute{Portal: "never bound"}, &pgproto3.Sync{})...),
		append(greeted, encoded[pgproto3.BackendMessage](t, &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})...), 1)
	if blind := sent.Blind(); len(blind) != 1 || len(sent.Statements()) != 0 {
		t.Fatalf("a run of a portal never bound: blind %v, recorded %+v; want it reported and nothing recorded", blind, sent.Statements())
	}
	sent, _ = fed(append(startup(), encoded[pgproto3.FrontendMessage](t,
		&pgproto3.Parse{Query: query}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})...),
		append(greeted, encoded[pgproto3.BackendMessage](t,
			&pgproto3.ParseComplete{}, &pgproto3.BindComplete{},
			&pgproto3.ErrorResponse{Severity: "ERROR", Code: "25P02", Message: "current transaction is aborted"},
			&pgproto3.ReadyForQuery{TxStatus: 'E'})...), 1)
	if got := sent.Statements(); len(got) != 1 || !got[0].Failed || got[0].Code != "25P02" || got[0].SQL != query || len(sent.Blind()) != 0 {
		t.Fatalf("an Execute the server refused: recorded %+v, blind %v; want it recorded as failed, with its code", got, sent.Blind())
	}
}

// drain reads the server's messages up to its next ReadyForQuery, failing on an error unless
// refused, and answers whether it refused one.
func drain(t *testing.T, frontend *pgproto3.Frontend, refused bool) bool {
	t.Helper()
	failed := false
	for {
		message, err := frontend.Receive()
		if err != nil {
			t.Fatalf("read the server's answer: %v", err)
		}
		switch message := message.(type) {
		case *pgproto3.ErrorResponse:
			if !refused {
				t.Fatalf("the server refused: %s %s", message.Code, message.Message)
			}
			failed = true
		case *pgproto3.ReadyForQuery:
			return failed
		}
	}
}

// counted is how many statements recorded since the before'th one the server ran with the SQL
// query, and how many it refused.
func counted(sent *SentStatements, before int, query string) (ran, refused int) {
	for _, statement := range sent.Statements()[before:] {
		switch {
		case statement.Failed:
			refused++
		case statement.SQL == query:
			ran++
		}
	}
	return ran, refused
}

// The recorder counts what the server runs, which a sequence it advances counts too: each
// statement is recorded when the server says it ran it, so a portal bound in one round trip of a
// transaction and run in a later one is recorded in the later, and a Bind nothing runs is not
// recorded at all. A portal ends with its transaction: once the server says none is open, an
// Execute of it is one the server refuses, which the recorder records as failed. Every way pgx
// sends a statement shows - each of its four modes of running a query, a statement prepared and
// run by name, a batch - and so does the pool's ping, with nothing the recorder could not read.
func TestSentStatementsCountWhatTheServerRuns(t *testing.T) {
	ctx := context.Background()
	database := Open(t)
	sent := &SentStatements{}
	recorded := SentStore(t, database, sent)
	conn, err := recorded.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	defer conn.Release()
	for _, statement := range []string{"create sequence sent_runs", "create sequence sent_bound", "begin"} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	const run, bound = "select nextval('sent_runs')", "select nextval('sent_bound')"
	frontend := conn.Conn().PgConn().Frontend()
	before := len(sent.Statements())
	frontend.Send(&pgproto3.Parse{Name: "runs", Query: run})
	frontend.Send(&pgproto3.Bind{DestinationPortal: "later", PreparedStatement: "runs"})
	frontend.Send(&pgproto3.Parse{Name: "bound", Query: bound})
	frontend.Send(&pgproto3.Bind{DestinationPortal: "never run", PreparedStatement: "bound"})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("bind the portals: %v", err)
	}
	drain(t, frontend, false)
	ranAtBind, _ := counted(sent, before, run)
	boundAtBind, _ := counted(sent, before, bound)
	before = len(sent.Statements())
	frontend.Send(&pgproto3.Execute{Portal: "later"})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("run the portal: %v", err)
	}
	drain(t, frontend, false)
	ranLater, _ := counted(sent, before, run)
	if _, err := conn.Exec(ctx, "commit"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var runs int64
	var boundCalled bool
	if err := database.Pool.QueryRow(ctx, `select (select last_value from sent_runs), (select is_called from sent_bound)`).Scan(&runs, &boundCalled); err != nil {
		t.Fatalf("read the sequences: %v", err)
	}
	if runs != 1 || boundCalled || ranAtBind+boundAtBind != 0 || ranLater != 1 {
		t.Fatalf("the server ran the bound-then-run portal %d times and the never-run one %v; recorded %d at the Binds and %d at the later Execute; want 1, false, 0 and 1",
			runs, boundCalled, ranAtBind+boundAtBind, ranLater)
	}

	before = len(sent.Statements())
	frontend.Send(&pgproto3.Execute{Portal: "later"})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("run the portal after its transaction: %v", err)
	}
	if !drain(t, frontend, true) {
		t.Fatal("control: the server ran a portal whose transaction had ended")
	}
	if ran, refused := counted(sent, before, run); ran != 0 || refused != 1 || len(sent.Blind()) != 0 {
		t.Fatalf("an Execute of a portal whose transaction ended: recorded %d run and %d refused, blind %v; want 0, 1 and nothing it could not read",
			ran, refused, sent.Blind())
	}

	sent = &SentStatements{}
	recorded = SentStore(t, database, sent)
	const read = "select $1::bigint"
	ways := 0
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec} {
		for range 2 {
			var value int64
			if err := recorded.Pool.QueryRow(ctx, read, mode, int64(42)).Scan(&value); err != nil {
				t.Fatalf("run the read in mode %v: %v", mode, err)
			}
			ways++
		}
	}
	named, err := recorded.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	if _, err := named.Conn().Prepare(ctx, "named", read); err != nil {
		t.Fatalf("prepare the read by name: %v", err)
	}
	for range 2 {
		var value int64
		if err := named.QueryRow(ctx, "named", int64(42)).Scan(&value); err != nil {
			t.Fatalf("run the read by name: %v", err)
		}
		ways++
	}
	if err := named.Conn().PgConn().Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	named.Release()
	batch := &pgx.Batch{}
	batch.Queue(read, int64(42))
	batch.Queue(read, int64(42))
	if err := recorded.Pool.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatalf("run the batch: %v", err)
	}
	ways += 2
	reads, pings := 0, 0
	for _, statement := range sent.Statements() {
		switch {
		case statement.Failed:
			t.Errorf("the server refused %q (%s), want every statement run", statement.SQL, statement.Code)
		case statement.SQL == read:
			reads++
			if value, readValue := statement.Int8(0); !readValue || value != 42 {
				t.Errorf("a read was recorded with the value %d (read %v), want 42", value, readValue)
			}
		case statement.SQL == PoolPing:
			pings++
		}
	}
	if reads != ways || pings == 0 || len(sent.Blind()) != 0 {
		t.Fatalf("recorded %d reads and %d pings, blind %v; want %d reads, the ping and nothing it could not read", reads, pings, sent.Blind(), ways)
	}
}

// What the server refuses or skips is not a statement it ran: a Bind it refuses binds nothing, and
// the Execute after it is skipped with every message up to the Sync; an Execute in a transaction
// that already failed is refused; an Execute after a Parse the server refuses is skipped. In each
// the recorder counts as run only what the server ran, keeps the values the portal held before the
// refused Bind, and records each refused Execute as failed, with nothing it could not read.
func TestSentStatementsCountNothingTheServerRefuses(t *testing.T) {
	ctx := context.Background()
	database := Open(t)
	const read = "select $1::bigint"
	for _, scenario := range []struct {
		name         string
		ran, refused int
	}{
		{name: "a Bind refused after the statement is closed", ran: 1},
		{name: "an Execute in a failed transaction", refused: 1},
		{name: "an Execute after a refused Parse"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			sent := &SentStatements{}
			conn, err := SentStore(t, database, sent).Pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire a connection: %v", err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "begin"); err != nil {
				t.Fatalf("begin: %v", err)
			}
			frontend := conn.Conn().PgConn().Frontend()
			round := func(refused bool, messages ...pgproto3.FrontendMessage) {
				t.Helper()
				for _, message := range messages {
					frontend.Send(message)
				}
				if err := frontend.Flush(); err != nil {
					t.Fatalf("send: %v", err)
				}
				if drain(t, frontend, refused) != refused {
					t.Fatalf("the server refused nothing, want the refusal the scenario makes")
				}
			}
			messages := []pgproto3.FrontendMessage{
				&pgproto3.Parse{Name: "statement", Query: read},
				&pgproto3.Bind{DestinationPortal: "portal", PreparedStatement: "statement", Parameters: [][]byte{[]byte("55")}},
			}
			switch scenario.name {
			case "a Bind refused after the statement is closed":
				messages = append(messages,
					&pgproto3.Execute{Portal: "portal"},
					&pgproto3.Close{ObjectType: 'S', Name: "statement"},
					&pgproto3.Parse{Name: "statement", Query: read},
					&pgproto3.Bind{DestinationPortal: "portal", PreparedStatement: "statement", Parameters: [][]byte{[]byte("56")}},
				)
			case "an Execute in a failed transaction":
				round(false, append(messages, &pgproto3.Sync{})...)
				round(true, &pgproto3.Query{String: "select 1/0"})
				messages = nil
			case "an Execute after a refused Parse":
				messages = append(messages, &pgproto3.Parse{Name: "refused", Query: "select ("})
			}
			before := len(sent.Statements())
			round(true, append(messages, &pgproto3.Execute{Portal: "portal"}, &pgproto3.Sync{})...)
			ran, refused := counted(sent, before, read)
			if ran != scenario.ran || refused != scenario.refused || len(sent.Blind()) != 0 {
				t.Fatalf("recorded %d run and %d refused, blind %v; want %d run, %d refused and nothing it could not read",
					ran, refused, sent.Blind(), scenario.ran, scenario.refused)
			}
			for _, statement := range sent.Statements()[before:] {
				if value, readValue := statement.Int8(0); statement.SQL == read && !statement.Failed && (!readValue || value != 55) {
					t.Errorf("the run was recorded with the value %d (read %v), want 55, the portal's value before the refused Bind", value, readValue)
				}
			}
		})
	}
}

// A client may send a Sync right after an Execute, before it reads whether the Execute started a
// COPY FROM, and the server ignores every Sync and Flush it gets in copy-in. The recorder drops
// those Syncs too, so the statement after the copy pairs with its own answers: here a COPY FROM
// run with a Sync sent at once and ended by a CopyDone, which the server runs, or by a CopyFail,
// which it refuses; and then a select, which the recorder records as run either way, with nothing
// it could not read. Each scenario sends its end of the copy both after reading the
// CopyInResponse and before it, in the same write as the Execute.
func TestSentStatementsIgnoreTheSyncsCopyInIgnores(t *testing.T) {
	ctx := context.Background()
	database := Open(t)
	const copying, after = "copy copy_sync from stdin", "select 99"
	for _, scenario := range []struct {
		name         string
		end          pgproto3.FrontendMessage
		ran, refused int
		early        bool
	}{
		{name: "ended by a CopyDone", end: &pgproto3.CopyDone{}, ran: 1},
		{name: "ended by a CopyFail", end: &pgproto3.CopyFail{Message: "the client gave up"}, refused: 1},
		{name: "ended by a CopyDone sent with the Execute", end: &pgproto3.CopyDone{}, ran: 1, early: true},
		{name: "ended by a CopyFail sent with the Execute", end: &pgproto3.CopyFail{Message: "the client gave up"}, refused: 1, early: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			sent := &SentStatements{}
			conn, err := SentStore(t, database, sent).Pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire a connection: %v", err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "create temporary table copy_sync (n int)"); err != nil {
				t.Fatalf("create the table: %v", err)
			}
			frontend := conn.Conn().PgConn().Frontend()
			send := func(messages ...pgproto3.FrontendMessage) {
				t.Helper()
				for _, message := range messages {
					frontend.Send(message)
				}
				if err := frontend.Flush(); err != nil {
					t.Fatalf("send: %v", err)
				}
			}
			before := len(sent.Statements())
			start := []pgproto3.FrontendMessage{&pgproto3.Parse{Query: copying}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{}}
			end := []pgproto3.FrontendMessage{&pgproto3.CopyData{Data: []byte("1\n")}, scenario.end, &pgproto3.Sync{}}
			if scenario.early {
				send(append(start, end...)...)
			} else {
				send(start...)
				for {
					message, err := frontend.Receive()
					if err != nil {
						t.Fatalf("read the server's answer: %v", err)
					}
					if refusal, refused := message.(*pgproto3.ErrorResponse); refused {
						t.Fatalf("the server refused the copy: %s %s", refusal.Code, refusal.Message)
					}
					if _, copyIn := message.(*pgproto3.CopyInResponse); copyIn {
						break
					}
				}
				send(end...)
			}
			drain(t, frontend, scenario.refused > 0)
			send(&pgproto3.Parse{Query: after}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
			drain(t, frontend, false)
			ranCopy, refused := counted(sent, before, copying)
			ranAfter, _ := counted(sent, before, after)
			if ranCopy != scenario.ran || refused != scenario.refused || ranAfter != 1 || len(sent.Blind()) != 0 {
				t.Fatalf("recorded the copy %d run and %d refused, the select after it %d run, blind %v; want %d, %d, 1 and nothing it could not read",
					ranCopy, refused, ranAfter, sent.Blind(), scenario.ran, scenario.refused)
			}
		})
	}
}

// A refusal ends a copy, and the recorder forgets it: pgx's own CopyFrom into a table that does not
// exist, whose data and CopyDone pgx sends before it reads the refusal, and an extended COPY FROM
// whose data the server refuses before the client's CopyDone. In each the copy is recorded as
// refused, and the select after it on the same connection as run, with nothing the recorder could
// not read.
func TestSentStatementsForgetACopyTheServerRefuses(t *testing.T) {
	ctx := context.Background()
	database := Open(t)
	const after = "select 1"
	for _, scenario := range []struct {
		name string
		copy func(t *testing.T, conn *pgx.Conn)
	}{
		{name: "pgx's CopyFrom into a missing table", copy: func(t *testing.T, conn *pgx.Conn) {
			if _, err := conn.PgConn().CopyFrom(ctx, strings.NewReader("1\n"), "copy no_such_table from stdin"); err == nil {
				t.Fatal("control: the server took a copy into a table that does not exist")
			}
		}},
		{name: "an extended copy whose data is refused before its CopyDone", copy: func(t *testing.T, conn *pgx.Conn) {
			if _, err := conn.Exec(ctx, "create temporary table copy_refused (n int)"); err != nil {
				t.Fatalf("create the table: %v", err)
			}
			frontend := conn.PgConn().Frontend()
			send := func(messages ...pgproto3.FrontendMessage) {
				t.Helper()
				for _, message := range messages {
					frontend.Send(message)
				}
				if err := frontend.Flush(); err != nil {
					t.Fatalf("send: %v", err)
				}
			}
			until := func(want func(pgproto3.BackendMessage) bool) {
				t.Helper()
				for {
					message, err := frontend.Receive()
					if err != nil {
						t.Fatalf("read the server's answer: %v", err)
					}
					if want(message) {
						return
					}
				}
			}
			send(&pgproto3.Parse{Query: "copy copy_refused from stdin"}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
			until(func(message pgproto3.BackendMessage) bool {
				_, copyIn := message.(*pgproto3.CopyInResponse)
				return copyIn
			})
			send(&pgproto3.CopyData{Data: []byte("not a number\n")})
			until(func(message pgproto3.BackendMessage) bool {
				_, refused := message.(*pgproto3.ErrorResponse)
				return refused
			})
			send(&pgproto3.Sync{})
			until(func(message pgproto3.BackendMessage) bool {
				_, ready := message.(*pgproto3.ReadyForQuery)
				return ready
			})
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			sent := &SentStatements{}
			conn, err := SentStore(t, database, sent).Pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire a connection: %v", err)
			}
			defer conn.Release()
			before := len(sent.Statements())
			scenario.copy(t, conn.Conn())
			var one int
			if err := conn.QueryRow(ctx, after).Scan(&one); err != nil {
				t.Fatalf("the select after the copy: %v", err)
			}
			ran, refused := counted(sent, before, after)
			if ran != 1 || refused != 1 || len(sent.Blind()) != 0 {
				t.Fatalf("recorded the select after the copy %d run and %d statements refused, blind %v; want 1, 1 (the copy) and nothing it could not read",
					ran, refused, sent.Blind())
			}
		})
	}
}
