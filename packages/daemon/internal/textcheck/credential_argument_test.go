package textcheck

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A credential given to curl as an argument - a bearer in `-H "Authorization: Bearer $(cat …)"`, a
// password in `-u "$user:$token"`, a token in a body `-d "token=$token"` - is in that curl's
// command line, which every process of the pod or machine can read in /proc for as long as the call
// runs, and a role copies the commands its texts give it. So every command this repository writes
// or gives an agent or an operator sends the credential on standard input or from a file instead:
// `printf 'Authorization: Bearer %s' "$token" | curl -H @- …` (printf is a shell builtin, which
// starts no process), `-H @<(printf …)`, `--data @-`, or a curl config on standard input (`-K -`).
// This check holds every text file of the repository to that, but docs/plans and
// docs/superpowers, which are dated history.

// credentialHeader matches a header argument that carries a credential from a variable, a command
// substitution or a placeholder for one, in each spelling a text of this repository writes:
// `-H "Authorization: Bearer $…`, `--header='Authorization: Bearer '"$…`, `-H "Authorization:
// Bearer \$…` inside a heredoc that writes a script, and `-H "Authorization: Bearer <token>"`. The
// credential is a bearer, a GitHub `token` or a `Basic` password, or the value of a header named
// for a key or a token (`X-Api-Key:`, `x-goog-api-key:`, `X-Auth-Token:`, IMDSv2's
// `X-aws-ec2-metadata-token:`); `X-Key:` and `Idempotency-Key:` name no credential. A whole header
// in a variable named for one (`-H "$auth_header"`) matches too. A header curl reads with `-H @…`
// names nothing after the flag, so it matches nothing.
var credentialHeader = regexp.MustCompile(`(?i)(?:^|[^\w-])(?:-H|--header)(?:=|\s*)\\?["']?(?:(?:authorization:\s*(?:bearer|token|basic)\s+|[\w-]*(?:api-key|-token)\s*:\s*)(?:\\?["'])*\\?[$<]|\\?\$\{?\w*(?:auth|token|bearer|credential))`)

// credentialFlag finds the flags whose next word is a user's password (`-u`, `--user`), a
// request's body (`-d`, the `--data` family, wget's `--post-data`, `-F`, `--form`) or a token
// itself (`--token`, kubectl's and gh's, and curl's `--oauth2-bearer`). It is written
// case-sensitive, since `-D` and `-U` are other flags; `--token-file` and `--github-token` are
// other flags too, and match nothing.
var credentialFlag = regexp.MustCompile(`(?:^|[^\w-])(-u|--user|-d|--data|--data-binary|--data-raw|--data-urlencode|--data-ascii|--post-data|-F|--form|--token|--oauth2-bearer)(?:=|[ \t]+)`)

// passwordWord matches a `-u` word whose password is a variable, a token file read into it or a
// placeholder (`"$user:$token"`, `"ada:$(cat …)"`, `"ada:<password>"`), or a variable named for a
// credential (`"$credential"`). A `--user "$(id -u):$(id -g)"` of docker's names no password, and
// `env -u "$name"` unsets a variable, so neither matches.
var passwordWord = regexp.MustCompile(`(?i):\\?["']?(?:\\?\$\{?\w|\\?\$\(\s*(?:cat\s|<)|<)|\$\{?\w*(?:token|credentials?)\b`)

// tokenWord matches a body word that carries a token: a form field `token=$…` or `token=<…>`, a
// JSON field `"…token": "$…"`, a variable named for one (`$agent_token`, but not
// `$app_tokens_dir`), or a token file read into it.
var tokenWord = regexp.MustCompile(`(?i)token\\?["']?\s*[=:]\s*\\?["']?\\?[$<]|\$\{?\w*token\b|\$\(\s*(?:cat\s|<)[^)]*token`)

// tokenValue matches a `--token` or `--oauth2-bearer` value that is one variable, one command
// substitution or one placeholder, quoted or not: `"$token"`, `$(cat "$file")`, `<bearer>`. A
// value that joins several, dev-oidc-issuer's `--token "$audience=$account=$file"`, names a file
// and no token, and a fixed fixture value (`--token fixture-token`) is no secret, so neither
// matches.
var tokenValue = regexp.MustCompile(`^\\?["']?(?:\\?\$\{?!?\w+\}?|\\?\$\([^)]*\)|<[^>]+>)\\?["']?(?:[\s;|&)]|$)`)

// shellWord is the shell word s starts with: up to the first blank, `|`, `;`, `&`, `<`, `>` or `)`
// that stands outside quotes and outside a `$(…)`, so a quoted body with blanks in it, or with a
// `$(cat "…")` inside its quotes, is one word.
func shellWord(s string) string {
	var open []byte // each '\'', '"' or '(' this word is inside, innermost last
	for i := 0; i < len(s); i++ {
		c := s[i]
		inner := byte(0)
		if len(open) > 0 {
			inner = open[len(open)-1]
		}
		switch inner {
		case '\'':
			if c == '\'' {
				open = open[:len(open)-1]
			}
			continue
		case '"':
			switch {
			case c == '\\':
				i++
			case c == '"':
				open = open[:len(open)-1]
			case c == '$' && i+1 < len(s) && s[i+1] == '(':
				open = append(open, '(')
				i++
			}
			continue
		}
		switch {
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			open = append(open, c)
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			open = append(open, '(')
			i++
		case c == '(' && inner == '(':
			open = append(open, '(')
		case c == ')' && inner == '(':
			open = open[:len(open)-1]
		case inner == 0 && strings.IndexByte(" \t|;&<>)", c) >= 0:
			return s[:i]
		}
	}
	return s
}

// credentialArgument says whether a line gives a command a credential as an argument.
func credentialArgument(line string) bool {
	if credentialHeader.MatchString(line) {
		return true
	}
	for _, at := range credentialFlag.FindAllStringSubmatchIndex(line, -1) {
		word := shellWord(line[at[1]:])
		switch flag := line[at[2]:at[3]]; flag {
		case "-u", "--user":
			if passwordWord.MatchString(word) {
				return true
			}
		case "--token", "--oauth2-bearer":
			if tokenValue.MatchString(line[at[1]:]) {
				return true
			}
		default:
			if tokenWord.MatchString(word) {
				return true
			}
		}
	}
	return false
}

// credentialCheckSelf is this file, which writes each spelling the matcher must catch.
const credentialCheckSelf = "packages/daemon/internal/textcheck/credential_argument_test.go"

// credentialTextFiles lists the repository's text files the check reads: every file git tracks,
// and every untracked one it does not ignore, so a new file is read before it is added, but
// docs/plans, docs/superpowers and this file.
func credentialTextFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("list the repository's files with git ls-files: %v", err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || rel == credentialCheckSelf ||
			strings.HasPrefix(rel, "docs/plans/") || strings.HasPrefix(rel, "docs/superpowers/") {
			continue
		}
		files = append(files, rel)
	}
	return files
}

func TestNoTextGivesACredentialAsACommandArgument(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	var offences []string
	read := map[string]int{}
	for _, rel := range credentialTextFiles(t, root) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted in the working tree, not yet in the index
		}
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		// A NUL in the first 8000 bytes is git's own test for a binary file.
		if bytes.IndexByte(body[:min(len(body), 8000)], 0) >= 0 {
			continue
		}
		top, _, _ := strings.Cut(rel, "/")
		read[top]++
		for number, line := range strings.Split(string(body), "\n") {
			if credentialArgument(line) {
				offences = append(offences, rel+":"+strconv.Itoa(number+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	// A tree this check read nothing of is a check that saw nothing, never a pass.
	for _, top := range []string{".github", "docs", "packages", "scripts", "skills"} {
		if read[top] == 0 {
			t.Fatalf("read no text file under %s, so this check saw none of it", top)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("%d lines give a command a credential as an argument, where every process of the pod reads it:\n\t%s\n"+
			"send it on standard input or from a file instead: printf 'Authorization: Bearer %%s' \"$token\" | curl -H @- …, "+
			"--data @-, or a curl config on standard input (-K -)",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// The matcher is what the check above can be blind through, so it is held to each spelling it
// must catch and to each form the repository writes in its place.
func TestTheCredentialMatcherCatchesEachSpellingAndPassesAnInputCurlReads(t *testing.T) {
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
		`curl -fsS -u "$GITHUB_ACTOR:$GITHUB_TOKEN" "$url"`,
		`[ -z "$credential" ] || authentication=(-u "$credential")`,
		`curl --user="ada:<password>" "$url"`,
		`curl -u "ada:$(cat "$PASSWORD_FILE")" "$url"`,
		`curl -s -H @- -d "token=<access token>" "$AGENT_SECRETS_URL/v1/oauth/introspect"`,
		`curl --data-urlencode "token=$TOKEN" "$url"`,
		`curl -X POST -d "{\"pod_token\": \"$(cat "$DEV_BROKER_DIR/pod-token")\", \"session\": \"s\"}" "$url"`,
		`curl --data "{\"token\":\"$t\"}" "$url"`,
		`curl -d "$(jq -nc --arg t "$TOKEN" '{token: $t}')" "$url"`,
		`curl -F "token=${upload_token}" "$url"`,
		`--data-urlencode "printed=$(field printed)" --data-urlencode "token=$(field token)" \`,
		`  -d "{\"pod_token\": \"$(cat "$POD_TOKEN_FILE")\", \"session\": \"$EXCHANGE_SESSION\"}" >/dev/null; then`,
		`kubectl config set-credentials legion-daemon --kubeconfig "$kubeconfig" --token "$token"`,
		`gh auth login --token=$GH_TOKEN`,
		`kubectl --token "$(cat "$token_file")" get pods`,
		`kubectl --token ${!name} get pods`,
		`  --token "$model_token" --refusal "${workdir}/model-refusal" --status "$model_status" \`,
		`run_search.py --url http://127.0.0.1:8799 --token <bearer> --out out.jsonl`,
		`curl -H "Authorization: Basic $(printf '%s:%s' "$user" "$password" | base64)" "$url"`,
		`curl --oauth2-bearer "$TOKEN" "$url"`,
		`curl --oauth2-bearer=$(cat "$token_file") "$url"`,
		`curl -H "X-Api-Key: $ANTHROPIC_API_KEY" "$url"`,
		`curl -H "x-goog-api-key: ${key}" "$url"`,
		`curl -H "X-Auth-Token: <token>" "$url"`,
		`curl -sf -m 5 -H "X-aws-ec2-metadata-token: $imds" http://169.254.169.254/latest/meta-data/local-ipv4`,
		`curl -H "$auth_header" "$url"`,
		`curl --header "${AUTHORIZATION}" "$url"`,
	} {
		if !credentialArgument(line) {
			t.Errorf("does not catch a credential given as an argument: %s", line)
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
		`printf 'token=%s' "$TOKEN" | curl -s -H @<(printf 'Authorization: Bearer %s' "$(cat "$F")") --data @- "$url"`,
		`printf 'user = "%s"\n' "$credential" | curl -K - "$url"`,
		`curl -fsS -u ada "$url"`,
		`work=$(mktemp -d "$work/token.XXXXXX")`,
		`base64 -d <<<"$token"`,
		`curl -d '{"approver":"ada@example.com"}' "$url"`,
		`git push -u origin "$branch"`,
		`docker run --rm --user "$(id -u):$(id -g)" "$image"`,
		`[ -n "$app_tokens_dir" ] && [ -d "$app_tokens_dir" ] ||`,
		`app_key_unsets+=(-u "$implement_app_secret")`,
		`printf '%s' "$(field token)" | curl -sS --data-urlencode token@- "$url"`,
		`  --token "$BROKER_AUDIENCE=$API_ACCOUNT=$ACCOUNT_TOKEN_FILE" \`,
		`dev-airlock-stands enroll-pod --url "$URL" --token-file "$REGISTRAR_TOKEN_FILE"`,
		`agent-secrets "$GITHUB_SECRET" --github-owner "$owner" --github-token -- sh -c 'exit 0'`,
		`python3 run_search.py --url http://127.0.0.1:8799 --token fixture-token --workers 8`,
		`kubectl config set users.legion-daemon.tokenFile "$token_file"`,
		`human -X POST "$url/api/v1/broadcasts" -H "Idempotency-Key: broadcast-$run"`,
		`say "xkey=\$(curl -sS -o /dev/null -w %{http_code} -H "X-Key: \$P" $AIRLOCK_S_URL/read)"`,
		`curl -H "$content_type" "$url"`,
		`curl -H @"$auth_header_file" "$url"`,
		`printf 'X-Api-Key: %s' "$KEY" | curl -H @- "$url"`,
		`curl --oauth2-bearer fixture-token "$url"`,
	} {
		if credentialArgument(line) {
			t.Errorf("catches a line that gives no credential as an argument: %s", line)
		}
	}
}
