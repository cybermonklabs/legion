package main

import (
	"os"
	"path/filepath"
)

// censusControls are the negative controls of the Go census's reading of every build (census.go)
// and of the answer census's rules for the ways out of an answer a type shows (answers_reach.go).
// controls (controls.go) lists them after the core controls.
var censusControls = []control{
	{id: "29", what: "a read of an answer's body and status line planted in a file only darwin builds and in one only linux/arm64 builds, and one planted in a file no CI build compiles", stage: 2,
		each: [][]string{
			{"answer census", plantDir + "control_darwin_answer.go", "controlDarwinBody", "outside"},
			{"answer census", plantDir + "control_darwin_answer.go", "controlDarwinBody", "status line as the answering party wrote it"},
			{"answer census", plantDir + "control_arm64_answer.go", "controlARM64Body", "outside"},
			{"UNKNOWN", plantDir + "control_windows_answer.go", "no CI build compiles it", "imports net/http"},
		},
		plant: func(c *controlRun) error {
			if err := c.write(plantDir+"control_darwin_answer.go", `//go:build darwin

package cipherclient

import (
	"fmt"
	"io"
	"net/http"
)

// controlDarwinBody is control (29)'s planted raw read in a file the Linux builds leave out.
func controlDarwinBody(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("the server answered %s: %s", resp.Status, body)
}
`); err != nil {
				return err
			}
			if err := c.write(plantDir+"control_arm64_answer.go", `//go:build linux && arm64

package cipherclient

import (
	"fmt"
	"io"
	"net/http"
)

// controlARM64Body is control (29)'s planted raw read in a file only linux/arm64 builds.
func controlARM64Body(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("the server answered %s", body)
}
`); err != nil {
				return err
			}
			return c.write(plantDir+"control_windows_answer.go", `//go:build windows

package cipherclient

import (
	"fmt"
	"io"
	"net/http"
)

// controlWindowsBody is control (29)'s planted raw read in a file no CI build compiles.
func controlWindowsBody(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("the server answered %s", body)
}
`)
		}},
	{id: "30", what: "an answer's words taken out through the shapes a type shows: httputil.DumpResponse and NewRemoteText as function values, json.Unmarshal of StreamTo's bytes and oauth2call.UnmarshalData into a holder of the box, a box's UnmarshalJSON called directly, net/http.ReadResponse with no request, a response printed whole, and a package-level function of the box's package that takes an Answer", stage: 2,
		each: [][]string{
			{"answer census", plantDir + "control_reach.go", "controlDumpValue", "httputil.DumpResponse", "referred to other than by its call"},
			{"answer census", plantDir + "control_reach.go", "controlMakerValue", "redact.NewRemoteText", "referred to other than by its call"},
			{"answer census", plantDir + "control_reach.go", "controlUnmarshalHolder", "fills a *oauth2call.CodedRefusal, which holds the box"},
			{"answer census", plantDir + "control_reach.go", "controlUnmarshalDataHolder", "oauth2call.UnmarshalData fills a *oauth2call.CodedRefusal, which holds the box"},
			{"answer census", plantDir + "control_reach.go", "controlUnmarshalBox", "fills a redact.RemoteText, which holds the box"},
			{"answer census", plantDir + "control_reach.go", "controlNoRequest", "given no request"},
			{"answer census", plantDir + "control_reach.go", "controlWhole", "a response handed whole"},
			{"answer census", "AnswerBytes, Holds", "not exactly Holds"},
		},
		absent: []string{"controlConstant"},
		plant: func(c *controlRun) error {
			if err := c.write("packages/envoy/internal/redact/control_answer_bytes.go", `package redact

// AnswerBytes is control (30)'s planted function that hands an answer's bytes out, as a method
// would.
func AnswerBytes(a Answer) []byte { return a.bytes() }
`); err != nil {
				return err
			}
			return c.write(plantDir+"control_reach.go", `package cipherclient

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

// controlUnmarshalDataHolder fills a holder of the box through oauth2call.UnmarshalData.
func controlUnmarshalDataHolder(data []byte) error {
	var refusal oauth2call.CodedRefusal
	_ = oauth2call.UnmarshalData(data, &refusal)
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
`)
		}},
	{id: "31", what: "a read of an answer's body planted in a file only windows builds, in a new directory that holds no other file, for which go list names no package and no error", stage: 2,
		each: [][]string{
			{"UNKNOWN", windowsOnlyDir + "answer_windows.go", "no CI build compiles it", "imports net/http", "selects .Body"},
		},
		plant: func(c *controlRun) error {
			if err := os.MkdirAll(filepath.Join(c.wt, windowsOnlyDir), 0o755); err != nil {
				return err
			}
			return c.write(windowsOnlyDir+"answer_windows.go", `//go:build windows

package controlwindows

import (
	"fmt"
	"io"
	"net/http"
)

// controlWindowsOnlyBody is control (31)'s planted raw read in a directory no CI build lists.
func controlWindowsOnlyBody(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("the server answered %s", body)
}
`)
		}},
}

// windowsOnlyDir is control (31)'s new directory, whose one file only windows builds compile.
const windowsOnlyDir = plantDir + "controlwindows/"
