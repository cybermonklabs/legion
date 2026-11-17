package testnats

import (
	"math"
	"testing"
	"time"
)

// A server of this rig answers a request through each service export of its system account that a
// client of the global account reaches through an import, at its start and after a reload and a
// restart, and each of those exports waits for a response longer than any test lives, so its
// response-threshold timer never fires while a test runs (responseThreshold says why). A client of
// the global account reaches four such exports, through the imports nats-server gives it: the
// JetStream API ("$JS.API.>", jetstream.go enableAllJetStreamServiceImportsAndMappings, for an
// account with JetStream) and, from events.go registerSystemImports in v2.15.1, the user's own
// information ("$SYS.REQ.USER.*.INFO") and the account's connections and statistics
// ("$SYS.REQ.ACCOUNT.*.CONNZ" and "$SYS.REQ.ACCOUNT.*.STATZ"). A start, a reload (Grant) and a
// restart are each read, since a restart is a new nats-server and a reload is what the timer races.
func TestTheSystemAccountsExportsOutliveEveryTestAndStillAnswer(t *testing.T) {
	const never = time.Duration(math.MaxInt64)
	allow := []string{"$JS.API.>", "$SYS.REQ.USER.INFO", "$SYS.REQ.ACCOUNT.PING.*", "_INBOX.>"}
	seed, public := User(t)
	grant := StartNkeyGranted(t, public, allow...)
	exports := []struct{ request, export string }{
		{"$JS.API.INFO", "$JS.API.>"},
		{"$SYS.REQ.USER.INFO", "$SYS.REQ.USER.*.INFO"},
		{"$SYS.REQ.ACCOUNT.PING.CONNZ", "$SYS.REQ.ACCOUNT.*.CONNZ"},
		{"$SYS.REQ.ACCOUNT.PING.STATZ", "$SYS.REQ.ACCOUNT.*.STATZ"},
	}
	check := func(when string) {
		t.Helper()
		conn, err := ConnectSeed(t, grant.URL, seed)
		if err != nil {
			t.Fatalf("%s: connect as the granted user: %v", when, err)
		}
		defer conn.Close()
		system := grant.srv.natsServer().SystemAccount()
		for _, e := range exports {
			if _, err := conn.Request(e.request, nil, connectTimeout); err != nil {
				t.Fatalf("%s: %s got no answer through the system account's export %q: %v", when, e.request, e.export, err)
			}
			got, err := system.ServiceExportResponseThreshold(e.export)
			if err != nil {
				t.Fatalf("%s: read the response threshold of the system account's export %q: %v", when, e.export, err)
			}
			if got != never {
				t.Errorf("%s: the system account's export %q, which %s went through, waits %s for a response, want %s: its timer can fire during a test and race a reload", when, e.export, e.request, got, never)
			}
		}
	}

	check("at the start")
	grant.Grant(t, append(allow, "granted.>")...)
	check("after a reload")
	grant.srv.Restart(t, publishGrantConfig(public, allow))
	check("after a restart")
}
