package testnats

import (
	"fmt"
	"math"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// responseThreshold is how long each service export of a server's system account waits for a
// response before nats-server drops the routing it keeps for it: longer than any process lives, so
// the export's response-threshold timer never fires while a test runs.
//
// That timer meets a data race in nats-server's config reload. Every reload that touches
// authorization (Reload, and so NkeyGrant.Grant, and the shared server's account doubling) rebinds
// each service export's account, under the exporting account's lock (configureAccounts, `se.acc =
// v.(*Account)`, server/server.go:1383 in v2.15.1 and :1388 at main 08f7bd21cee8), and the
// export's timer reads that field without the lock when it fires (checkExpiredResponses, `acc :=
// se.acc`, server/accounts.go:2574 in v2.15.1 and :2611 at 08f7bd21cee8). A request with a reply
// through a service import arms the timer of the export it reaches - a JetStream API request, say,
// through the system account's "$JS.API.>" export - and it fires nats-server's default two minutes
// later (DEFAULT_SERVICE_EXPORT_RESPONSE_THRESHOLD), so -race reported a test whose server had
// served such a request, then reloaded, and lived two minutes past the request, as a loaded machine
// makes a test live. No lock this process can take orders the two, and the timer reads the field
// before it takes any lock of its own, so the rig keeps the timer from firing instead.
//
// Only the exports of the implicit system account, the one nats-server makes when the
// configuration declares none, outlive a reload: each reload gives an account the configuration
// declares fresh copies of the exports the configuration names (shallowCopy,
// server/accounts.go:436-446 in v2.15.1), which no request has reached yet. No configuration here
// declares a system account, so the rig sets that account's exports alone. The cost is the cleanup
// the threshold drives: the routing nats-server keeps for a response, which this timer drops two
// minutes on when nothing else has (a response that never came, a streamed export's), stays until
// its requester drops the reply subscription or the server stops. No test waits two minutes for a
// response.
const responseThreshold = time.Duration(math.MaxInt64)

// outliveResponseTimers sets every service export of srv's system account to wait
// responseThreshold for a response. launch calls it before any client can reach srv: no request
// has armed a timer yet, which matters because a timer that fires reads the threshold without the
// lock as well. A server without a system account has no such export.
func outliveResponseTimers(srv *natsserver.Server) error {
	system := srv.SystemAccount()
	if system == nil {
		return nil
	}
	accounts, err := srv.Accountz(&natsserver.AccountzOptions{Account: system.GetName()})
	if err != nil {
		return fmt.Errorf("list the exports of the system account %s: %w", system.GetName(), err)
	}
	for _, export := range accounts.Account.Exports {
		if export.Type != jwt.Service {
			continue
		}
		if err := system.SetServiceExportResponseThreshold(string(export.Subject), responseThreshold); err != nil {
			return fmt.Errorf("set the response threshold of the system account's export %q: %w", export.Subject, err)
		}
	}
	return nil
}
