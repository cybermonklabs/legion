//go:build ignore

// Setting-prints is the Go half of scripts/checks/setting-prints.ts, which runs it: the search for
// every line that prints a value read from a setting, by the value and not by its name.
//
//	go run scripts/checks/setting-prints/go/*.go <module directory> <file>...
//
// It reads the packages that hold the named files from source, type-checked together (every other
// package from the export data `go list -export` names), and follows each value read from the
// environment (os.Getenv, os.LookupEnv and those functions handed on as values, os.Environ, the
// settings table a constant names a variable of, os.Args, the flag package) or from a file
// (os.ReadFile, os.Open) through every variable, field, parameter, result, map value, slice,
// function value and composite literal it reaches, to every line that prints it: the message of an
// error (fmt.Errorf, errors.New), a log call (log/slog, log), fmt's printing functions, panic,
// http.Error and a child's command line (os/exec). A variable of a function holds what the
// assignments that can reach the read put there, not every assignment of it. A value of a type
// that cannot hold text ends its path, and so does every function of the redact package, the one
// place that decides what a line shows of a setting's URL. A function of a package it does not read
// hands on the text it is handed only when that package is one of keepsText's (url.Parse's,
// strconv's and pgx's errors quote their input); any such function hands on what a function value it
// is handed answers.
//
// A line it reports is where a setting as read is written into text; a print of text another line
// built is an end of that line. A setting can be a URL when its value reaches a URL taker (urlTakers,
// redact) as the URL or the head of its text, or a function of the module this program does not read.
// It prints one JSON line for each such line of every file of the packages it reads
// ({"finding": …}), one for each line of a setting that reaches no URL ({"quiet": …}), one with what
// its planted shapes gave when any was not as marked ({"blind": […]}), and a summary
// ({"summary": "…"}). The planted shapes are one more file of the first named file's package, read
// with it: a line marked `planted: found` must be found and one marked `planted: quiet` must not.
//
// Its files hold one concern each: loading the packages (load.go), the planted shapes
// (planted.go), the analysis's model (model.go), its rules of the language and the libraries
// (rules.go), the propagation (flow.go) and the records it prints (report.go).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/checks/setting-prints/go/*.go <module directory> <file>...")
		os.Exit(2)
	}
	root, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	a, files, typeErrors := load(root, os.Args[1], os.Args[2:])
	rounds, sinks := a.run(files)
	encoder := json.NewEncoder(os.Stdout)
	plantedFound, reported, quiet := a.writeFindings(encoder)
	blind := plantedBlind(plantedFound)
	if len(typeErrors) > 0 {
		blind = append(blind, "the type check of the module's packages failed: "+strings.Join(typeErrors, "; "))
	}
	a.summarize(encoder, blind, len(files), reported, quiet, sinks, rounds)
}
