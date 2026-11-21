package testdb

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// processChildEnv carries the admin URL to the test binary TestTwoBinariesEachMakeADatabaseOfTheirOwnAndRunDropsIt
// runs again: set, TestProcessDatabaseChild is that child.
const processChildEnv = "TESTDB_PROCESS_CHILD_ADMIN_URL"

// processChildPrefix is the prefix of the databases the children make.
const processChildPrefix = "testdb_process_test"

// TestProcessDatabaseChild is the child process TestTwoBinariesEachMakeADatabaseOfTheirOwnAndRunDropsIt
// runs, and returns at once in any other run. In the child it does what a package's TestMain and
// tests do, through Run: it asks ProcessDatabaseOn twice for its binary's database, checks that
// both answers are the one database, and prints the database pgx selects from that URL and the
// one the server says the connection is in, which the parent reads.
func TestProcessDatabaseChild(t *testing.T) {
	admin := os.Getenv(processChildEnv)
	if admin == "" {
		return
	}
	code := Run(func() int {
		first := ProcessDatabaseOn(t, admin, processChildPrefix)
		if again := ProcessDatabaseOn(t, admin, processChildPrefix); again != first {
			t.Errorf("a second ProcessDatabaseOn of the same server and prefix answered another database")
		}
		config, err := pgx.ParseConfig(first)
		if err != nil {
			t.Fatal("parse the binary's database URL with pgx") // its error can quote the URL, which carries a credential
		}
		ctx := context.Background()
		conn, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatalf("connect to the binary's database: %v", err)
		}
		defer conn.Close(ctx)
		var current string
		if err := conn.QueryRow(ctx, "select current_database()").Scan(&current); err != nil {
			t.Fatalf("read current_database(): %v", err)
		}
		fmt.Printf("testdb-child: url_database=%s current_database=%s\n", config.Database, current)
		return 0
	})
	if code != 0 {
		t.Fatalf("Run answered %d: a drop failed (printed above)", code)
	}
}

// Two test binaries on one server each get a database of their own, the one their URL names in
// every field pgx selects a database by, and Run drops each once its binary's tests end. The admin
// URL names its own database in the query as well as the path (dbname and database, which pgx
// reads over the path): kept in the binary's URL, they put both binaries in that one database, as
// a reviewer's run did. The row runs this test binary twice, at once, as TestProcessDatabaseChild,
// and reads what each says; then it reads pg_database for the two names.
//
// Controls: keep the query's dbname in databaseURL and the children read the admin's database;
// take the drop out of Run and a child's database is left on the server.
func TestTwoBinariesEachMakeADatabaseOfTheirOwnAndRunDropsIt(t *testing.T) {
	admin := os.Getenv("BROKER_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run the Postgres rows of testdb")
	}
	adminConfig, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal("parse BROKER_TEST_DATABASE_URL with pgx") // its error can quote the URL, which carries a credential
	}
	queried, err := url.Parse(admin)
	if err != nil {
		t.Fatal("parse BROKER_TEST_DATABASE_URL")
	}
	query := queried.Query()
	query.Set("dbname", adminConfig.Database)
	query.Set("database", adminConfig.Database)
	queried.RawQuery = query.Encode()

	line := regexp.MustCompile(`(?m)^testdb-child: url_database=(\S+) current_database=(\S+)$`)
	type child struct {
		cmd *exec.Cmd
		out bytes.Buffer
	}
	children := make([]*child, 2)
	for i := range children {
		c := &child{cmd: exec.Command(os.Args[0], "-test.run=^TestProcessDatabaseChild$", "-test.count=1", "-test.v")}
		c.cmd.Env = append(os.Environ(), processChildEnv+"="+queried.String())
		c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
		if err := c.cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
		children[i] = c
	}
	names := make([]string, 0, len(children))
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v:\n%s", i, err, c.out.String())
		}
		match := line.FindStringSubmatch(c.out.String())
		if match == nil {
			t.Fatalf("child %d printed no testdb-child line:\n%s", i, c.out.String())
		}
		selected, current := match[1], match[2]
		if selected != current {
			t.Errorf("child %d: its URL selects %s, and the server put the connection in %s", i, selected, current)
		}
		if !strings.HasPrefix(current, processChildPrefix+"_") || current == adminConfig.Database {
			t.Errorf("child %d ran in %s, not a database of its own (%s_…): the URL's query chose the database", i, current, processChildPrefix)
			continue // a database the child did not make: read it no further, and never drop it
		}
		names = append(names, current)
	}
	if len(names) == 2 && names[0] == names[1] {
		t.Errorf("both children ran in one database, %s", names[0])
	}
	if len(names) == 0 {
		return
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", adminConfig.Host, err)
	}
	defer conn.Close(ctx)
	var left []string
	rows, err := conn.Query(ctx, "select datname from pg_database where datname = any($1) order by 1", names)
	if err != nil {
		t.Fatalf("read pg_database: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read pg_database: %v", err)
		}
		left = append(left, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_database: %v", err)
	}
	if len(left) > 0 {
		t.Errorf("Run left the children's databases on the server: %v", left)
		for _, name := range left {
			_ = execOn(admin, adminConfig.Host, "drop database if exists "+name+" with (force)")
		}
	}
}
