package cipherclient

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/oauth2call"
)

// controlDumpValue dumps an answer through httputil.DumpResponse held as a function value.
func controlDumpValue(resp *http.Response) error {
	dump := httputil.DumpResponse
	b, _ := dump(resp, true)
	return fmt.Errorf("refused: %s", b)
}

// controlMakerValue holds a header as the box through NewRemoteText held as a function value.
func controlMakerValue(resp *http.Response) string {
	hold := redact.NewRemoteText
	return hold(resp.Header.Get("X-Reason")).String()
}

// controlUnmarshalHolder fills a holder of the box from the bytes StreamTo hands a buffer.
func controlUnmarshalHolder(resp *http.Response) error {
	answer, _ := redact.ReadAnswer(resp, 1<<20)
	var b bytes.Buffer
	_, _ = answer.StreamTo(&b)
	var refusal oauth2call.CodedRefusal
	_ = json.Unmarshal(b.Bytes(), &refusal)
	return fmt.Errorf("refused: %s", refusal.Error)
}

// controlUnmarshalBox fills the box through its own UnmarshalJSON, called directly.
func controlUnmarshalBox(data []byte) string {
	var said redact.RemoteText
	_ = said.UnmarshalJSON(data)
	return said.String()
}

// controlNoRequest reads an answer off a connection with no request, so it holds no record.
func controlNoRequest(conn net.Conn) string {
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	answer, _ := redact.ReadAnswer(resp, 1<<20)
	return answer.Words().String()
}

// controlWhole prints a response whole.
func controlWhole(resp *http.Response) error {
	return fmt.Errorf("refused: %+v", resp)
}

// controlConstant holds a constant as the box, which no answer wrote: the census passes it.
func controlConstant() string {
	return redact.NewRemoteText("a word of this program's own").String()
}
