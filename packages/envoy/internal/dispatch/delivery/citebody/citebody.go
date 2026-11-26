// Package citebody holds a pull request body that delivery's citation reading reaches only
// through Body's methods, each of which counts the bytes it hands out. The text is unexported, so
// no code outside this package can read a byte of it without a count, short of reflect or unsafe:
// a scan back from an issue path has to ask Span for the bytes it scans, and delivery's cost rows
// hold the counts to a bound that such a scan cannot meet. What a reader does with bytes it was
// handed is not counted.
package citebody

import "regexp"

// Reads is what a Body has read: Passes whole passes over the body (Find), and Spans spans
// handed out (Span), Handed bytes in all, the Longest of them.
type Reads struct {
	Passes, Spans, Handed, Longest int
}

// Body is a body read only through counted methods.
type Body struct {
	text  string
	reads *Reads
}

// New returns text as a Body that counts what it reads into reads. A nil reads counts nothing,
// which is what production passes.
func New(text string, reads *Reads) Body {
	return Body{text: text, reads: reads}
}

// Find is every match of re in the body, with its submatches, as
// regexp.Regexp.FindAllStringSubmatchIndex answers, and counts one pass over the body.
func (b Body) Find(re *regexp.Regexp) [][]int {
	if b.reads != nil {
		b.reads.Passes++
	}
	return re.FindAllStringSubmatchIndex(b.text, -1)
}

// Span is the body's bytes from from to to, and counts them as one span.
func (b Body) Span(from, to int) string {
	if b.reads != nil {
		b.reads.Spans++
		b.reads.Handed += to - from
		b.reads.Longest = max(b.reads.Longest, to-from)
	}
	return b.text[from:to]
}
