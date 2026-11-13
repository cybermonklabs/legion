package testlimits

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// legionTestPG is the pod's test-server script, from this package's directory.
const legionTestPG = "../../../../daemon/docker/legion-testpg"

// postgresSuperuserReserve is Postgres's default superuser_reserved_connections, which
// legion-testpg leaves unset.
const postgresSuperuserReserve = 3

var (
	// maxConnectionsLine is the one assignment of legion-testpg's max_connections variable.
	maxConnectionsLine = regexp.MustCompile(`(?m)^max_connections=(\S*)$`)
	// setsSuperuserReserve is a setting of superuser_reserved_connections, as `-c name=value` or
	// `alter system set name = value` would write it.
	setsSuperuserReserve = regexp.MustCompile(`(?i)superuser_reserved_connections\s*(=|to\b)`)
)

// legion-testpg cannot run Go, so it holds the size of the pod's test server as a literal. The
// literal is ServerConnections plus Postgres's default superuser reserve, the server is started
// with it, and the script leaves that reserve at its default, so a change to any constant here
// fails this test until the script is changed with it.
func TestLegionTestPGAllowsServerConnections(t *testing.T) {
	script, err := os.ReadFile(legionTestPG)
	if err != nil {
		t.Fatalf("read legion-testpg: %v", err)
	}
	text := string(script)
	want := ServerConnections + postgresSuperuserReserve

	matches := maxConnectionsLine.FindAllStringSubmatch(text, -1)
	if len(matches) != 1 {
		t.Fatalf("%s assigns max_connections %d times; want one line max_connections=%d "+
			"(testlimits.ServerConnections=%d + Postgres's default superuser reserve of %d)",
			legionTestPG, len(matches), want, ServerConnections, postgresSuperuserReserve)
	}
	if got, err := strconv.Atoi(matches[0][1]); err != nil || got != want {
		t.Errorf("%s sets max_connections=%s; write max_connections=%d "+
			"(testlimits.ServerConnections=%d + Postgres's default superuser reserve of %d)",
			legionTestPG, matches[0][1], want, ServerConnections, postgresSuperuserReserve)
	}
	if !strings.Contains(text, "-c max_connections=$max_connections ") {
		t.Errorf("%s does not start its server with -c max_connections=$max_connections, "+
			"so the variable this test checks is not what the server allows", legionTestPG)
	}
	if setsSuperuserReserve.MatchString(text) {
		t.Errorf("%s sets superuser_reserved_connections, and its max_connections is sized for "+
			"Postgres's default of %d: remove the setting, or size max_connections from it",
			legionTestPG, postgresSuperuserReserve)
	}
}
