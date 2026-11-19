package textcheck

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A credential given to curl as an argument - a bearer in `-H "Authorization: Bearer $(cat …)"`, a
// password in `-u "$user:$token"`, in a proxy URL or in the URL itself, a token in a body
// `-d "token=$token"` - is in that curl's command line, which every process of the pod or machine
// can read in /proc for as long as the call runs, and a role copies the commands its texts give it.
// So every command this repository writes or gives an agent or an operator sends the credential on
// standard input or from a file instead: `printf 'Authorization: Bearer %s' "$token" | curl -H @- …`
// (printf is a shell builtin, which starts no process), `-H @<(printf …)`, `--data @-`, or a curl
// config on standard input or a `<(printf …)` path (`-K -`, `-K <(…)`). This check holds every text
// file of the repository to that, but docs/plans and docs/superpowers, which are dated history.

// credentialHeader matches a header argument that carries a credential from a variable, a command
// substitution or a placeholder for one, in each spelling a text of this repository writes:
// `-H "Authorization: Bearer $…`, `--header='Authorization: Bearer '"$…`, `-H "Authorization:
// Bearer \$…` inside a heredoc that writes a script, and `-H "Authorization: Bearer <token>"`, and
// the same to a proxy (`--proxy-header "Proxy-Authorization: Basic $…"`). The credential is a
// bearer, a GitHub `token` or a `Basic` password, or the value of a header named for a key or a
// token (`X-Api-Key:`, `x-goog-api-key:`, `X-Auth-Token:`, IMDSv2's `X-aws-ec2-metadata-token:`);
// `X-Key:` and `Idempotency-Key:` name no credential. A whole header in a variable named for one
// (`-H "$auth_header"`, `--header "$PLACEHOLDER"`, `--header "$api_key"`) matches too. A header curl
// reads with `-H @…` names nothing after the flag, so it matches nothing.
var credentialHeader = regexp.MustCompile(`(?i)(?:^|[^\w-])(?:-H|--header|--proxy-header)(?:=|\s*)\\?["']?(?:(?:(?:proxy-)?authorization:\s*(?:bearer|token|basic)\s+|[\w-]*(?:api-key|-token)\s*:\s*)(?:\\?["'])*\\?[$<]|\\?\$\{?\w*(?:auth|token|bearer|credential|placeholder|password|passwd|secret|key))`)

// credentialFlag finds the flags whose next word can carry a credential, as curl's manual (curl
// 8.5) names them: a user's password (`-u`, `--user`, and curl's `-U` and `--proxy-user` for the
// proxy's), a request's body or query (`-d`, the `--data` family, `--json`, wget's `--post-data`,
// `-F`, `--form`, `--form-string`, `--url-query`, and a `--variable` curl expands into one), a
// token itself (`--token`, kubectl's and gh's, and curl's `--oauth2-bearer`), one of curl's other
// passwords (`--pass`, `--proxy-pass`, `--tlspassword`, `--proxy-tlspassword`, `--ftp-account`,
// and the `:password` of a client certificate given to `-E`, `--cert` or `--proxy-cert`), a
// session's cookie (`-b`, `--cookie`), a proxy URL with its user's password in it (`-x`,
// `--proxy`, `--preproxy`), or a whole header (`-H`, `--header`, `--proxy-header`). It is written
// case-sensitive, since `-D`, `-X`, `-B` and `-e` are other flags; `--token-file`,
// `--github-token`, `--proxy-file` and `--header-file` are other flags too, and match nothing.
var credentialFlag = regexp.MustCompile(`(?:^|[^\w-])(-u|--user|-U|--proxy-user|-d|--data|--data-binary|--data-raw|--data-urlencode|--data-ascii|--json|--post-data|-F|--form|--form-string|--url-query|--variable|--token|--oauth2-bearer|--pass|--proxy-pass|--tlspassword|--proxy-tlspassword|--ftp-account|-E|--cert|--proxy-cert|-b|--cookie|-x|--proxy|--preproxy|-H|--header|--proxy-header)(?:=|[ \t]+)`)

// curlCommand matches a line that runs curl, the one command whose `-E` is a client certificate
// and whose `-b` is a cookie; grep's, sed's and sudo's `-E` and git's `-b` are other flags.
var curlCommand = regexp.MustCompile(`(?:^|[^\w-])curl(?:\s|$)`)

// passwordWord matches a `-u` word whose password is a variable, a token file read into it or a
// placeholder (`"$user:$token"`, `"ada:$(cat …)"`, `"ada:<password>"`), or a variable named for a
// credential (`"$credential"`); curl's `-U` and a certificate's `:password` are read the same way. A
// `--user "$(id -u):$(id -g)"` of docker's names no password, and `env -u "$name"` unsets a
// variable, so neither matches.
var passwordWord = regexp.MustCompile(`(?i):\\?["']?(?:\\?\$\{?\w|\\?\$\(\s*(?:cat\s|<)|<)|\$\{?\w*(?:token|credentials?)\b`)

// tokenWord matches a body word that carries a token: a form field `token=$…` or `token=<…>`, a
// JSON field `"…token": "$…"`, a variable named for one (`$agent_token`, but not
// `$app_tokens_dir`), or a token file read into it.
var tokenWord = regexp.MustCompile(`(?i)token\\?["']?\s*[=:]\s*\\?["']?\\?[$<]|\$\{?\w*token\b|\$\(\s*(?:cat\s|<)[^)]*token`)

// cookieWord matches a cookie word that carries a credential: a pair whose name is a session's,
// an auth's or a token's (`session=$…`, `auth_token=<…>`) and whose value is a variable, a command
// substitution or a placeholder. A cookie jar's file (`-b "$jar"`) names none.
var cookieWord = regexp.MustCompile(`(?i)(?:^|[\s;"'])[\w.-]*(?:session|sid|auth|token|secret|jwt)[\w.-]*\s*=\s*\\?["']?\\?[$<]`)

// tokenValue matches a value that is one variable, one command substitution or one placeholder,
// quoted or not: `"$token"`, `$(cat "$file")`, `<bearer>`, given to `--token`, `--oauth2-bearer` or
// one of curl's other password flags. A value that joins several, dev-oidc-issuer's `--token
// "$audience=$account=$file"`, names a file and no token, and a fixed fixture value (`--token
// fixture-token`) is no secret, so neither matches.
var tokenValue = regexp.MustCompile(`^\\?["']?(?:\\?\$\{?!?\w+\}?|\\?\$\([^)]*\)|<[^>]+>)\\?["']?(?:[\s;|&)]|$)`)

// urlUserinfo matches a URL whose user's password is a variable, a command substitution or a
// placeholder: a proxy's (`http://agent-secrets:$PROXY_PASSWORD@host`, `http://ada:<password>@proxy`)
// and, on a line that runs curl, the URL curl is asked for (`https://ada:$PASSWORD@host/`). A URL
// with no user part (`http://$host:$port`) matches nothing.
var urlUserinfo = regexp.MustCompile(`://[^\s/@"']*:(?:\\?\$\{?\w+\}?|\\?\$\([^)]*\)|<[^>]*>)[^\s/@"']*@`)

// A script can also hold a credential in a variable or a function whose name says nothing of it:
// `proxy="$(proxy_for "$addr")"`, then `--proxy "$proxy"`. heldCredentials reads where each one came
// from, so a whole word that expands one of them is read for what it carries.

// shellAssignment matches a shell assignment, with `local`, `export`, `readonly` or `declare` before
// it or not: the name, and the value.
var shellAssignment = regexp.MustCompile(`^\s*(?:(?:local|export|readonly|declare(?:\s+-\w+)*)\s+)?([A-Za-z_]\w*)=(.*)$`)

// shellFunction matches the line a shell function starts on: its name, and what follows its brace.
var shellFunction = regexp.MustCompile(`^\s*(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\)\s*\{(.*)$`)

// credentialSource matches a value read from where a credential is kept: a file named for one, read
// by a command substitution (`"$(cat "$TOKEN_FILE")"`, `"$(< "$dir/password")"`), or a JSON field
// named for one, or a released value, read with jq (`jq -r '.airlock.proxy_password'`,
// `jq -r '.values.GITHUB_TOKEN'`).
var credentialSource = regexp.MustCompile(`(?i)\$\(\s*(?:cat\s|<)[^)]*(?:token|password|passwd|secret|credential|placeholder)|\bjq\b.*\.(?:\w*(?:token|password|passwd|secret|credential|placeholder)|values?\b)`)

// credentialPrint matches a function body line that prints a credential: a URL with an argument as
// its user's password (`printf 'http://agent-secrets:%s@%s' "$PROXY_PASSWORD" "$1"`), or a file
// named for a credential.
var credentialPrint = regexp.MustCompile(`(?i)\bprintf\s+(?:'[^']*|"[^"]*)://[^\s/@"']*:%s@|(?:^|[\s;{|&])cat\s+[^|;&]*(?:token|password|passwd|secret|credential)`)

// substitutionOf matches a word that is one command substitution: the command's name. expansionOf
// matches a word that is one variable's value: the variable's name.
var (
	substitutionOf = regexp.MustCompile(`^\\?["']?\\?\$\(\s*([\w-]+)`)
	expansionOf    = regexp.MustCompile(`^\\?["']?\\?\$\{?(\w+)\}?\\?["']?$`)
)

// held is the set of names, variables and functions, that a script holds a credential in.
type held map[string]bool

// heldCredentials reads lines, one script, for the names it holds a credential in: each function
// with a body line that prints one, each variable assigned from a credential's source, and each
// variable assigned from one of those, by a substitution of the function or the variable's own
// value. A function written on one line ends on it; any other runs to the first line that is only
// its closing brace.
func heldCredentials(lines []string) held {
	h := held{}
	function := ""
	for _, line := range lines {
		if m := shellFunction.FindStringSubmatch(line); m != nil {
			if credentialPrint.MatchString(m[2]) {
				h[m[1]] = true
			}
			function = m[1]
			if strings.HasSuffix(strings.TrimSpace(m[2]), "}") {
				function = ""
			}
			continue
		}
		if function == "" {
			continue
		}
		if strings.TrimRight(strings.TrimSpace(line), ";") == "}" {
			function = ""
		} else if credentialPrint.MatchString(line) {
			h[function] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, line := range lines {
			m := shellAssignment.FindStringSubmatch(line)
			if m == nil || h[m[1]] {
				continue
			}
			value := shellWord(m[2])
			if credentialSource.MatchString(value) || h.expands(value) {
				h[m[1]] = true
				grew = true
			}
		}
	}
	return h
}

// expands says whether word is one expansion of a name h holds: `"$proxy"`, `"$(proxy_for …)"`.
func (h held) expands(word string) bool {
	if m := expansionOf.FindStringSubmatch(word); m != nil && h[m[1]] {
		return true
	}
	m := substitutionOf.FindStringSubmatch(word)
	return m != nil && h[m[1]]
}

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

// credentialArgument says whether a line gives a command a credential as an argument, h naming
// what its script holds a credential in (nil for a line read alone).
func credentialArgument(line string, h held) bool {
	if credentialHeader.MatchString(line) {
		return true
	}
	curl := curlCommand.MatchString(line)
	if curl && urlUserinfo.MatchString(line) {
		return true
	}
	for _, at := range credentialFlag.FindAllStringSubmatchIndex(line, -1) {
		word := shellWord(line[at[1]:])
		var carries bool
		switch flag := line[at[2]:at[3]]; flag {
		case "-u", "--user", "-U", "--proxy-user":
			carries = passwordWord.MatchString(word) || h.expands(word)
		case "-E":
			carries = curl && passwordWord.MatchString(word)
		case "--cert", "--proxy-cert":
			carries = passwordWord.MatchString(word)
		case "--token", "--oauth2-bearer", "--pass", "--proxy-pass", "--tlspassword", "--proxy-tlspassword", "--ftp-account":
			carries = tokenValue.MatchString(line[at[1]:])
		case "-b":
			carries = curl && (cookieWord.MatchString(word) || tokenWord.MatchString(word) || h.expands(word))
		case "--cookie":
			carries = cookieWord.MatchString(word) || tokenWord.MatchString(word) || h.expands(word)
		case "-x", "--proxy", "--preproxy":
			carries = urlUserinfo.MatchString(word) || h.expands(word)
		case "-H", "--header", "--proxy-header":
			carries = h.expands(word)
		default:
			carries = tokenWord.MatchString(word) || h.expands(word)
		}
		if carries {
			return true
		}
	}
	return false
}

// credentialLines answers the 1-based numbers of the lines of body, one file, that give a command
// a credential as an argument.
func credentialLines(body string) []int {
	lines := strings.Split(body, "\n")
	h := heldCredentials(lines)
	var found []int
	for i, line := range lines {
		if credentialArgument(line, h) {
			found = append(found, i+1)
		}
	}
	return found
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
		lines := strings.Split(string(body), "\n")
		for _, number := range credentialLines(string(body)) {
			offences = append(offences, rel+":"+strconv.Itoa(number)+": "+strings.TrimSpace(lines[number-1]))
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
			"--data @-, or a curl config on standard input or a <(printf …) path (-K -)",
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
		// A whole header in a variable named for a credential, as k9 of airlock-check.sh passed the
		// grant's placeholder to dev-airlock-stands load.
		`		--header "$PLACEHOLDER" --rate 50 --seconds 600 --out "$WORK_DIR/k9/load.json" \`,
		`		--ca "$AIRLOCK_CA_CERT_FILE" --header "$PLACEHOLDER" --gap 90 \`,
		`curl -H "$api_key" "$url"`,
		`curl -H "${GITHUB_SECRET}" "$url"`,
		`curl --header "$password_header" "$url"`,
		// The proxy's credential: curl's -U and --proxy-user, and a proxy URL with its user's
		// password in it, which k9 gave dev-airlock-stands load.
		`curl -U "agent-secrets:$PROXY_PASSWORD" -x http://127.0.0.1:3128 "$url"`,
		`curl --proxy-user="ada:<password>" -x "$proxy_addr" "$url"`,
		`curl -U "$proxy_credential" "$url"`,
		`"$STANDS_BIN" load gap --proxy "http://agent-secrets:$PROXY_PASSWORD@${BROKER_PROXY_ADDRS[-1]}" \`,
		`curl --proxy "http://agent-secrets:$PROXY_PASSWORD@$addr" "$url"`,
		`curl -x "http://ada:<password>@proxy.example:3128" "$url"`,
		`curl --preproxy "socks5://ada:${SOCKS_PASSWORD}@127.0.0.1:1080" -x "http://$addr" "$url"`,
		`curl --proxy-header "Proxy-Authorization: Basic $(printf '%s:%s' "$user" "$pass" | base64)" -x "http://$addr" "$url"`,
		`curl -H "Proxy-Authorization: Basic $credential" -x "http://$addr" "$url"`,
		// curl's other passwords: a client certificate's, a private key's passphrase and TLS-SRP's,
		// each for the server and for an HTTPS proxy.
		`curl -E "$CLIENT_CERT:$CLIENT_CERT_PASSWORD" "$url"`,
		`curl --cert "client.pem:<password>" "$url"`,
		`curl --proxy-cert "$PROXY_CERT:$(cat "$PROXY_CERT_PASSWORD_FILE")" -x "https://$addr" "$url"`,
		`curl --pass "$KEY_PASSPHRASE" --key client.key "$url"`,
		`curl --proxy-pass "$(cat "$proxy_key_passphrase_file")" --proxy-key proxy.key -x "https://$addr" "$url"`,
		`curl --tlsuser ada --tlspassword "$SRP_PASSWORD" "$url"`,
		`curl --proxy-tlsuser ada --proxy-tlspassword <password> -x "https://$addr" "$url"`,
		// The rest of curl's manual: a body or query in JSON, a literal form field, a query pair and
		// a variable curl expands, FTP's account data, a session's cookie, and the user's password in
		// the URL curl is asked for.
		`curl --json "{\"token\": \"$TOKEN\"}" "$url"`,
		`curl --form-string "token=$UPLOAD_TOKEN" "$url"`,
		`curl --url-query "access_token=$ACCESS_TOKEN" "$url"`,
		`curl --variable "token=$TOKEN" --expand-header "Authorization: Bearer {{token}}" "$url"`,
		`curl --ftp-account "$FTP_ACCOUNT" ftp://ftp.example.com/`,
		`curl -b "session=$SESSION_ID" "$url"`,
		`curl --cookie "auth_token=<token>" "$url"`,
		`curl -fsS "https://ada:$PASSWORD@example.com/api"`,
		`curl "https://x-access-token:$(cat "$TOKEN_FILE")@api.github.com/user"`,
	} {
		if !credentialArgument(line, nil) {
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
		// The proxy's credential in a curl config, on standard input or a <(printf …) path, and the
		// files dev-airlock-stands load reads the proxy and the header from.
		`printf 'proxy-user = "agent-secrets:%s"\n' "$PROXY_PASSWORD" | curl -K - -x "http://$addr" "$url"`,
		`printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS -K <(proxy_config "$addr") -H @- "$url"`,
		`"$STANDS_BIN" load gap --proxy-file <(proxy_for "$addr") --header-file <(printf '%s' "$PLACEHOLDER") --gap 8 \`,
		`curl -x "http://$addr" "$url"`,
		`curl --proxy "http://127.0.0.1:$port" "$url"`,
		`curl --cert "$CLIENT_CERT" --key "$CLIENT_KEY" "$url"`,
		`grep -E "^$name:$value" "$file"`,
		`psql -U "$PGUSER" -h localhost -d legion`,
		`bash -x "$script"`,
		// The same carriers given nothing a credential: a JSON body on standard input, a variable
		// curl imports from its own environment, a cookie jar's file, and URLs with no user.
		`printf '{"token": "%s"}' "$TOKEN" | curl --json @- "$url"`,
		`curl --variable %TOKEN --expand-header "Authorization: Bearer {{TOKEN}}" "$url"`,
		`curl -b "$cookie_jar" -c "$cookie_jar" "$url"`,
		`git checkout -b "session=$name"`,
		`curl -fsS "http://$host:$port/healthz"`,
		`curl "https://example.com/api?user=$user"`,
	} {
		if credentialArgument(line, nil) {
			t.Errorf("catches a line that gives no credential as an argument: %s", line)
		}
	}
}

// A whole word can carry a credential its script read into a name that says nothing of it: a
// variable assigned from a credential's source, or a function that prints one, and a variable
// assigned from either. The matcher reads the file for those names, and each case is a script with
// the lines that must be caught.
func TestTheCredentialMatcherReadsWhereTheScriptReadTheCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script []string
		caught []int
	}{
		{
			// k3, k4 and k9 of airlock-check.sh, as main wrote them.
			name: "the proxy URL the grant's proxy_password is printed into",
			script: []string{
				`	PROXY_PASSWORD="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.proxy_password // ""')"`,
				`proxy_for() { printf 'http://agent-secrets:%s@%s' "$PROXY_PASSWORD" "$1"; }`,
				`	local proxy demo_value`,
				`	proxy="$(proxy_for "${AIRLOCK_PROXY_URL##*://}")"`,
				`	"$STANDS_BIN" load load --proxy "$proxy" --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \`,
				`	"$STANDS_BIN" load gap --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" --host s.airlock.test \`,
				`	short_now="$(printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "${AIRLOCK_PROXY_URL##*://}")" -H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"`,
			},
			caught: []int{5, 6, 7},
		},
		{
			name: "a function of several lines that prints the proxy URL",
			script: []string{
				`proxy_url() {`,
				`	local address=$1`,
				`	printf 'http://agent-secrets:%s@%s' "$(cat "$WORK_DIR/proxy-password")" "$address"`,
				`}`,
				`curl -x "$(proxy_url 127.0.0.1:3128)" "$url"`,
			},
			caught: []int{5},
		},
		{
			name: "a header read from a token file",
			script: []string{
				`header="$(cat "$WORK_DIR/bearer-token")"`,
				`curl -sS --header "$header" "$url"`,
			},
			caught: []int{2},
		},
		{
			name: "a login read out of a release's values, and a copy of it",
			script: []string{
				`login="$(printf '%s' "$RELEASE_JSON" | jq -r '.values.GITHUB_ADMIN_PAT')"`,
				`copy="$login"`,
				`curl -u "$copy" "$url"`,
			},
			caught: []int{3},
		},
		{
			// The forms that carry nothing: a header and a proxy read from where no credential is
			// kept, and the files and curl configs the credential goes in instead.
			name: "no credential in a command line",
			script: []string{
				`header="$(cat "$WORK_DIR/content-type")"`,
				`curl -sS --header "$header" "$url"`,
				`addr="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.proxy_address')"`,
				`proxy_url() { printf 'http://%s' "$1"; }`,
				`proxy="$(proxy_url "$addr")"`,
				`curl --proxy "$proxy" "$url"`,
				`proxy_for() { printf 'http://agent-secrets:%s@%s' "$PROXY_PASSWORD" "$1"; }`,
				`"$STANDS_BIN" load gap --proxy-file <(proxy_for "$addr") --header-file <(printf '%s' "$PLACEHOLDER") --gap 8 \`,
				`printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS -K <(printf 'proxy = "%s"\n' "$(proxy_for "$addr")") -H @- "$url"`,
			},
		},
	} {
		got := credentialLines(strings.Join(tc.script, "\n"))
		if !slices.Equal(got, tc.caught) {
			t.Errorf("%s: caught lines %v, want %v", tc.name, got, tc.caught)
		}
	}
}
