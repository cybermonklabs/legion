package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/agentloop"
)

// limiter holds the chat tokens of a whole eval under a number a minute: each call reserves what
// it may spend before it is made and waits while the calls of the last minute and its reservation
// would pass the limit, and what the call spent, as the model's answer counts it, replaces its
// reservation once it ends. Every call of the eval's server and agent goes through it (chatProxy),
// and so does every call of the judge.
type limiter struct {
	perMinute int
	mu        sync.Mutex
	changed   chan struct{}
	spent     []spend
}

// spend is one call's tokens and when they were spent: reserved while the call runs, and then
// the call's own count, at its end.
type spend struct {
	at     time.Time
	tokens int
	open   bool
}

func newLimiter(perMinute int) *limiter {
	return &limiter{perMinute: perMinute, changed: make(chan struct{})}
}

// reserve waits until estimate more tokens fit the last minute, and reserves them; done, called
// once the call ends, puts the call's own count (or the estimate, where the answer gave none) in
// place of the reservation. A call bigger than the limit alone goes when the last minute spent
// nothing.
func (l *limiter) reserve(ctx context.Context, estimate int) (func(spent int), error) {
	for {
		l.mu.Lock()
		now := time.Now()
		used := 0
		for _, s := range l.spent {
			if s.open || now.Sub(s.at) < time.Minute {
				used += s.tokens
			}
		}
		if used == 0 || used+estimate <= l.perMinute {
			index := len(l.spent)
			l.spent = append(l.spent, spend{at: now, tokens: estimate, open: true})
			l.mu.Unlock()
			return func(spent int) {
				l.mu.Lock()
				if spent <= 0 {
					spent = estimate
				}
				l.spent[index] = spend{at: time.Now(), tokens: spent}
				close(l.changed)
				l.changed = make(chan struct{})
				l.mu.Unlock()
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		case <-time.After(time.Second):
		}
	}
}

// highestMinute is the most tokens spent within any 60 seconds of the eval, and the calls counted.
func (l *limiter) highestMinute() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	spent := slices.Clone(l.spent)
	slices.SortFunc(spent, func(a, b spend) int { return a.at.Compare(b.at) })
	highest, window, first := 0, 0, 0
	for _, s := range spent {
		window += s.tokens
		for s.at.Sub(spent[first].at) >= time.Minute {
			window -= spent[first].tokens
			first++
		}
		highest = max(highest, window)
	}
	return highest, len(spent)
}

// chatProxy is the chat endpoint the eval's server and its agent call: an https listener on
// loopback that sends each call on to the deployment's endpoint as it came, the server's own
// Azure sign-in included, through the limiter, and counts what each call spent from the usage
// its answer streams. Its certificate is its own, which the server trusts beside the system's
// authorities (bundle).
type chatProxy struct {
	server   *http.Server
	listener net.Listener
	upstream *url.URL
	bundle   string
}

// startChatProxy serves a proxy of upstream, the deployment's chat endpoint, on loopback, and
// writes into dir the bundle of certificate authorities the server is given as SSL_CERT_FILE, and
// chat-calls.log, a line for each call: its status, time, tokens and size, and the start of the
// answer of a call that failed.
func startChatProxy(upstream *url.URL, limits *limiter, dir string) (*chatProxy, error) {
	certificate, certPEM, err := selfSigned()
	if err != nil {
		return nil, err
	}
	system, err := systemAuthorities()
	if err != nil {
		return nil, err
	}
	bundle := filepath.Join(dir, "authorities.pem")
	if err := os.WriteFile(bundle, append(append(system, '\n'), certPEM...), 0o600); err != nil {
		return nil, fmt.Errorf("write the certificate bundle: %w", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, fmt.Errorf("listen for the chat proxy: %w", err)
	}
	calls, err := os.Create(filepath.Join(dir, "chat-calls.log"))
	if err != nil {
		return nil, err
	}
	var logMu sync.Mutex
	logCall := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(calls, time.Now().UTC().Format(time.RFC3339Nano)+" "+format+"\n", args...)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.URL.Path, r.Out.URL.RawPath = r.In.URL.Path, r.In.URL.RawPath
			r.Out.Host = upstream.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logCall("the call did not reach the deployment: %v", err)
			http.Error(w, "the chat proxy could not reach the deployment", http.StatusBadGateway)
		},
	}
	p := &chatProxy{listener: listener, upstream: upstream, bundle: bundle}
	p.server = &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read the call: "+err.Error(), http.StatusBadRequest)
			return
		}
		started := time.Now()
		done, err := limits.reserve(r.Context(), estimateTokens(body))
		if err != nil {
			http.Error(w, "the call ended before the limiter let it through", http.StatusServiceUnavailable)
			return
		}
		waited := time.Since(started)
		counter := &usageCounter{}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		writer := &countingWriter{ResponseWriter: w, counter: counter, status: http.StatusOK}
		proxy.ServeHTTP(writer, r)
		tokens := counter.total()
		done(tokens)
		var bound struct {
			MaxTokens           *int `json:"max_tokens"`
			MaxCompletionTokens *int `json:"max_completion_tokens"`
		}
		_ = json.Unmarshal(body, &bound)
		line := fmt.Sprintf("status=%d ms=%d waited_ms=%d tokens=%d prompt_tokens=%d completion_tokens=%d finish=%q max_tokens=%s max_completion_tokens=%s request_bytes=%d answer_bytes=%d",
			writer.status, time.Since(started).Milliseconds(), waited.Milliseconds(), tokens, counter.prompt, counter.completion,
			counter.finish, jsonOrNone(bound.MaxTokens), jsonOrNone(bound.MaxCompletionTokens), len(body), writer.written)
		if writer.status >= 300 || tokens == 0 {
			line += " answer=" + strconv.Quote(string(writer.head))
		}
		logCall("%s", line)
	})}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}

// endpoint is the base URL the server is given in place of the deployment's: the proxy's origin
// with the deployment's own path.
func (p *chatProxy) endpoint() string {
	return "https://" + p.listener.Addr().String() + p.upstream.Path
}

func (p *chatProxy) close() {
	_ = p.server.Close()
}

// estimateTokens is what a chat call may spend before its answer says: a token for each three
// bytes of the request, and the most tokens it lets the answer write.
func estimateTokens(body []byte) int {
	var request struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &request)
	return len(body)/3 + max(request.MaxTokens, request.MaxCompletionTokens, 1000)
}

// countingWriter passes the answer through and reads its usage on the way, keeping its status,
// its size and its first bytes for the call's line.
type countingWriter struct {
	http.ResponseWriter
	counter *usageCounter
	status  int
	written int
	head    []byte
}

func (c *countingWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *countingWriter) Write(data []byte) (int, error) {
	c.counter.read(data)
	c.written += len(data)
	if len(c.head) < 600 {
		c.head = append(c.head, data[:min(len(data), 600-len(c.head))]...)
	}
	return c.ResponseWriter.Write(data)
}

func (c *countingWriter) Flush() {
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// jsonOrNone is a request's number as the call's line writes it, "none" where the request named
// none.
func jsonOrNone(value *int) string {
	if value == nil {
		return "none"
	}
	return strconv.Itoa(*value)
}

// usageCounter reads what a chat answer counts: the usage object of a whole answer, or of the
// last event of a stream that carries one, and the finish reason its last choice gives.
type usageCounter struct {
	mu                 sync.Mutex
	pending            []byte
	tokens             int
	prompt, completion int
	finish             string
}

func (u *usageCounter) read(data []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pending = append(u.pending, data...)
	for {
		i := bytes.IndexByte(u.pending, '\n')
		if i < 0 {
			break
		}
		u.line(u.pending[:i])
		u.pending = u.pending[i+1:]
	}
}

func (u *usageCounter) line(line []byte) {
	line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	if !bytes.Contains(line, []byte(`"usage"`)) && !bytes.Contains(line, []byte(`"finish_reason"`)) {
		return
	}
	var answer struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(line, &answer) != nil {
		return
	}
	for _, choice := range answer.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			u.finish = *choice.FinishReason
		}
	}
	if answer.Usage != nil {
		u.prompt, u.completion = answer.Usage.Prompt, answer.Usage.Completion
		u.tokens = max(answer.Usage.Total, answer.Usage.Prompt+answer.Usage.Completion)
	}
}

func (u *usageCounter) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.pending) > 0 {
		u.line(u.pending)
		u.pending = nil
	}
	return u.tokens
}

// selfSigned is a certificate for 127.0.0.1, valid for three days, and its PEM.
func selfSigned() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "going-eval chat proxy"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(72 * time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, certPEM, nil
}

// systemAuthorities is the PEM of the certificate authorities this process trusts: the file
// SSL_CERT_FILE names, or the first of the system's usual bundles.
func systemAuthorities() ([]byte, error) {
	candidates := []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/pki/tls/cacert.pem", "/etc/ssl/cert.pem"}
	if own := os.Getenv("SSL_CERT_FILE"); own != "" {
		candidates = []string{own}
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return nil, errors.New("found no bundle of certificate authorities to trust the chat model's endpoint by; set SSL_CERT_FILE")
}

// judge reads an answer against the facts its task lists, with one call of a chat deployment on
// the eval's own Azure sign-in, through the limiter.
type judge struct {
	model   agentloop.Model
	limits  *limiter
	written time.Time
}

// judgement is the judge's reading of one answer: whether it states each fact, and why.
type judgement struct {
	Stated []bool
	Why    []string
}

// pass reports whether the answer states every fact.
func (j judgement) pass() bool {
	return len(j.Stated) > 0 && !slices.Contains(j.Stated, false)
}

// judgePrompt is the judge's system prompt; %s is the day the seed's facts are written for.
const judgePrompt = `You grade an answer that the assistant of Dispatch, an issue tracker, gave a person. You are given the words the person typed, the answer, and numbered facts that a right answer states.

For each fact, decide whether the answer states it. The answer may state it in other words, in a table or a list, or with more beside it. A fact the answer leaves out, gets wrong, contradicts or states only in part is not stated. A number, a name, an issue key, a time or a state must be the fact's own. Read only what the answer says, not what it might have meant.

The facts were written on %s, the day the data is about: "today" in a fact is that day, and an answer that names that day or that time states it. A week may be named by its ISO number (2026-W45) or by a day of it, such as its Monday (2026-11-02): both name the one week.

Answer with one JSON object and nothing else: {"facts": [{"fact": <its number>, "stated": true or false, "why": "<one sentence>"}]}, one entry for each fact, in order.`

// read is the judge's reading of answer, to the person's words, against facts.
func (j *judge) read(ctx context.Context, words, answer string, facts []string) (judgement, error) {
	var text strings.Builder
	fmt.Fprintf(&text, "The person typed:\n%s\n\nThe answer:\n%s\n\nThe facts:\n", words, answer)
	for i, fact := range facts {
		fmt.Fprintf(&text, "%d. %s\n", i+1, fact)
	}
	system := fmt.Sprintf(judgePrompt, j.written.UTC().Format("Monday 2 January 2006, 15:04 UTC"))
	var last error
	for range 3 {
		reading, err := j.call(ctx, system, text.String(), len(facts))
		if err == nil {
			return reading, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return judgement{}, last
}

// call is one judge call and its reading.
func (j *judge) call(ctx context.Context, system, text string, facts int) (judgement, error) {
	done, err := j.limits.reserve(ctx, (len(system)+len(text))/3+4000)
	if err != nil {
		return judgement{}, err
	}
	result, err := agentloop.Run(ctx, agentloop.Request{
		Model:   j.model,
		System:  system,
		Input:   []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: text}}}},
		Bounds:  agentloop.Bounds{Time: 3 * time.Minute, Calls: 1, OutputTokens: 4000},
		NoCache: true,
	})
	done(result.Usage.Input + result.Usage.Output)
	if err != nil {
		return judgement{}, err
	}
	if result.Fault != "" {
		return judgement{}, fmt.Errorf("the judge's call ended %s: %s", result.Fault, result.Detail)
	}
	reply := result.Text
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return judgement{}, fmt.Errorf("the judge answered no JSON object: %q", clipText(reply))
	}
	var parsed struct {
		Facts []struct {
			Fact   int    `json:"fact"`
			Stated bool   `json:"stated"`
			Why    string `json:"why"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &parsed); err != nil {
		return judgement{}, fmt.Errorf("the judge's answer is no reading: %w", err)
	}
	reading := judgement{Stated: make([]bool, facts), Why: make([]string, facts)}
	seen := make([]bool, facts)
	for _, f := range parsed.Facts {
		if f.Fact < 1 || f.Fact > facts {
			continue
		}
		reading.Stated[f.Fact-1], reading.Why[f.Fact-1], seen[f.Fact-1] = f.Stated, f.Why, true
	}
	if slices.Contains(seen, false) {
		return judgement{}, fmt.Errorf("the judge read %d of the %d facts", len(parsed.Facts), facts)
	}
	return reading, nil
}

// clipText is text cut to a line a log can show.
func clipText(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) > 200 {
		return flat[:197] + "..."
	}
	return flat
}

// sseFrames reads a stream of server-sent events: each frame's id, name and data.
func sseFrames(body io.Reader, each func(id, name string, data []byte)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var id, name string
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if name != "" || len(data) > 0 {
				each(id, name, data)
			}
			id, name, data = "", "", nil
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
	}
	if name != "" || len(data) > 0 {
		each(id, name, data)
	}
	return scanner.Err()
}
