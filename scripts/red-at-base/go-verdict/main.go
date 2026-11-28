// Command go-verdict is scripts/red-at-base.sh's reader of one `go test -json` run, as that
// script's header says: it takes the -run regex and the run's exit status, reads the stream on
// stdin, writes the run's text to stderr and "<VERDICT><tab><what decided it>" to stdout. The
// script builds it from a copy in its own directory under TMPDIR when its list holds a go line.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type event struct {
	Action      string
	Test        string
	Output      string
	FailedBuild string
}

// splitRegexp is testing/match.go's: the regex split at each / and | outside [] and (), into its
// alternatives, each one pattern for each level of a test's name. Each pattern is then rewritten,
// as the testing package's verify does (rewrite, below), before it is compiled.
func splitRegexp(s string) [][]string {
	var alternatives [][]string
	var levels []string
	cs, cp := 0, 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '[':
			cs++
		case ']':
			if cs--; cs < 0 {
				cs = 0
			}
		case '(':
			if cs == 0 {
				cp++
			}
		case ')':
			if cs == 0 {
				cp--
			}
		case '\\':
			i++
		case '/', '|':
			if cs == 0 && cp == 0 {
				levels = append(levels, s[:i])
				if s[i] == '|' {
					alternatives = append(alternatives, levels)
					levels = nil
				}
				s = s[i+1:]
				i = 0
				continue
			}
		}
		i++
	}
	return append(alternatives, append(levels, s))
}

// rewrite is testing/match.go's: each blank becomes _, and an unprintable character its Go escape,
// as the testing package rewrites both a subtest's name and each pattern of -run.
func rewrite(s string) string {
	b := []byte{}
	for _, r := range s {
		switch {
		case isSpace(r):
			b = append(b, '_')
		case !strconv.IsPrint(r):
			s := strconv.QuoteRune(r)
			b = append(b, s[1:len(s)-1]...)
		default:
			b = append(b, string(r)...)
		}
	}
	return string(b)
}

// isSpace is testing/match.go's: not the same as Unicode's Z class.
func isSpace(r rune) bool {
	if r < 0x2000 {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xA0, 0x1680:
			return true
		}
	} else {
		if r <= 0x200a {
			return true
		}
		switch r {
		case 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
			return true
		}
	}
	return false
}

type selector [][]*regexp.Regexp

// selects: the name, split at each /, matches each pattern of one alternative.
func (sel selector) selects(name string) bool {
	levels := strings.Split(name, "/")
	for _, alternative := range sel {
		if len(levels) < len(alternative) {
			continue
		}
		matched := true
		for i, pattern := range alternative {
			if !pattern.MatchString(levels[i]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

var compileError = regexp.MustCompile(`^[^ \t#].*:[0-9]+:[0-9]+: `)

func verdict(regex string, status int, stream io.Reader, text io.Writer) string {
	var sel selector
	for _, alternative := range splitRegexp(regex) {
		var patterns []*regexp.Regexp
		for _, pattern := range alternative {
			compiled, err := regexp.Compile(rewrite(pattern))
			if err != nil {
				return "NO-TEST\tgo test cannot read the regex: " + err.Error()
			}
			patterns = append(patterns, compiled)
		}
		sel = append(sel, patterns)
	}
	var (
		built                                               = true
		build                                               []string
		first, panicked, offline, outside, fail, pass, skip string
		noTests, noTestFiles                                bool
		started                                             []string
		ended                                               = map[string]bool{}
	)
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for scanner.Scan() {
		line := scanner.Text()
		var e event
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &e) != nil {
			// What go printed outside the stream, such as a refusal of its arguments.
			fmt.Fprintln(text, line)
			if first == "" && strings.TrimSpace(line) != "" {
				first = line
			}
			if offline == "" && strings.Contains(line, "GOPROXY=off") {
				offline = line
			}
			continue
		}
		switch e.Action {
		case "build-output":
			io.WriteString(text, e.Output)
			out := strings.TrimRight(e.Output, "\n")
			build = append(build, out)
			if offline == "" && (strings.Contains(out, "GOPROXY=off") || strings.Contains(out, "toolchain not available")) {
				offline = out
			}
		case "build-fail":
			built = false
		case "output":
			io.WriteString(text, e.Output)
			out := strings.TrimRight(e.Output, "\n")
			if panicked == "" && strings.HasPrefix(out, "panic: ") {
				panicked = out
			}
			if e.Test == "" && strings.Contains(out, "no tests to run") {
				noTests = true
			}
			if first == "" && strings.TrimSpace(out) != "" && !strings.HasPrefix(out, "=== ") {
				first = out
			}
		case "run":
			if sel.selects(e.Test) {
				started = append(started, e.Test)
			}
		case "pass", "fail", "skip":
			if e.Test == "" {
				built = built && e.FailedBuild == ""
				noTestFiles = noTestFiles || e.Action == "skip"
				continue
			}
			ended[e.Test] = true
			result := "--- " + strings.ToUpper(e.Action) + ": " + e.Test
			switch {
			case !sel.selects(e.Test):
				if e.Action == "fail" && outside == "" {
					outside = result
				}
			case e.Action == "fail" && fail == "":
				fail = result
			case e.Action == "pass" && pass == "":
				pass = result
			case e.Action == "skip" && skip == "":
				skip = result
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "NO-TEST\tthe tool could not read the run's stream: " + err.Error()
	}
	if offline != "" && fail == "" && pass == "" {
		return "NO-TEST\tthe build needs what the module cache does not hold, and the tool reads no network: " + offline
	}
	if !built {
		for _, out := range build {
			if compileError.MatchString(out) {
				return "BUILD-FAILED\t" + out
			}
		}
		for _, out := range build {
			if out != "" && !strings.HasPrefix(out, "#") {
				return "BUILD-FAILED\t" + out
			}
		}
		return "BUILD-FAILED\tthe test build failed"
	}
	if fail != "" {
		return "RED\t" + fail
	}
	if status != 0 {
		for _, name := range started {
			if !ended[name] {
				why := panicked
				if why == "" {
					why = "exit " + strconv.Itoa(status)
				}
				return "RED\t" + name + " started and did not end: " + why
			}
		}
		if pass != "" {
			return fmt.Sprintf("GREEN\t%s, and then the run failed outside the tests the regex names (exit %d)", pass, status)
		}
		why := outside
		if why == "" {
			why = first
		}
		return fmt.Sprintf("NO-TEST\tthe run failed (exit %d) before a test the regex names ran: %s", status, why)
	}
	switch {
	case pass != "":
		return "GREEN\t" + pass
	case skip != "":
		return "NO-TEST\tevery test the regex names skipped: " + skip
	case noTestFiles:
		return "NO-TEST\tthe package has no test files"
	case noTests:
		return "NO-TEST\tno tests to run"
	}
	return "NO-TEST\tno test the regex names ran; only tests it reaches as the parents of others ran"
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go-verdict <regex> <exit status of go test> < <its -json stream>")
		os.Exit(2)
	}
	status, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "go-verdict: the exit status is no number:", os.Args[2])
		os.Exit(2)
	}
	fmt.Println(verdict(os.Args[1], status, os.Stdin, os.Stderr))
}
