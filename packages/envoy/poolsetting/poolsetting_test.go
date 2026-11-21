package poolsetting_test

import (
	"testing"

	"github.com/sjawhar/envoy/poolsetting"
	"github.com/sjawhar/envoy/poolsetting/poolsettingtest"
)

// Named reads pool_max_conns as pgx reads the settings: in a URL's query, in a keyword/value
// string, and in the service file a `service=` entry names. Text inside a quoted value names
// nothing, and neither does a connection string pgx cannot parse.
func TestNamedReadsTheSettingsAsPgxDoes(t *testing.T) {
	poolsettingtest.ServiceFile(t)
	for _, tc := range []struct {
		name, connString string
		named            bool
	}{
		{"a postgres URL that names it", "postgres://u@h/db?sslmode=disable&pool_max_conns=7", true},
		{"a postgresql URL that names it", "postgresql://u@h/db?pool_max_conns=7", true},
		{"a URL that does not", "postgres://u@h/db?sslmode=disable", false},
		{"a URL that names another pool key", "postgres://u@h/db?pool_min_conns=2", false},
		{"a keyword/value string that names it", "host=h dbname=db pool_max_conns=7", true},
		{"a keyword/value string with spaces around the =", "host=h pool_max_conns = 7", true},
		{"a keyword/value string that does not", "host=h dbname=db", false},
		{"the key inside a quoted value", "host=localhost application_name='worker pool_max_conns=7'", false},
		{"a service whose file names it", "host=h service=" + poolsettingtest.ServiceName, true},
		{"a string pgx cannot parse", "postgres://u@h:port/db?pool_max_conns=7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolsetting.Named(tc.connString); got != tc.named {
				t.Errorf("Named(%q) = %t, want %t", tc.connString, got, tc.named)
			}
		})
	}
}

// Lookup answers what Named does and, for a connection string pgx cannot parse, pgx's own error,
// which Dispatch's Open refuses the string with; a string it can parse brings no error.
func TestLookupReturnsPgxsParseError(t *testing.T) {
	if named, err := poolsetting.Lookup("postgres://u@h:port/db?pool_max_conns=7"); err == nil || named {
		t.Errorf("Lookup of a URL with a port that is no number = %t, %v; want false and pgx's parse error", named, err)
	}
	if named, err := poolsetting.Lookup("host=h pool_max_conns = 7"); err != nil || !named {
		t.Errorf("Lookup(%q) = %t, %v; want true and no error", "host=h pool_max_conns = 7", named, err)
	}
}
