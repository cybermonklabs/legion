package main

import "io"

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
