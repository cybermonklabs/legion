package storetest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestMain runs the package's tests through Main, which drops the test binary's own database that
// URL makes each test's schema in.
func TestMain(m *testing.M) { os.Exit(Main(m)) }

// A test schema's URL keeps the server URL's other pairs as pgx reads them: on a
// BROKER_TEST_DATABASE_URL whose options pair holds an escaped space, URL migrates its schema and
// answers a URL whose connections run with that option and in that schema, and with the params it
// was given. Every test that goes through URL, ciphertest.NewRig's among them, reads the server URL
// this way. Control: re-encode the query in URL (url.Values.Encode, as it did) and URL fails to
// open its schema: unrecognized configuration parameter "+work_mem".
func TestAURLOnAServerURLWithAnOptionsSpaceConnectsWithTheOption(t *testing.T) {
	server := serverURL(t)
	separator := "?"
	if strings.Contains(server, "?") {
		separator = "&"
	}
	t.Setenv(DatabaseURLEnv, server+separator+"options=-c%20work_mem%3D7MB")
	dsn := URL(t, "application_name=storetest url")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test schema: %v", err)
	}
	defer conn.Close(ctx)
	var workMem, searchPath, applicationName string
	if err := conn.QueryRow(ctx, "select current_setting('work_mem'), current_setting('search_path'), current_setting('application_name')").Scan(&workMem, &searchPath, &applicationName); err != nil {
		t.Fatalf("read the connection's settings: %v", err)
	}
	if workMem != "7MB" || !strings.HasPrefix(searchPath, "broker_test_") || applicationName != "storetest url" {
		t.Errorf("work_mem %q, search_path %q, application_name %q; want 7MB, the test's schema (broker_test_…) and %q", workMem, searchPath, applicationName, "storetest url")
	}
	var tables int
	if err := conn.QueryRow(ctx, "select count(*) from information_schema.tables where table_schema = $1", searchPath).Scan(&tables); err != nil {
		t.Fatalf("count the schema's tables: %v", err)
	}
	if tables == 0 {
		t.Errorf("the test schema %s holds no table: URL did not migrate it", searchPath)
	}
}
