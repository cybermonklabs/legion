// packages/envoy/cmd/dev-airlock-stands/main_test.go
//
// The command as a check script uses it: the real compiled binary, started, read for its JSON,
// called at one stand-in, read back at that stand-in's recording route, and stopped with the
// signal the script sends it. What each stand-in answers is the stand-in package's own rows;
// what this row is about is that a script can find the stands, trust them and read them back.
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
)

// buildStands builds the command into a directory of the test's own and returns its path.
func buildStands(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "dev-airlock-stands")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.3")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/dev-airlock-stands: %v\n%s", err, out)
	}
	return binary
}

// TestTheStandsServeTheirRecordsAndStopOnASignal is the whole of what a check script does with
// this command.
func TestTheStandsServeTheirRecordsAndStopOnASignal(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(buildStands(t), "--dir", dir, "--host", "s=s.airlock.test")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()

	// The report, read as a script reads it: one JSON object, written before the command
	// serves anything else.
	var out report
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&out); err != nil {
		t.Fatalf("read the command's JSON: %v\n%s", err, stderr.String())
	}
	if out.Dir != dir || out.CACertFile != filepath.Join(dir, "ca.crt") || out.CAKeyFile != filepath.Join(dir, "ca.key") {
		t.Fatalf("the report names dir %q and the authority %q, %q; want them in the directory it was given", out.Dir, out.CACertFile, out.CAKeyFile)
	}
	if _, err := tls.LoadX509KeyPair(out.CACertFile, out.CAKeyFile); err != nil {
		t.Fatalf("the authority's files are no key pair: %v", err)
	}
	if out.AWSKeyID == "" || out.AWSSecretKey == "" {
		t.Error("the report names no AWS key pair; a script has nothing to sign its calls with")
	}
	for _, name := range stands {
		stand, served := out.Stands[name]
		if !served || stand.Address == "" || stand.URL == "" || stand.Records == "" {
			t.Fatalf("the report's %s stand is %+v; want an address, a URL and a recording route", name, stand)
		}
		if plainStands[name] {
			continue
		}
		if stand.CertFile != filepath.Join(dir, name+".crt") {
			t.Errorf("the %s stand's certificate is %q; want one written in the directory", name, stand.CertFile)
		}
	}

	// One call to S, verified against the authority's file alone and under the name --host
	// gave it, which is how a command behind the airlock reaches a stand-in.
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(out.CACertFile)
	if err != nil {
		t.Fatal(err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no PEM certificate", out.CACertFile)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "s.airlock.test", MinVersion: tls.VersionTLS12},
	}}
	request, err := http.NewRequest(http.MethodGet, out.Stands[standS].URL+"/v1/things?page=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Check", "k4")
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("call the S stand: %v", err)
	}
	answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the S stand answered %d; want 200", answer.StatusCode)
	}

	// Every stand's recording route answers, and S's holds the call just made.
	for _, name := range stands {
		records := recordsOf(t, out.Stands[name].Records)
		if name == standS {
			if len(records) != 1 {
				t.Fatalf("the S stand's records hold %d requests; want the one call", len(records))
			}
			got := records[0]
			if got.Method != http.MethodGet || got.Path != "/v1/things" || got.Query != "page=2" || got.Header.Get("X-Check") != "k4" {
				t.Errorf("the S stand recorded %+v; want the call as it was sent", got)
			}
			continue
		}
		if len(records) != 0 {
			t.Errorf("the %s stand's records hold %d requests; want none", name, len(records))
		}
	}

	// And the signal a script sends it: a clean stop.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the command: %v", err)
	}
	stopped = true
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the command exited %v on SIGTERM; want a clean stop\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the command did not stop within 10s of SIGTERM\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "stands listening records="+out.RecordsURL) {
		t.Errorf("the command logged %q; want the line naming its recording URL", stderr.String())
	}
}

// recordsOf reads one stand's recording route.
func recordsOf(t *testing.T, url string) []airlocktest.Request {
	t.Helper()
	answer, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered %d %s; want 200", url, answer.StatusCode, body)
	}
	var records []airlocktest.Request
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("%s answered no JSON array of records: %v\n%s", url, err, body)
	}
	return records
}

// startStands starts the command with args, answers the report it printed, and stops it when
// the row ends. The row above starts and stops the command itself, since the start and the stop
// are what it is about; this is for a row about what the stands answer once they are up.
func startStands(t *testing.T, args ...string) report {
	t.Helper()
	cmd := exec.Command(buildStands(t), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	var out report
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&out); err != nil {
		t.Fatalf("read the command's JSON: %v\n%s", err, stderr.String())
	}
	return out
}

// TestTheModelStandsAnswerIsSetWhileItRuns is the command's own half of the model stand: the
// route the report names sets what the stand answers from then on, which is how one check
// drives several cases against one running stack, and a body that sets no answer is refused
// rather than quietly leaving the last one in place.
func TestTheModelStandsAnswerIsSetWhileItRuns(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir())
	if out.ModelAnswerURL != out.RecordsURL+"/answers/model" {
		t.Fatalf("the report names the answer route %q; want it on the recording server at %s", out.ModelAnswerURL, out.RecordsURL)
	}
	if out.DispatchOpenIssue == "" || out.DispatchDoneIssue == "" {
		t.Errorf("the report names the issues %q and %q; want the open one and the done one",
			out.DispatchOpenIssue, out.DispatchDoneIssue)
	}

	// Until a check sets anything the model approves, so a stack nothing called this route
	// on still judges a request through to a grant.
	if reply := askTheModel(t, out.Stands[standModel].URL); !strings.Contains(reply, "approve") {
		t.Errorf("the first answer is %q; want the approving one it starts with", reply)
	}
	for _, c := range []struct {
		name, answer, want, unwanted string
	}{
		{name: "a referring verdict", answer: `{"verdict":"refer","reason":"the reason names no issue"}`, want: "refer", unwanted: "approve"},
		{name: "a reply that is no object", answer: `{"text":"I would rather not say."}`, want: "I would rather not say."},
	} {
		t.Run(c.name, func(t *testing.T) {
			if status, body := setTheAnswer(t, out.ModelAnswerURL, c.answer); status != http.StatusOK {
				t.Fatalf("setting %s answered %d %s; want 200", c.answer, status, body)
			}
			reply := askTheModel(t, out.Stands[standModel].URL)
			if !strings.Contains(reply, c.want) || (c.unwanted != "" && strings.Contains(reply, c.unwanted)) {
				t.Errorf("the answer is %q; want %q in it", reply, c.want)
			}
		})
	}
	if status, body := setTheAnswer(t, out.ModelAnswerURL, `{}`); status != http.StatusBadRequest {
		t.Errorf("an answer naming nothing was answered %d %s; want 400", status, body)
	}

	records := recordsOf(t, out.Stands[standModel].Records)
	if len(records) != 3 {
		t.Fatalf("the model stand recorded %d calls; want the three the row made", len(records))
	}
	if records[0].Path != "/v1/messages" || records[0].Method != http.MethodPost {
		t.Errorf("the model stand recorded %s %s; want the Messages API call", records[0].Method, records[0].Path)
	}
}

// setTheAnswer posts one answer to the model stand's answer route.
func setTheAnswer(t *testing.T, url, answer string) (int, string) {
	t.Helper()
	reply, err := http.Post(url, "application/json", strings.NewReader(answer))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer reply.Body.Close()
	body, err := io.ReadAll(reply.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return reply.StatusCode, strings.TrimSpace(string(body))
}

// askTheModel makes one Messages API call against the model stand and answers the event stream
// it got, which is where the answer the route set rides.
func askTheModel(t *testing.T, url string) string {
	t.Helper()
	reply, err := http.Post(url+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"the-judge-model","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"decide this"}]}`))
	if err != nil {
		t.Fatalf("call the model stand: %v", err)
	}
	defer reply.Body.Close()
	body, err := io.ReadAll(reply.Body)
	if err != nil {
		t.Fatalf("read the model stand's answer: %v", err)
	}
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("the model stand answered %d %s; want 200", reply.StatusCode, body)
	}
	return string(body)
}
