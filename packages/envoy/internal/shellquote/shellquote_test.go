package shellquote_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/shellquote"
)

// The repository's one rule for quoting a word for a shell is the table in
// scripts/fixtures/shell-quote.json, which @legion/envoy-client's shellQuote and
// scripts/dev-postgres.sh's quote are held to as well, so the copies cannot drift apart: which
// words stay bare, and that every quoted word reads back whole in sh. The scripts infra writes
// with Quote keep their bytes.
func TestQuoteMatchesTheSharedTable(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/fixtures/shell-quote.json")
	require.NoError(t, err)
	var cases []struct{ Word, Quoted string }
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases, "the shared table holds no case")
	for _, c := range cases {
		// Quote spells a ' inside a quoted word as shlex.quote does, '"'"', where the table spells
		// it '\'' (both close the quote, write one ', and reopen it): a case holding a ' is held
		// to the table with that one spelling swapped.
		want := strings.ReplaceAll(c.Quoted, `'\''`, `'"'"'`)
		require.Equal(t, want, shellquote.Quote(c.Word), "Quote(%q), the table's %q", c.Word, c.Quoted)
		out, err := exec.Command("sh", "-c", "printf '%s' "+shellquote.Quote(c.Word)).Output()
		require.NoError(t, err, "sh reading %s", shellquote.Quote(c.Word))
		require.Equal(t, c.Word, string(out), "sh read %s back", shellquote.Quote(c.Word))
	}
}

// What the table does not hold: the empty word, a line break, a letter that is not ASCII, and the
// paths a temporary directory may name, each read back whole.
func TestQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"line\nbreak", "'line\nbreak'"},
		{"caf\u00e9", "'caf\u00e9'"},
		{"/tmp/tmp with space/x", "'/tmp/tmp with space/x'"},
		{"/tmp/tmp-sq'x/y", `'/tmp/tmp-sq'"'"'x/y'`},
		{"/tmp/tmp-q?x/*", "'/tmp/tmp-q?x/*'"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, shellquote.Quote(c.in), "Quote(%q)", c.in)
		out, err := exec.Command("sh", "-c", "printf '%s' "+shellquote.Quote(c.in)).Output()
		require.NoError(t, err)
		require.Equal(t, c.in, string(out))
	}
}

func TestJoin(t *testing.T) {
	require.Equal(t, `nats-server --config '/tmp/x y' -js ''`,
		shellquote.Join([]string{"nats-server", "--config", "/tmp/x y", "-js", ""}))
}
