package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// readyzAnswer is one /readyz answer: its status code and the status its body names.
type readyzAnswer struct {
	code   int
	status string
}

func readReadyz(t *testing.T, response *http.Response) readyzAnswer {
	t.Helper()
	defer response.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return readyzAnswer{code: response.StatusCode, status: fmt.Sprintf("no JSON body: %v", err)}
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("/readyz Content-Type = %q, want application/json", got)
	}
	return readyzAnswer{code: response.StatusCode, status: body.Status}
}

// The listener binary serves /readyz from its first answer, and answers it 200 only once /v1
// serves: every 200 is followed by a /v1 request that the gate does not refuse, and every answer
// before it is the 503 "starting". A binary that does not mount the path answers 404 and fails at
// once.
func TestHealthzReadyzOfTheListenerBinaryAnswers200OnlyOnceV1Serves(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	listener := startListenerProcess(t, binary, testnats.URL(t), "readyz-binary")
	if addr, own := listeningAddr(t, listener.Process), "127.0.0.1:"+strconv.Itoa(listener.port); addr != own {
		t.Fatalf("the listener listens on %s, not on its own %s", addr, own)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(listener.port) + "/readyz"
	// The deadline is a hang guard and decides nothing: the row passes at the first 200 and fails
	// at the first answer that is neither the 503 nor the 200.
	deadline := time.Now().Add(2 * time.Minute)
	starting := 0
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET /readyz: %v\n%s", err, listener.Output.String())
		}
		switch got := readReadyz(t, response); got {
		case readyzAnswer{code: http.StatusServiceUnavailable, status: "starting"}:
			starting++
		case readyzAnswer{code: http.StatusOK, status: "serving"}:
			status, answer := callListener(t, listener.port, http.MethodGet, "/v1/registry/ses_readyz_probe", "")
			if status == http.StatusServiceUnavailable {
				t.Fatalf("/readyz answered 200, and /v1 then answered 503: %s", answer)
			}
			t.Logf("/readyz answered 503 starting %d times, then 200 serving; /v1 then answered %d", starting, status)
			return
		default:
			t.Fatalf("/readyz answered %+v, want 503 starting or 200 serving", got)
		}
		select {
		case <-listener.Exited:
			t.Fatalf("the listener exited before /readyz answered 200:\n%s", listener.Output.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("/readyz did not answer 200 within the hang guard; it answered 503 starting %d times:\n%s", starting, listener.Output.String())
}
