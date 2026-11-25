package secretstore

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// shellUnsafe is any character outside the set a POSIX shell reads literally in a bare word.
var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_./:@-]`)

// ShellWord is s as one shell word: bare when every character is literal, else single-quoted with
// each `'` closed, escaped and reopened.
func ShellWord(s string) string {
	if s != "" && !shellUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Command is the command line a form prints for the person to run: prefix (the writer's
// RerunPrefix), agent-secrets, then words, each a ShellWord, so running it signs in the same way.
func Command(prefix string, words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = ShellWord(w)
	}
	return prefix + "agent-secrets " + strings.Join(quoted, " ")
}

// CallDeadline bounds each store call a secret form makes, and so how long a create holds the
// prompt or a signal waits on it. It is 60 s, Secret Manager's Go client's own default on each of
// those calls (CreateSecret, AddSecretVersion, ListSecretVersions and DeleteSecret:
// defaultCallOptions in cloud.google.com/go/secretmanager/apiv1), which that client applies only
// to a call whose context carries no deadline of its own. The AWS SDK puts none on a call - its
// defaults bound the dial, at 30 s, and the TLS handshake, at 10 s, not the wait for an answer - so
// AWS's one CreateSecret takes the same 60 s. Tests shorten it.
var CallDeadline = 60 * time.Second

// WithCallDeadline is ctx bounded by CallDeadline, for one store call.
func WithCallDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, CallDeadline)
}

// CallDeadlineText is CallDeadline as a line says it: whole seconds as such ("60s"), any other
// length as Go writes it.
func CallDeadlineText() string {
	if CallDeadline >= time.Second && CallDeadline%time.Second == 0 {
		return strconv.FormatInt(int64(CallDeadline/time.Second), 10) + "s"
	}
	return CallDeadline.String()
}
