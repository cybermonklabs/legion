package redact

import (
	"bytes"
	"io"
)

// separator is what a write must hold for the rule to have a URL to find in it.
var separator = []byte("://")

// Writer is w with URLCredentials applied to each write: the text of a write reaches w with the
// query of every URL in it removed and an authority holding an "@" replaced, in one write of w. A
// write that holds no "://" reaches w as it is, with no allocation and no copy. Writer(nil) is nil,
// not a writer around nil, so a caller that reads a nil writer as no output at all (exec.Cmd, which
// gives a child the null device for one) reads Writer's answer the same way; and Writer of a writer
// Writer made is that writer.
//
// A write the rule changed answers len(p) once w has taken the text, however much longer or shorter
// the rule made it: io.Copy and log.Logger take a shorter count for a failed write
// (io.ErrShortWrite). When w fails it, Write answers 0 and w's error. A write the rule leaves as it
// is reaches w as it is, and Write answers w's own count and error.
//
// Each write is taken as a whole, and a URL is found only inside one write. Go's log.Logger and
// each fmt.Fprint* call write one record in one call, and the launcher's role log writes each line
// in one call, so none of them splits a URL across two writes, and the rule covers everything they
// write; the one exception is the role log's cut of a line longer than its bound, which the
// launcher's logLineCut states. It does not cover what a child process writes straight to a file
// descriptor it was given (exec.Cmd hands a child an *os.File as that file, never through a
// writer), nor a writer that exec's copy of a child's pipe feeds directly, whose reads can cut a
// URL in two.
func Writer(w io.Writer) io.Writer {
	switch w := w.(type) {
	case nil:
		return nil
	case urlWriter:
		return w
	}
	return urlWriter{out: w}
}

// urlWriter is Writer's answer: out, behind the rule.
type urlWriter struct{ out io.Writer }

func (w urlWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, separator) {
		return w.out.Write(p)
	}
	text := URLCredentials(string(p))
	// The rule can replace as well as remove, so only a text equal to the write is the write as it
	// was; a text of the same length can be a replaced authority.
	if text == string(p) {
		return w.out.Write(p)
	}
	if _, err := io.WriteString(w.out, text); err != nil {
		return 0, err
	}
	return len(p), nil
}
