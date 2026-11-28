package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// replayUsage is the replay line of usage.
const replayUsage = "       going-eval replay --body <file> [--tries <n>] [--parallel <n>] [--variants <names>] [--bisect <part>] [--system <file>] [--replace <old>=><new>]..."

// replayCommand is `going-eval replay`: it sends one chat request body - refused/<n>.json, which
// the eval's chat proxy keeps of each call the deployment's content filter refused
// (startChatProxy) - to the chat deployment run reads from its environment, on the server's own
// keyless Azure sign-in (chatToken), --tries times for each variant of it, and prints for each
// variant how many calls the filter refused, how many the deployment answered and how many failed
// otherwise, with the filter's label. A variant is the body with one named part taken out
// (replayVariants): the system message, a half of its lines, the tool list, the page and the
// page's read in the person's message, or the text of one message. --bisect then narrows the
// refusal to the words of one part (narrow): the system message, a message or the tool list, by
// its lines, then the sentences and the words of each line the refusal needs. --system puts a
// file's text in place of the body's system message, and --replace swaps words in every text of
// the body, before any variant: so a body refused with an older prompt is replayed with the
// prompt of this tree, and a rewording is measured before it is made. It exits 0 once every
// variant was sent, 1 when the body cannot be read or the deployment not reached, and 2 on a
// usage error.
func replayCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("going-eval replay", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bodyFile := flags.String("body", "", "the chat request body to send, such as refused/<n>.json of a run's work dir (required)")
	tries := flags.Int("tries", 5, "how many times each variant is sent")
	parallel := flags.Int("parallel", 4, "how many calls are made at once")
	variants := flags.String("variants", "all", "the variants to send, by name and comma-separated (replayVariants); all for every one the body has, none for none")
	bisect := flags.String("bisect", "", "the part whose words the refusal is narrowed to: system, tools or message-<i>")
	systemFile := flags.String("system", "", "a file whose text replaces the body's system message before any variant")
	var replaces replaceFlag
	flags.Var(&replaces, "replace", "<old>=><new>: replace old by new in every text of the body before any variant; repeat for more")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || strings.TrimSpace(*bodyFile) == "" || *tries < 1 || *parallel < 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	raw, err := os.ReadFile(*bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval replay: %v\n", err)
		return 1
	}
	body, err := readChatBody(raw)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval replay: %s: %v\n", *bodyFile, err)
		return 1
	}
	if *systemFile != "" {
		text, err := os.ReadFile(*systemFile)
		if err != nil {
			fmt.Fprintf(stderr, "going-eval replay: %v\n", err)
			return 1
		}
		if !body.setSystem(string(text)) {
			fmt.Fprintf(stderr, "going-eval replay: %s holds no system message to replace\n", *bodyFile)
			return 1
		}
		raw = nil
	}
	if len(replaces) > 0 {
		for _, r := range replaces {
			if n := body.replace(r.old, r.new); n == 0 {
				fmt.Fprintf(stderr, "going-eval replay: %q is in no text of the body\n", r.old)
				return 1
			}
		}
		raw = nil
	}
	chat, err := chatFromEnvironment()
	if err != nil {
		fmt.Fprintf(stderr, "going-eval replay: %v\n", err)
		return 1
	}
	token, err := chatToken(chat)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval replay: %v\n", err)
		return 1
	}
	endpoint := strings.TrimRight(chat.Endpoint, "/") + "/chat/completions"
	if chat.APIVersion != "" {
		endpoint += "?" + url.Values{"api-version": {chat.APIVersion}}.Encode()
	}
	r := &replayer{
		endpoint: endpoint, token: token, client: &http.Client{Timeout: 3 * time.Minute},
		limits: newLimiter(tokenLimit), tries: *tries, parallel: *parallel, out: stdout,
	}
	fmt.Fprintf(stdout, "# going-eval replay of %s to %s, %d tries a variant, %s\n", *bodyFile, chat.Deployment, *tries, time.Now().UTC().Format(time.RFC3339))
	if err := r.replay(ctx, body, raw, *variants, *bisect); err != nil {
		fmt.Fprintf(stderr, "going-eval replay: %v\n", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	return 0
}

// errUsage is a variant or a part the body does not have.
var errUsage = errors.New("usage")

// chatToken is the token source of the server's own keyless Azure sign-in for the chat model of
// chat (cmd/dispatch's chatModel), at the scope chat names, else embed.DefaultEntraScope.
func chatToken(chat chatSettings) (func(context.Context) (string, error), error) {
	tokens, err := embed.NewEntraTokenSource(embed.EntraConfig{
		TenantID: chat.TenantID, ClientID: chat.ClientID, TokenFile: chat.TokenFile, Scope: cmp.Or(chat.Scope, embed.DefaultEntraScope),
	})
	if err != nil {
		return nil, fmt.Errorf("the Azure sign-in: %w", err)
	}
	return tokens.Token, nil
}

// replaceFlag is each --replace, old=>new.
type replaceFlag []struct{ old, new string }

func (f *replaceFlag) String() string { return fmt.Sprint(len(*f)) }

func (f *replaceFlag) Set(value string) error {
	old, new, ok := strings.Cut(value, "=>")
	if !ok || old == "" {
		return errors.New("want <old>=><new>, old not empty")
	}
	*f = append(*f, struct{ old, new string }{old, new})
	return nil
}

// chatBody is a Chat Completions request body, decoded with its numbers as written, which each
// variant changes a copy of.
type chatBody struct {
	value map[string]any
}

// readChatBody decodes raw as a Chat Completions request: an object with a list of messages.
func readChatBody(raw []byte) (*chatBody, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("no JSON object: %w", err)
	}
	if _, ok := value["messages"].([]any); !ok {
		return nil, errors.New("no list of messages: not a Chat Completions request")
	}
	return &chatBody{value: value}, nil
}

// encode is the body as JSON, its text as written (no HTML escapes).
func (b *chatBody) encode() []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(b.value)
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// clone is a copy of the body that shares nothing with it.
func (b *chatBody) clone() *chatBody {
	copied, err := readChatBody(b.encode())
	if err != nil {
		panic(err)
	}
	return copied
}

func (b *chatBody) messages() []any {
	messages, _ := b.value["messages"].([]any)
	return messages
}

func (b *chatBody) tools() []any {
	tools, _ := b.value["tools"].([]any)
	return tools
}

// systemIndex is the index of the body's first system message, -1 for none.
func (b *chatBody) systemIndex() int {
	for i, m := range b.messages() {
		if message, _ := m.(map[string]any); message["role"] == "system" || message["role"] == "developer" {
			return i
		}
	}
	return -1
}

// text is message i's text: its content where that is a string, else the text of its parts.
func (b *chatBody) text(i int) string {
	message, _ := b.messages()[i].(map[string]any)
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		var text strings.Builder
		for _, part := range content {
			if p, ok := part.(map[string]any); ok {
				t, _ := p["text"].(string)
				text.WriteString(t)
			}
		}
		return text.String()
	}
	return ""
}

// setText puts text in place of message i's, as a string or as one text part, as it was given.
func (b *chatBody) setText(i int, text string) {
	message, _ := b.messages()[i].(map[string]any)
	if _, parts := message["content"].([]any); parts {
		message["content"] = []any{map[string]any{"type": "text", "text": text}}
		return
	}
	message["content"] = text
}

// setSystem puts text in place of the system message's, and reports whether the body has one.
func (b *chatBody) setSystem(text string) bool {
	i := b.systemIndex()
	if i < 0 {
		return false
	}
	b.setText(i, text)
	return true
}

// replace puts new in place of each old in every text the body holds - every message's, every
// tool's - and answers how many it replaced.
func (b *chatBody) replace(old, new string) int {
	n := 0
	var walk func(value any) any
	walk = func(value any) any {
		switch value := value.(type) {
		case string:
			n += strings.Count(value, old)
			return strings.ReplaceAll(value, old, new)
		case []any:
			for i := range value {
				value[i] = walk(value[i])
			}
		case map[string]any:
			for key := range value {
				value[key] = walk(value[key])
			}
		}
		return value
	}
	for _, key := range []string{"messages", "tools"} {
		if value, ok := b.value[key]; ok {
			b.value[key] = walk(value)
		}
	}
	return n
}

// openingParts are the parts of the person's message, as going/agent's conversation writes it,
// each by the words it starts with.
var openingParts = []string{
	"Earlier in this conversation", "The person asks now:", "The time now:", "The page the person asks from",
	"The page's issue, as read gave it", "The earlier steps of this run",
}

// withoutOpeningParts is text, the person's message, without each part whose first words drop
// names: a part runs from its first words at a line's start to the next part's.
func withoutOpeningParts(text string, drop ...string) (string, bool) {
	type part struct {
		start int
		name  string
	}
	var parts []part
	for _, name := range openingParts {
		for at := 0; ; {
			i := strings.Index(text[at:], name)
			if i < 0 {
				break
			}
			if start := at + i; start == 0 || text[start-1] == '\n' {
				parts = append(parts, part{start, name})
				break
			}
			at += i + len(name)
		}
	}
	slices.SortFunc(parts, func(a, b part) int { return a.start - b.start })
	var out strings.Builder
	dropped := false
	last := 0
	for i, p := range parts {
		end := len(text)
		if i+1 < len(parts) {
			end = parts[i+1].start
		}
		if slices.Contains(drop, p.name) {
			out.WriteString(text[last:p.start])
			last, dropped = end, true
		}
	}
	out.WriteString(text[last:])
	return out.String(), dropped
}

// variant is one body to send, and its name.
type variant struct {
	name string
	body []byte
}

// replayVariants is each variant names lists of body: "all" names every one the body has, in
// this order - whole, the body as given; reencoded, decoded and encoded again, which every other
// variant is, so a difference of the encoding alone shows; no-system; drop-system-lines-A-B for
// each half of the system message's lines; no-tools, with tool_choice and parallel_tool_calls;
// no-page, the person's message without the page and the page's read; no-page-read, without the
// read alone; and blank-message-<i> for each message after the system message, its text replaced
// by "(removed)", its tool calls and their ids kept, so the conversation stays one the
// deployment takes. A part the body does not have, such as a page, names no variant of all; named
// alone, it is refused. drop-system-lines-A-B and drop-message-<i>-lines-A-B take any lines, from
// 1. raw is the body's own bytes, which whole sends, nil where the body was changed.
func replayVariants(body *chatBody, raw []byte, names string) ([]variant, error) {
	system := body.systemIndex()
	names = strings.TrimSpace(names)
	if names == "none" {
		return nil, nil
	}
	all := names == "all"
	if all {
		list := []string{"whole", "reencoded"}
		if system >= 0 {
			lines := len(strings.SplitAfter(body.text(system), "\n"))
			list = append(list, "no-system", fmt.Sprintf("drop-system-lines-1-%d", lines/2), fmt.Sprintf("drop-system-lines-%d-%d", lines/2+1, lines))
		}
		list = append(list, "no-tools", "no-page", "no-page-read")
		for i := range body.messages() {
			if i != system {
				list = append(list, fmt.Sprintf("blank-message-%d", i))
			}
		}
		names = strings.Join(list, ",")
	}
	var out []variant
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		v, err := makeVariant(body, raw, name)
		if all && errors.Is(err, errUsage) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, variant{name: name, body: v})
	}
	return out, nil
}

var (
	dropLines    = regexp.MustCompile(`^drop-(system|message-(\d+))-lines-(\d+)-(\d+)$`)
	blankMessage = regexp.MustCompile(`^blank-message-(\d+)$`)
)

// makeVariant is the body of the variant name names (replayVariants).
func makeVariant(body *chatBody, raw []byte, name string) ([]byte, error) {
	if name == "whole" && raw != nil {
		return raw, nil
	}
	b := body.clone()
	messages := b.messages()
	switch {
	case name == "whole" || name == "reencoded":
	case name == "no-system":
		i := b.systemIndex()
		if i < 0 {
			return nil, fmt.Errorf("%w: the body holds no system message", errUsage)
		}
		b.value["messages"] = slices.Delete(messages, i, i+1)
	case name == "no-tools":
		if b.tools() == nil {
			return nil, fmt.Errorf("%w: the body holds no tools", errUsage)
		}
		delete(b.value, "tools")
		delete(b.value, "tool_choice")
		delete(b.value, "parallel_tool_calls")
	case name == "no-page" || name == "no-page-read":
		drop := []string{"The page's issue, as read gave it"}
		if name == "no-page" {
			drop = append(drop, "The page the person asks from")
		}
		found := false
		for i, m := range messages {
			if message, _ := m.(map[string]any); message["role"] == "user" {
				if text, dropped := withoutOpeningParts(b.text(i), drop...); dropped {
					b.setText(i, text)
					found = true
				}
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: the person's message holds no page", errUsage)
		}
	case blankMessage.MatchString(name):
		i, _ := strconv.Atoi(blankMessage.FindStringSubmatch(name)[1])
		if i >= len(messages) {
			return nil, fmt.Errorf("%w: the body holds %d messages, not %d", errUsage, len(messages), i+1)
		}
		b.setText(i, "(removed)")
	case dropLines.MatchString(name):
		match := dropLines.FindStringSubmatch(name)
		i := b.systemIndex()
		if match[1] != "system" {
			i, _ = strconv.Atoi(match[2])
		}
		from, _ := strconv.Atoi(match[3])
		to, _ := strconv.Atoi(match[4])
		if i < 0 || i >= len(messages) {
			return nil, fmt.Errorf("%w: the body holds no %s", errUsage, match[1])
		}
		lines := strings.SplitAfter(b.text(i), "\n")
		if from < 1 || to < from || to > len(lines) {
			return nil, fmt.Errorf("%w: %s: the text holds lines 1-%d", errUsage, name, len(lines))
		}
		b.setText(i, strings.Join(slices.Delete(lines, from-1, to), ""))
	default:
		return nil, fmt.Errorf("%w: no variant is named %q", errUsage, name)
	}
	return b.encode(), nil
}

// replayer sends variants of a body to the chat deployment and says what each drew.
type replayer struct {
	endpoint string
	token    func(context.Context) (string, error)
	client   *http.Client
	limits   *limiter
	tries    int
	parallel int
	out      io.Writer
}

// outcome is how the deployment answered one call: refused by its content filter, with what the
// filter said (agentloop.ContentFilterRefusal); answered, its stream's first reply arrived; or
// neither, with why.
type outcome struct {
	refused, answered bool
	words, other      string
}

// tally is what the tries of one variant drew.
type tally struct {
	refused, answered int
	labels            map[string]int
	others            map[string]int
}

// filterLabel is the label Azure's words name ("Response content blocked by label 'Jailbreak'."),
// else the words whole.
var filterLabel = regexp.MustCompile(`label '([^']+)'`)

func (t tally) String() string {
	labels := "-"
	if len(t.labels) > 0 {
		var parts []string
		for _, label := range slices.Sorted(maps.Keys(t.labels)) {
			parts = append(parts, label+"="+strconv.Itoa(t.labels[label]))
		}
		labels = strings.Join(parts, ",")
	}
	other := 0
	for _, n := range t.others {
		other += n
	}
	return fmt.Sprintf("refused=%d answered=%d other=%d labels=%s", t.refused, t.answered, other, labels)
}

// replay sends each variant names lists, then bisects part where it names one.
func (r *replayer) replay(ctx context.Context, body *chatBody, raw []byte, names, part string) error {
	variants, err := replayVariants(body, raw, names)
	if err != nil {
		return err
	}
	for _, v := range variants {
		t, err := r.sendTries(ctx, v.body)
		if err != nil {
			return err
		}
		fmt.Fprintf(r.out, "replay variant=%s bytes=%d tries=%d %s\n", v.name, len(v.body), r.tries, t)
		for _, why := range slices.Sorted(maps.Keys(t.others)) {
			fmt.Fprintf(r.out, "replay variant=%s other %dx: %s\n", v.name, t.others[why], why)
		}
	}
	if part == "" {
		return nil
	}
	return r.bisect(ctx, body, part)
}

// sendTries sends body r.tries times, r.parallel at once, and tallies what the calls drew.
func (r *replayer) sendTries(ctx context.Context, body []byte) (tally, error) {
	t := tally{labels: map[string]int{}, others: map[string]int{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, r.parallel)
	for range r.tries {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			o := r.send(ctx, body)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case o.refused:
				t.refused++
				label := o.words
				if match := filterLabel.FindStringSubmatch(o.words); match != nil {
					label = match[1]
				}
				t.labels[cmp.Or(label, "unnamed")]++
			case o.answered:
				t.answered++
			default:
				t.others[o.other]++
			}
		})
	}
	wg.Wait()
	return t, ctx.Err()
}

// send makes one call of body, through the limiter, and makes it again after a 429, up to five
// times, after the wait the deployment names (Retry-After), else two seconds.
func (r *replayer) send(ctx context.Context, body []byte) outcome {
	for attempt := 0; ; attempt++ {
		done, err := r.limits.reserve(ctx, estimateTokens(body))
		if err != nil {
			return outcome{other: err.Error()}
		}
		o, wait := r.post(ctx, body)
		done(0)
		if wait == 0 || attempt == 4 {
			return o
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return outcome{other: ctx.Err().Error()}
		}
	}
}

// post makes one call of body, and answers its outcome, or the wait before it is made again
// where the deployment answered 429. A refusal is read off the body of an error status, or off a
// stream chunk of a 2xx answer; a 2xx answer is answered at its first chunk that replies, and is
// read no further.
func (r *replayer) post(ctx context.Context, body []byte) (outcome, time.Duration) {
	token, err := r.token(ctx)
	if err != nil {
		return outcome{other: "the token: " + err.Error()}, 0
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return outcome{other: err.Error()}, 0
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := r.client.Do(request)
	if err != nil {
		return outcome{other: err.Error()}, 0
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		wait := 2 * time.Second
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
			wait = time.Duration(min(seconds, 60)) * time.Second
		}
		return outcome{other: "429"}, wait
	}
	if response.StatusCode/100 != 2 {
		head, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if words, filtered := agentloop.ContentFilterRefusal(head); filtered {
			return outcome{refused: true, words: words}, 0
		}
		return outcome{other: fmt.Sprintf("%d %s", response.StatusCode, clipText(string(head)))}, 0
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(scanner.Bytes()), []byte("data:"))
		if !ok {
			continue
		}
		if words, filtered := agentloop.ContentFilterRefusal(data); filtered {
			return outcome{refused: true, words: words}, 0
		}
		if replied(data) {
			return outcome{answered: true}, 0
		}
	}
	return outcome{other: "the stream ended with no reply"}, 0
}

// replied reports whether a stream chunk carries the start of a reply: prose, reasoning, a tool
// call or a finish reason.
func replied(data []byte) bool {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" || c.Delta.ReasoningContent != "" || len(c.Delta.ToolCalls) > 0 || c.FinishReason != "" {
			return true
		}
	}
	return false
}

// textPart is a text of the body the bisect narrows: its name, how it is read and how it is put.
type textPart struct {
	name string
	get  func(*chatBody) string
	set  func(*chatBody, string)
}

// bisect narrows the refusal of body to the words of part: "system", the system message, or
// "message-<i>", message i's text, each by its lines, then each line's sentences and each
// sentence's words; or "tools", the tool list by its tools, then each tool's description. It
// prints each set it sends and what it drew, and at the end each piece the refusal needs.
func (r *replayer) bisect(ctx context.Context, body *chatBody, part string) error {
	var text textPart
	switch {
	case part == "system":
		i := body.systemIndex()
		if i < 0 {
			return fmt.Errorf("%w: the body holds no system message", errUsage)
		}
		text = textPart{name: "system", get: func(b *chatBody) string { return b.text(i) }, set: func(b *chatBody, s string) { b.setText(i, s) }}
	case strings.HasPrefix(part, "message-"):
		i, err := strconv.Atoi(strings.TrimPrefix(part, "message-"))
		if err != nil || i < 0 || i >= len(body.messages()) {
			return fmt.Errorf("%w: the body holds messages 0-%d, not %s", errUsage, len(body.messages())-1, part)
		}
		text = textPart{name: part, get: func(b *chatBody) string { return b.text(i) }, set: func(b *chatBody, s string) { b.setText(i, s) }}
	case part == "tools":
		return r.bisectTools(ctx, body)
	default:
		return fmt.Errorf("%w: no part is named %q: system, tools or message-<i>", errUsage, part)
	}
	return r.bisectText(ctx, body, text, 0)
}

// The levels a text is bisected at, coarsest first: its lines, a line's sentences, a sentence's
// words.
var bisectLevels = []struct {
	name  string
	split func(string) []string
}{
	{"line", func(s string) []string { return strings.SplitAfter(s, "\n") }},
	{"sentence", splitSentences},
	{"word", func(s string) []string { return strings.SplitAfter(s, " ") }},
}

// sentenceEnd ends a sentence of a rule: a full stop, a colon or a semicolon before a space, or
// a dash between spaces.
var sentenceEnd = regexp.MustCompile(`[.:;] | - `)

// splitSentences is s cut after each sentenceEnd, every byte kept.
func splitSentences(s string) []string {
	var out []string
	last := 0
	for _, match := range sentenceEnd.FindAllStringIndex(s, -1) {
		out = append(out, s[last:match[1]])
		last = match[1]
	}
	return append(out, s[last:])
}

// bisectText narrows the refusal of body to the pieces of text at level and finer. It prints each
// piece the refusal needs, and goes down a level inside each, with the others of its level taken
// out.
func (r *replayer) bisectText(ctx context.Context, body *chatBody, text textPart, level int) error {
	pieces := bisectLevels[level].split(text.get(body))
	with := func(removed []bool) []byte {
		b := body.clone()
		var kept strings.Builder
		for i, piece := range pieces {
			if !removed[i] {
				kept.WriteString(piece)
			}
		}
		// A text with every piece taken out says so, as blank-message does: the deployment
		// refuses a message with no text for that alone.
		text.set(b, cmp.Or(kept.String(), "(removed)"))
		return b.encode()
	}
	label := fmt.Sprintf("%s by %s", text.name, bisectLevels[level].name)
	found, err := r.narrow(ctx, label, len(pieces), with)
	if err != nil || found == nil {
		return err
	}
	for _, i := range found {
		fmt.Fprintf(r.out, "bisect %s: the refusal needs %s %d of %d: %q\n", label, bisectLevels[level].name, i+1, len(pieces), pieces[i])
	}
	if level+1 >= len(bisectLevels) {
		return nil
	}
	for _, i := range found {
		if len(bisectLevels[level+1].split(pieces[i])) < 2 {
			continue
		}
		// The pieces found beside this one are taken out, so the bisect inside it reads the
		// refusal this piece alone draws.
		inner := body.clone()
		var kept strings.Builder
		for j, piece := range pieces {
			if j == i || !slices.Contains(found, j) {
				kept.WriteString(piece)
			}
		}
		text.set(inner, kept.String())
		// The piece is found again in a body by the length of the text before it and of the
		// text after it, which a bisect inside it leaves as they are.
		start := 0
		for j := range i {
			if !slices.Contains(found, j) {
				start += len(pieces[j])
			}
		}
		tail := len(text.get(inner)) - start - len(pieces[i])
		piece := textPart{
			name: fmt.Sprintf("%s %s %d", label, bisectLevels[level].name, i+1),
			get: func(b *chatBody) string {
				whole := text.get(b)
				return whole[start : len(whole)-tail]
			},
			set: func(b *chatBody, s string) {
				whole := text.get(b)
				text.set(b, whole[:start]+s+whole[len(whole)-tail:])
			},
		}
		if err := r.bisectText(ctx, inner, piece, level+1); err != nil {
			return err
		}
	}
	return nil
}

// bisectTools narrows the refusal of body to the tools of its list, and to the words of each
// found tool's description.
func (r *replayer) bisectTools(ctx context.Context, body *chatBody) error {
	tools := body.tools()
	if len(tools) == 0 {
		return fmt.Errorf("%w: the body holds no tools", errUsage)
	}
	with := func(removed []bool) []byte {
		b := body.clone()
		var kept []any
		for i, tool := range b.tools() {
			if !removed[i] {
				kept = append(kept, tool)
			}
		}
		if len(kept) == 0 {
			delete(b.value, "tools")
			delete(b.value, "tool_choice")
			delete(b.value, "parallel_tool_calls")
		} else {
			b.value["tools"] = kept
		}
		return b.encode()
	}
	found, err := r.narrow(ctx, "tools by tool", len(tools), with)
	if err != nil {
		return err
	}
	for _, i := range found {
		function, _ := tools[i].(map[string]any)["function"].(map[string]any)
		name, _ := function["name"].(string)
		fmt.Fprintf(r.out, "bisect tools by tool: the refusal needs tool %d of %d: %s\n", i+1, len(tools), name)
		if _, ok := function["description"].(string); !ok {
			continue
		}
		description := func(b *chatBody) map[string]any {
			f, _ := b.tools()[i].(map[string]any)["function"].(map[string]any)
			return f
		}
		part := textPart{
			name: "tool " + name + " description",
			get:  func(b *chatBody) string { s, _ := description(b)["description"].(string); return s },
			set:  func(b *chatBody, s string) { description(b)["description"] = s },
		}
		if err := r.bisectText(ctx, body, part, 1); err != nil {
			return err
		}
	}
	return nil
}

// narrow finds the pieces, of count, whose words the refusal needs, as with(removed) builds the
// body with the pieces removed marks taken out: it confirms the body with every piece is refused
// and the body with none is not, then halves the pieces it holds until each holds one. A half
// whose removal clears the refusal holds what the refusal needs; where removing neither half
// clears it, each half alone draws it, and each is narrowed with the other taken out. A set is
// refused where any try of it was, and cleared where none was and one was answered; a set whose
// tries were neither refused nor answered - a 400 for another cause - gives no verdict, and the
// narrowing stops there with an error naming it. It answers nil where the body is not refused,
// or is refused with every piece taken out.
func (r *replayer) narrow(ctx context.Context, label string, count int, with func(removed []bool) []byte) ([]int, error) {
	refused := func(removed []bool, what string) (bool, error) {
		t, err := r.sendTries(ctx, with(removed))
		if err != nil {
			return false, err
		}
		fmt.Fprintf(r.out, "bisect %s: %s: %s\n", label, what, t)
		for _, why := range slices.Sorted(maps.Keys(t.others)) {
			fmt.Fprintf(r.out, "bisect %s: %s: other %dx: %s\n", label, what, t.others[why], why)
		}
		if t.refused == 0 && t.answered == 0 {
			return false, fmt.Errorf("bisect %s: %s drew neither a refusal nor an answer, so it says nothing of the filter", label, what)
		}
		return t.refused > 0, nil
	}
	span := func(removed []bool, from, to int) []bool {
		out := slices.Clone(removed)
		for i := from; i <= to; i++ {
			out[i] = true
		}
		return out
	}
	none := make([]bool, count)
	if ok, err := refused(none, fmt.Sprintf("with all %d", count)); err != nil || !ok {
		if err == nil {
			fmt.Fprintf(r.out, "bisect %s: the body is not refused with all %d; nothing to narrow\n", label, count)
		}
		return nil, err
	}
	if ok, err := refused(span(none, 0, count-1), "with none"); err != nil || ok {
		if err == nil {
			fmt.Fprintf(r.out, "bisect %s: the body is refused with none of the %d too; the refusal needs none of them\n", label, count)
		}
		return nil, err
	}
	var find func(from, to int, removed []bool) ([]int, error)
	find = func(from, to int, removed []bool) ([]int, error) {
		if from == to {
			return []int{from}, nil
		}
		mid := (from + to) / 2
		withoutFirst, err := refused(span(removed, from, mid), fmt.Sprintf("without %d-%d", from+1, mid+1))
		if err != nil {
			return nil, err
		}
		if !withoutFirst {
			return find(from, mid, removed)
		}
		withoutSecond, err := refused(span(removed, mid+1, to), fmt.Sprintf("without %d-%d", mid+2, to+1))
		if err != nil {
			return nil, err
		}
		if !withoutSecond {
			return find(mid+1, to, removed)
		}
		first, err := find(from, mid, span(removed, mid+1, to))
		if err != nil {
			return nil, err
		}
		second, err := find(mid+1, to, span(removed, from, mid))
		return append(first, second...), err
	}
	return find(0, count-1, none)
}
