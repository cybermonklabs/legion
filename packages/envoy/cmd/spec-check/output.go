package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// output is the standard output of a subcommand, which is its product: the lines that a caller
// reads, the JSON of checks and the blocks of print, which a change copies into the files. It
// passes each write to w and keeps the first error, or io.ErrShortWrite for a write that w took
// in part, and takes no write after it, so run reports a failed write once, whichever line failed.
type output struct {
	w   io.Writer
	err error
}

func (o *output) Write(p []byte) (int, error) {
	if o.err != nil {
		return 0, o.err
	}
	n, err := o.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	o.err = err
	return n, err
}

// printJSON writes value as one line of JSON on stdout and answers 0, or 2 with the error on
// stderr, after dir, when encoding/json cannot write it. It is the one writer of the JSON of checks
// and of state. The line does not pass through runnerlog.Say, whose prefix and escapes would make
// it no JSON. encoding/json writes each control character of a value as an escape, so no line
// break of a value reaches the log, and the line starts with "[" or "{", so it starts no workflow
// command; the "[" of each "##[", which can only be in a value, is written as \u005b, which a JSON
// reader reads back as "[".
func printJSON(stdout, stderr io.Writer, dir string, value any) int {
	line, err := json.Marshal(value)
	if err != nil {
		say(stderr, "%s: %s", dir, err)
		return 2
	}
	fmt.Fprintln(stdout, strings.ReplaceAll(string(line), "##[", `##\u005b`))
	return 0
}
