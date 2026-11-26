package ghtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// ErrNotAWrite is Lookup's answer for a recorded call that posts no comment or review: there is
// nothing on GitHub to find it by, so its outcome stays unknown.
var ErrNotAWrite = errors.New("posts no comment or review")

// Found is what Lookup found on GitHub: whether a comment or review carrying the call's answer key
// exists, and, when one does, its URL and GitHub id.
type Found struct {
	Exists bool
	URL    string
	ID     int64
}

// Lookup tells a restarted answer what became of its call seq, recorded with no result because the
// daemon stopped while it ran (A18): for a call posting a comment or review, it reads the comments
// or reviews of the pull request or issue the call wrote to, with the routed owner's credentials
// as the call ran with them, and answers the one whose Legion footer carries the call's answer key
// (<question>.<seq>), or none. A call the record does not hold, one that has a result, and one
// whose target cannot be read are errors, a read gh answered with more than readLimit characters
// among them; a call posting no comment or review is ErrNotAWrite.
func Lookup(ctx context.Context, a *Answer, seq int, bounds config.AnswerBounds) (Found, error) {
	recorded, ok, err := a.host.Calls.AnswerToolCall(ctx, a.question, seq)
	switch {
	case err != nil:
		return Found{}, err
	case !ok:
		return Found{}, fmt.Errorf("gh call %s.%d is not recorded", a.question, seq)
	case recorded.Result != nil:
		return Found{}, fmt.Errorf("gh call %s.%d has its result recorded; its outcome is known", a.question, seq)
	}
	if bounds.GHCall <= 0 {
		return Found{}, errors.New("the gh tool has no time bound (answers.bounds.gh_seconds), so it runs nothing")
	}
	ctx, cancel := context.WithTimeout(ctx, bounds.GHCall)
	defer cancel()
	limit := readLimit(bounds)
	place, err := a.placeOf(ctx, recorded.Args, limit)
	if err != nil {
		return Found{}, fmt.Errorf("gh call %s.%d: %w", a.question, seq, err)
	}
	argv := []string{"api", place.endpoint}
	if place.list {
		argv = append(argv, "--paginate", "--slurp")
	}
	out, err := a.read(ctx, lookupLine, argv, limit)
	if err != nil {
		return Found{}, fmt.Errorf("gh call %s.%d: read %s: %w", a.question, seq, place.endpoint, err)
	}
	items, err := decodeItems(out, place.list)
	if err != nil {
		return Found{}, fmt.Errorf("gh call %s.%d: read %s: %w", a.question, seq, place.endpoint, err)
	}
	key := a.question + "." + strconv.Itoa(seq)
	for _, item := range items {
		if answerKeys(item.Body)[key] {
			return Found{Exists: true, URL: item.HTMLURL, ID: item.ID}, nil
		}
	}
	return Found{}, nil
}

// place is where a call's comment or review would be on GitHub: a REST endpoint listing them
// (list) or naming the one the call edited or submitted.
type place struct {
	endpoint string
	list     bool
}

// apiPlaces are the REST endpoints a gh api comment or review write posts to, each with the method
// that posts, and where what it posted is read: $1 is the endpoint's repository path and number.
var apiPlaces = []struct {
	method string
	path   *regexp.Regexp
	read   string
	list   bool
}{
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/issues/[^/]+/comments)$`), "$1", true},
	{"PATCH", regexp.MustCompile(`^(repos/[^/]+/[^/]+/issues/comments/[^/]+)$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/reviews)$`), "$1", true},
	{"PUT", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/reviews/[^/]+)$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/reviews/[^/]+)/events$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/comments)(/[^/]+/replies)?$`), "$1", true},
	{"PATCH", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/comments/[^/]+)$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/commits/[^/]+/comments)$`), "$1", true},
	{"PATCH", regexp.MustCompile(`^(repos/[^/]+/[^/]+/comments/[^/]+)$`), "$1", false},
}

// placeOf is where the call argv, as recorded, would have posted: for pr comment, issue comment
// and a closing or reopening comment, the issue comments of its pull request or issue; for pr
// review, the pull request's reviews; for a gh api write, the endpoint it posted to read back. A
// pr command naming its pull request by branch has its number read from GitHub.
func (a *Answer) placeOf(ctx context.Context, argv []string, limit int) (place, error) {
	words := commandWords(argv)
	key := writeOf(words)
	spec, ok := writes[key]
	if !ok {
		return place{}, ErrNotAWrite
	}
	parsed, err := parseArgs(argv, spec, key)
	if err != nil {
		return place{}, err
	}
	if key == "api" {
		if len(parsed.positionals) < 2 {
			return place{}, errors.New("its gh api call names no endpoint")
		}
		endpoint := apiPath(parsed.positionals[1])
		method := apiMethod(parsed)
		for _, p := range apiPlaces {
			if m := p.path.FindStringSubmatchIndex(endpoint); m != nil && p.method == method {
				return place{endpoint: string(p.path.ExpandString(nil, p.read, endpoint, m)), list: p.list}, nil
			}
		}
		return place{}, ErrNotAWrite
	}
	if (key == "pr close" || key == "pr reopen") && len(parsed.given("comment")) == 0 {
		return place{}, ErrNotAWrite
	}
	repo, number, err := a.target(ctx, parsed, words[0] == "pr", limit)
	if err != nil {
		return place{}, err
	}
	if key == "pr review" {
		return place{endpoint: "repos/" + repo + "/pulls/" + number + "/reviews", list: true}, nil
	}
	return place{endpoint: "repos/" + repo + "/issues/" + number + "/comments", list: true}, nil
}

// githubIssueURL is a pull request's or an issue's github.com URL, as gh reads one in a number's
// place: its repository and number.
var githubIssueURL = regexp.MustCompile(`(?i)^https?://(?:www\.)?github\.com/([^/]+/[^/]+)/(?:pull|issues)/([0-9]+)`)

// target is the repository (OWNER/REPO) and number a pr or issue command's arguments name, as
// namedTarget reads them, a pr command's branch read from GitHub with `gh pr view`.
func (a *Answer) target(ctx context.Context, parsed parsedArgs, pr bool, limit int) (string, string, error) {
	repo, number, selector, err := namedTarget(parsed)
	if err != nil || number != "" {
		return repo, number, err
	}
	if !pr || selector == "" {
		return "", "", fmt.Errorf("its selector %q names no pull request or issue the gh tool can read", selector)
	}
	out, err := a.read(ctx, lookupLine, []string{"pr", "view", selector, "-R", repo, "--json", "number", "--jq", ".number"}, limit)
	if err != nil {
		return "", "", fmt.Errorf("read the pull request of %q in %s: %w", selector, repo, err)
	}
	number = strings.TrimSpace(string(out))
	if number == "" || strings.Trim(number, "0123456789") != "" {
		return "", "", fmt.Errorf("gh named %q as the pull request of %q in %s", number, selector, repo)
	}
	return repo, number, nil
}

// namedTarget is the repository (OWNER/REPO) and number a pr or issue command's arguments name,
// and its selector: the selector a number, #N or a URL, the repository the URL's or -R's. A
// selector that is none of those — a branch, or none at all — names its repository and no
// number. A command naming no repository runs in the answer's empty working directory, where gh
// resolves none, so its target cannot be read.
func namedTarget(parsed parsedArgs) (repo, number, selector string, err error) {
	if len(parsed.positionals) > 2 {
		selector = parsed.positionals[2]
	}
	if given := parsed.given("repo"); len(given) > 0 {
		value := given[len(given)-1].value
		for _, prefix := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
			if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
				value = value[len(prefix):]
				break
			}
		}
		named, err := ghrepo.Parse("its -R", strings.TrimSuffix(value, "/"))
		if err != nil {
			return "", "", "", err
		}
		repo = named.String()
	}
	if m := githubIssueURL.FindStringSubmatch(selector); m != nil {
		named, err := ghrepo.Parse("its URL's repository", m[1])
		if err != nil {
			return "", "", "", err
		}
		return named.String(), m[2], selector, nil
	}
	if repo == "" {
		return "", "", "", errors.New("it names no repository, and gh resolves none from the answer's empty working directory")
	}
	if n := strings.TrimPrefix(selector, "#"); n != "" && strings.Trim(n, "0123456789") == "" {
		return repo, n, selector, nil
	}
	return repo, "", selector, nil
}

// The messages of read's log line: lookupLine for a restart's lookup (Lookup, Answer.target), and
// headReadLine for the head a review is checked against (Answer.headRefusal).
const (
	lookupLine   = "answers: gh lookup"
	headReadLine = "answers: gh head read"
)

// readLimit is how many characters of gh's stdout a read of the daemon's own takes (Answer.read):
// gh_output_chars for each of gh_calls, the most gh output one answer hands its model over all its
// calls, and the largest int where that product is larger. A restart's lookup reads every comment
// or review of the pull request or issue its call wrote to, which anyone can add to on a public
// repository, so the read is bounded; it is not bounded at one call's gh_output_chars, which a
// pull request's comments, as the REST API answers them, pass early: 32 answered 211,415 bytes.
func readLimit(bounds config.AnswerBounds) int {
	if bounds.GHCalls > 0 && bounds.GHOutputChars > math.MaxInt/bounds.GHCalls {
		return math.MaxInt
	}
	return bounds.GHOutputChars * bounds.GHCalls
}

// read runs a read of the daemon's own, argv, as a call runs (routed, the scrubbed environment,
// the answer's empty working directory), and answers its whole stdout; a gh exiting non-zero is an
// error naming its stderr, and a stdout of more than limit characters (readLimit) an error naming
// the bound, of which no more than limit characters were kept. It records nothing: it is no call
// of the model's, but a restart's lookup or the head a review is checked against
// (Answer.headRefusal). It logs one line, line its message.
func (a *Answer) read(ctx context.Context, line string, argv []string, limit int) ([]byte, error) {
	_, owner, dir, err := a.route(ctx, argv)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, a.host.GH, argv...)
	cmd.Env, cmd.Dir = a.env(dir), a.cwd
	procgroup.Configure(cmd)
	stdout := NewCappedOutput(limit)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err = cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	a.host.Log.Info(line, "question", a.question, "issue", a.agent.Issue, "role", a.agent.Role,
		"app", string(a.agent.App()), "appID", a.agent.AppID, "owner", owner, "args", argv, "exit", exit)
	if err != nil {
		return nil, fmt.Errorf("gh %s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	if stdout.chars > limit {
		return nil, fmt.Errorf("gh %s printed %d characters, past the %d a read of the daemon's own takes (answers.bounds.gh_output_chars times answers.bounds.gh_calls)",
			strings.Join(argv, " "), stdout.chars, limit)
	}
	return stdout.kept, nil
}

// item is a GitHub comment or review as the REST API answers it.
type item struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

// decodeItems reads a REST answer: for a list, gh api --paginate --slurp's array of pages, each an
// array of items; otherwise one item.
func decodeItems(out []byte, list bool) ([]item, error) {
	if !list {
		var one item
		if err := json.Unmarshal(out, &one); err != nil {
			return nil, fmt.Errorf("its answer is no comment or review: %w", err)
		}
		return []item{one}, nil
	}
	var pages [][]item
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("its answer is no list of comments or reviews: %w", err)
	}
	var items []item
	for _, page := range pages {
		items = append(items, page...)
	}
	return items, nil
}

// answerKeys are the answer keys of every Legion footer in body.
func answerKeys(body string) map[string]bool {
	keys := map[string]bool{}
	for rest := body; ; {
		at := strings.Index(rest, footerMarker)
		if at < 0 {
			return keys
		}
		rest = rest[at+len(footerMarker):]
		end := strings.Index(rest, "-->")
		if end < 0 {
			return keys
		}
		var footer struct {
			Answer string `json:"answer"`
		}
		if json.Unmarshal([]byte(rest[:end]), &footer) == nil && footer.Answer != "" {
			keys[footer.Answer] = true
		}
		rest = rest[end:]
	}
}
