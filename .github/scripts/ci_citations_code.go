// The code of each Go file that .github/scripts/ci_citations.py holds a citation token's name
// against: the file's text with every comment go/scanner finds removed, each comment's newlines
// kept so that a line of the code stays where it was, and every string, raw string and rune
// literal kept as it is written; and the file's identifiers, which an identifier the token names
// must be one of, so that no string or comment holds one.
//
// Usage: GOWORK=off go run .github/scripts/ci_citations_code.go < paths
// GOWORK=off, as the file belongs to no module: it imports the standard library alone. The paths
// come on stdin, each ended by a NUL byte, since go run reads every argument that ends in .go as
// a file to compile. For each path, in the order given, it prints the code and then the
// identifiers, one a line, each followed by a NUL byte. A path that cannot be read or scanned is
// named on stderr, and the exit status is then 1.
package main

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
)

// codeOf is src without its comments, and its identifiers one a line. A comment's end is read
// from src rather than from the literal the scanner returns, which has its carriage returns
// removed.
func codeOf(path string, src []byte) ([]byte, []byte, error) {
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(src))
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(file, src, errs.Add, scanner.ScanComments)
	code := make([]byte, 0, len(src))
	var identifiers []byte
	seen := map[string]bool{}
	last := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT && !seen[lit] {
			seen[lit] = true
			identifiers = append(append(identifiers, lit...), '\n')
		}
		if tok != token.COMMENT {
			continue
		}
		start := file.Offset(pos)
		end := len(src)
		if lit[1] == '/' {
			if i := bytes.IndexByte(src[start:], '\n'); i >= 0 {
				end = start + i
			}
		} else if i := bytes.Index(src[start+2:], []byte("*/")); i >= 0 {
			end = start + 2 + i + 2
		}
		code = append(code, src[last:start]...)
		code = append(code, bytes.Repeat([]byte{'\n'}, bytes.Count(src[start:end], []byte{'\n'}))...)
		last = end
	}
	if err := errs.Err(); err != nil {
		return nil, nil, err
	}
	return append(code, src[last:]...), bytes.TrimSuffix(identifiers, []byte{'\n'}), nil
}

func main() {
	paths, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := false
	var out bytes.Buffer
	for _, path := range bytes.FieldsFunc(paths, func(r rune) bool { return r == 0 }) {
		src, err := os.ReadFile(string(path))
		var code, identifiers []byte
		if err == nil {
			code, identifiers, err = codeOf(string(path), src)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = true
			continue
		}
		out.Write(code)
		out.WriteByte(0)
		out.Write(identifiers)
		out.WriteByte(0)
	}
	if _, err := os.Stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}
