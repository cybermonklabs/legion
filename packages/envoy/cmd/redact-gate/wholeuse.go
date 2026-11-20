package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// wholeUseDir holds the whole-value census's two listing programs: main.go, over a Go module's
// production expressions typed oauth2.RetrieveError or *RetrieveError, and whole-use.ts, over the
// TypeScript workspace's expressions typed as one of openid-client's error classes. Each lists how
// every such value is used and fails a whole use; their doc comments say how they classify.
const wholeUseDir = "packages/envoy/internal/redact/testdata/wholeuse"

// wholeUse is the whole-value census: it runs both listings on the snapshot at dir, the Go one
// over each Go module the call census reads, through the snapshot's go.work, and the TypeScript
// one from the snapshot's root over every package's tsconfig.json. Each use they list is a
// finding, OK where the listing passed it. A BLIND line, an untyped value the TypeScript listing
// cannot judge, is a finding of kind blind that passes and is reported. A listing that did not
// build or load, read no production file, or printed no summary is UNKNOWN.
func (g *gate) wholeUse(dir string) census {
	var c census
	bin := filepath.Join(g.work, "wholeuse")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join(dir, wholeUseDir)
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("whole-value census: the Go listing does not build: %v: %s", err, lastLine(out)))
		return c
	}
	mods, err := goModules(dir)
	if err != nil || len(mods) == 0 {
		c.Unknown = append(c.Unknown, fmt.Sprintf("whole-value census: the Go modules cannot be listed: %v", err))
		return c
	}
	roots, _ := workRoots(dir)
	for _, mod := range mods {
		list := exec.Command(bin, "-dir", ".", "./...")
		list.Dir = filepath.Join(dir, mod)
		// Each module loads as its own build loads it (moduleWork).
		list.Env = append(os.Environ(), "GOWORK="+moduleWork(dir, mod, roots))
		out, _ := list.CombinedOutput()
		c.add(out, mod, "Go module "+mod)
	}
	ts := exec.Command("bun", filepath.Join(wholeUseDir, "whole-use.ts"))
	ts.Dir = dir
	out, _ := ts.CombinedOutput()
	c.add(out, "", "the TypeScript workspace")
	return c
}

// add reads one listing's output: its findings, each file relative to the snapshot's root (prefix,
// the module's directory, joined to the listing's own relative path), and UNKNOWN where the
// listing read nothing or printed no summary.
func (c *census) add(out []byte, prefix, what string) {
	summary := ""
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		switch fields[0] {
		case "SUMMARY":
			summary = scanner.Text()
		case "ok", "FAIL":
			if len(fields) < 5 {
				c.Unknown = append(c.Unknown, fmt.Sprintf("whole-value census: %s: a line it cannot read: %q", what, scanner.Text()))
				continue
			}
			file, line := position(prefix, fields[1])
			c.Whole = append(c.Whole, finding{File: file, Line: line, Function: fields[2], What: fields[4], Kind: "whole", OK: fields[0] == "ok", Why: fields[3]})
		case "BLIND":
			if len(fields) < 4 {
				c.Unknown = append(c.Unknown, fmt.Sprintf("whole-value census: %s: a line it cannot read: %q", what, scanner.Text()))
				continue
			}
			file, line := position(prefix, fields[1])
			c.Whole = append(c.Whole, finding{File: file, Line: line, What: fields[3], Kind: "blind", OK: true, Why: fields[2]})
		}
	}
	switch {
	case summary == "":
		c.Unknown = append(c.Unknown, fmt.Sprintf("whole-value census: %s: the listing printed no summary: %s", what, lastLine(out)))
	case strings.Contains(summary, "production files=0\t") || strings.HasSuffix(summary, "production files=0"):
		c.Unknown = append(c.Unknown, fmt.Sprintf("whole-value census: %s: the listing read no production file", what))
	}
}

// position splits "file:line:column" and joins file to prefix.
func position(prefix, at string) (string, int) {
	parts := strings.Split(at, ":")
	if len(parts) < 3 {
		return at, 0
	}
	line, _ := strconv.Atoi(parts[len(parts)-2])
	file := strings.Join(parts[:len(parts)-2], ":")
	if prefix != "" {
		file = filepath.ToSlash(filepath.Join(prefix, file))
	}
	return file, line
}

// lastLine is the last non-empty line of out, for an UNKNOWN's reason.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}
