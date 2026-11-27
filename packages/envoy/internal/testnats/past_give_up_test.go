package testnats

import (
	"net"
	"os"
	"reflect"
	"regexp"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// pastGiveUpChildEnv has TestAConnectionAWaitMadeKeepsWorkingPastTheGiveUp run as its own
	// child (testwait.RunChild), which connects to the server whose URL the variable holds, then
	// publishes and flushes past the shared wait's give-up.
	pastGiveUpChildEnv = "TESTNATS_PAST_GIVE_UP_CHILD"

	// pastGiveUpGuard is the child's -timeout (testwait.RunChild): a hang guard only, which a child
	// whose connect or flush never returns runs into, and never what decides the row. The child's
	// whole run measured 0.01 s beside two busy loops (a builds Job at the commit that made this
	// form) and 0.06 s in an issue pod; two minutes is over a thousand times the slower, and a
	// setup held 19 s still passes inside it.
	pastGiveUpGuard = 2 * time.Minute
)

// pastGiveUpDone is the line the child logs once its publish and its flush past the give-up have
// both succeeded.
var pastGiveUpDone = regexp.MustCompile(`published and flushed \S+ past the shared wait's give-up`)

// A connection a wait made keeps working once the shared wait's give-up has passed: the give-up
// the dial sets as the connection's deadline (giveUpDial) bounds the connect alone, as nats.go's
// processConnectInit sets its own deadline over it and clears both before the connect returns.
// Left on the connection, that deadline would end the read of every connection a test made through
// Connect at the give-up, the last 30 s of every test binary: nats.go closes a connection with no
// reconnect on that read's i/o timeout, and every later publish and flush on it fails. Main's
// TestAConnectionAWaitMadeKeepsWorkingPastTheWaitsEnd held the same of main's wait.
//
// The row runs as its own child. The child connects through Connect on the wall clock, under the
// shared wait's own bound, and reads two values the connection keeps (deadlineConn): the deadline
// its dial gave it, which must be the give-up, and the read deadline it carries once the connect
// has returned, which must be none. It then takes the give-up as a step of its own
// (testwait.SetUpDone) and publishes and flushes past it. The kernel keeps a socket's deadline on
// the wall clock, which a step clock does not move, so a read that a deadline left on the
// connection would fail at the give-up comes only once the wall clock gets there; the row reads
// the deadline itself instead, the state that would fail that read, so its verdict follows from
// what the dial and nats.go did with the deadline, not from how long a loaded machine took over
// the connect. Under a fixed -timeout of 50 s the give-up came 20 s in, and a connect that returned
// less than nats.go's default timeout before it failed the child, since its dial could have given
// it that timeout in place of the give-up. The parent starts the server, under its own deadline,
// before it runs the child. In the child, nothing the row picks bounds the publish or the flush:
// the publish appends its few bytes to the connection's buffer and writes nothing itself, and the
// flush waits for the server's answer at most nats.go's own 10 s (Conn.Flush, nats.go v1.55.0
// nats.go:6162-6164).
func TestAConnectionAWaitMadeKeepsWorkingPastTheGiveUp(t *testing.T) {
	if uri := os.Getenv(pastGiveUpChildEnv); uri != "" {
		publishPastTheGiveUp(t, uri)
		return
	}
	_, uri := Start(t)
	stdout, stderr, err := testwait.RunChild(t, pastGiveUpGuard, "^"+t.Name()+"$", pastGiveUpChildEnv+"="+uri)
	if output := stdout + stderr; err != nil || !pastGiveUpDone.MatchString(output) {
		t.Fatalf("the child's connection did not publish and flush past the shared wait's give-up (%v):\n%s", err, output)
	}
}

// publishPastTheGiveUp is the child of TestAConnectionAWaitMadeKeepsWorkingPastTheGiveUp: it
// connects through Connect to the real server the parent started at uri, requires the dial to have
// given the connection the shared wait's give-up as its deadline and the connect to have left it no
// read deadline, takes the give-up as its next step, then publishes and flushes on the connection,
// and fails unless both succeed.
func publishPastTheGiveUp(t *testing.T, uri string) {
	end, bounded := testwait.GiveUpAt(t)
	if !bounded {
		t.Fatal("the child runs with no deadline: no wait of its gives up, and its dial gives its connection no deadline")
	}
	conn := Connect(t, uri)
	t.Cleanup(conn.Close)
	dialed := dialedConn(t, conn)
	// A dial begun more than nats.go's default timeout before the give-up gives its connection the
	// give-up as its deadline, and one begun later that timeout from the dial (giveUpDialer.Dial).
	want := end
	if own := dialed.dialedAt.Add(natsgo.DefaultTimeout); own.After(want) {
		want = own
	}
	if !dialed.deadline.Equal(want) {
		t.Fatalf("the dial gave its connection the deadline %s, want %s: the shared wait's give-up, %s, or nats.go's default timeout from a dial made later than that before it, at %s",
			dialed.deadline.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), dialed.dialedAt.Format(time.RFC3339Nano))
	}
	if read := dialed.readDeadline(); !read.IsZero() {
		t.Fatalf("the connection still carries the read deadline %s once its connect returned: past it every read of the connection fails, and nats.go closes a connection with no reconnect on that read's i/o timeout", read.Format(time.RFC3339Nano))
	}
	testwait.SetUpDone(t)
	if err := conn.Publish("past.the.give.up", []byte("x")); err != nil {
		t.Fatalf("publish past the shared wait's give-up: %v (the connection's last error: %v)", err, conn.LastError())
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush past the shared wait's give-up: %v (the connection's last error: %v)", err, conn.LastError())
	}
	t.Logf("published and flushed %s past the shared wait's give-up", testwait.Now().Sub(end))
}

// dialedConn is the connection giveUpDialer dialed under conn (deadlineConn). nats.go does not
// export it, so it is read through reflect, with nats.go's connection lock held, as connLock reads
// that lock (Conn.conn, nats.go v1.55.0 nats.go:667); a nats.go whose Conn holds no such field, or
// a connection some other dialer made, fails the test, naming it.
func dialedConn(t testing.TB, conn *natsgo.Conn) *deadlineConn {
	t.Helper()
	lock := connLock(t, conn)
	lock.RLock()
	defer lock.RUnlock()
	field := reflect.ValueOf(conn).Elem().FieldByName("conn")
	if !field.IsValid() || field.Type() != reflect.TypeFor[net.Conn]() {
		t.Fatal("nats.go's Conn holds no conn net.Conn, the connection its dialer made (nats.go v1.55.0 nats.go:667)")
	}
	held := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
	dialed, ok := held.(*deadlineConn)
	if !ok {
		t.Fatalf("the connection under the client is a %T, not one giveUpDialer dialed", held)
	}
	return dialed
}
