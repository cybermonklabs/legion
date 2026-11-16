package textcheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A bearer token given to curl as a header argument, `-H "Authorization: Bearer $(cat …)"`, is in
// that curl's command line, which every process of the pod can read in /proc for as long as the
// call runs, and a role copies the commands its texts give it. So every command the repository
// gives an agent or an operator sends the header on standard input instead: `printf
// 'Authorization: Bearer %s' "$token" | curl -H @- …`, where printf is a shell builtin that starts
// no process, or from a file curl reads (`-H @<(printf …)`, `-H @"$file"`). This check holds every
// such text to that: the skills, the daemon's prompts, every package's Markdown, the docs but
// docs/plans and docs/superpowers, which are dated history, and .github's scripts and workflows.
// (dispatch://INFRA-3950)

// bearerArgument matches a header argument that carries a bearer from a variable, a command
// substitution or a placeholder for one, in each spelling a text of this repository writes:
// `-H "Authorization: Bearer $…`, `--header='Authorization: Bearer '"$…`, `-H "Authorization:
// Bearer \$…` inside a heredoc that writes a script, and `-H "Authorization: Bearer <token>"`.
// A header curl reads with `-H @…` names no `Authorization:` after the flag, so it matches nothing.
var bearerArgument = regexp.MustCompile(`(?i)(?:^|[^\w-])(?:-H|--header)(?:=|\s*)\\?["']?authorization:\s*(?:bearer|token)\s+(?:\\?["'])*\\?[$<]`)

// bearerTextRoot is one tree the check reads, and which of its files it reads.
type bearerTextRoot struct {
	dir  string
	read func(rel string) bool
}

var bearerTextRoots = []bearerTextRoot{
	{"skills", func(string) bool { return true }},
	{"docs", func(rel string) bool {
		return !strings.HasPrefix(rel, "docs/plans/") && !strings.HasPrefix(rel, "docs/superpowers/")
	}},
	{".github", func(string) bool { return true }},
	{"packages", func(rel string) bool {
		return strings.HasSuffix(rel, ".md") || slices.Contains(strings.Split(rel, "/"), "prompts")
	}},
}

// bearerSkippedDirs are trees nothing commits: installed dependencies, build output, and the
// Claude Code plugin's skills, which packages/claude-envoy/scripts/build.ts generates from the
// skills/ this check reads.
var bearerSkippedDirs = []string{"node_modules", "dist", "packages/claude-envoy/legion"}

func TestNoTextGivesABearerAsACurlArgument(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	var offences []string
	for _, tree := range bearerTextRoots {
		read := 0
		err := filepath.WalkDir(filepath.Join(root, tree.dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if entry.IsDir() {
				if slices.Contains(bearerSkippedDirs, entry.Name()) || slices.Contains(bearerSkippedDirs, rel) {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || !tree.read(rel) {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			read++
			for number, line := range strings.Split(string(body), "\n") {
				if bearerArgument.MatchString(line) {
					offences = append(offences, rel+":"+strconv.Itoa(number+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read %s: %v", tree.dir, err)
		}
		// A tree this walk read nothing of is a check that saw nothing, never a pass.
		if read == 0 {
			t.Fatalf("read no file under %s, so this check saw none of it", tree.dir)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("%d lines give a bearer token to curl as a header argument, where every process of the pod reads it:\n\t%s\n"+
			"send the header on standard input instead: printf 'Authorization: Bearer %%s' \"$token\" | curl -H @- …",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// The matcher is what the check above can be blind through, so it is held to each spelling it
// must catch and to each form the repository writes in its place.
func TestTheBearerArgumentMatcherCatchesEachSpellingAndPassesAHeaderCurlReads(t *testing.T) {
	for _, line := range []string{
		`curl -sf -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/whoami"`,
		`  -H "Authorization: Bearer $token" "$@" "$url") || status=000`,
		`curl -fsS -H "Authorization: Bearer ${agent_token}" "$url"`,
		`curl -H 'Authorization: Bearer '"$TOKEN" "$url"`,
		`curl --header="Authorization: Bearer $TOKEN" "$url"`,
		`curl -H"authorization: bearer $TOKEN" "$url"`,
		`curl -fsS -H "Authorization: Bearer \$$name" $AIRLOCK_S_URL/ok >curl.out`,
		`say "post=\$(curl -sS -H \"Authorization: Bearer \$P\" $AIRLOCK_S_URL/read)"`,
		`credential=(-H "Authorization: Bearer $agent_token")`,
		`With ENVOY_API_TOKEN set, add -H "Authorization: Bearer <token>".`,
		`curl -H "Authorization: token $GITHUB_TOKEN" https://api.github.com/user`,
	} {
		if !bearerArgument.MatchString(line) {
			t.Errorf("does not catch a bearer given as an argument: %s", line)
		}
	}
	for _, line := range []string{
		`printf 'Authorization: Bearer %s' "$(cat "$DISPATCH_TOKEN_FILE")" | curl -sf -H @- "$DISPATCH_URL/api/v1/whoami"`,
		`curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$url"`,
		`curl -fsS -H "@$work/envoy-auth-header" -H 'Content-Type: application/json' "$@"`,
		`sh -c 'printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" | curl -sS -H @- https://slack.com/api/auth.test'`,
		`curl -fsS -H 'Authorization: Bearer e2e-token' "$url"`,
		`request.add_header("Authorization", f"Bearer {token}")`,
		`curl -H 'Content-Type: application/json' -d "$body" "$url"`,
	} {
		if bearerArgument.MatchString(line) {
			t.Errorf("catches a line that gives curl no bearer as an argument: %s", line)
		}
	}
}
