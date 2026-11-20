//go:build ignore

// Setting-prints is the Go half of scripts/checks/setting-prints.ts, which runs it: the search for
// every line that prints a value read from a setting, by the value and not by its name.
//
//	go run scripts/checks/setting-prints/go/*.go <module directory>
//
// It reads every package of the module from source, type-checked together (every package outside
// the module from the export data `go list -export` names), and follows settings through each
// command apart, the command with every package of the module it imports, and once more through
// the packages no command imports: a setting reaches a print only inside one process. It follows
// each value read from the environment (os.Getenv, os.LookupEnv and those functions handed on as
// values, os.Environ, the
// settings table a constant names a variable of, os.Args, the flag package) or from a file
// (os.ReadFile, os.Open) through every variable, field, parameter, result, map value, slice,
// function value and composite literal it reaches, to every line that prints it: the message of an
// error (fmt.Errorf, errors.New), a log call (log/slog, log), fmt's printing functions, panic,
// http.Error and a child's command line (os/exec). A variable of a function holds what the
// assignments that can reach the read put there, not every assignment of it. A function's answer
// is read for each call from what that call hands it, as the TypeScript half reads it (analysis):
// a line that writes a parameter into the answer is in a call's answer only where that call hands
// the parameter a setting, and a line that writes or prints a parameter names what every call of
// the unit hands it. A value of a type that cannot hold text ends its path, and so does every
// function of the redact package, the one place that decides what a line shows of a setting's URL.
// A call of a method of an interface the module declares runs each implementation the unit
// declares, and hands on the text of its arguments for an implementation outside the module, but
// never its receiver's: an interface holds a struct whose fields are read apart. A function outside
// the module hands on the text it is handed only when its package is one of keepsText's
// (url.Parse's, strconv's and pgx's errors quote their input); any such function hands on what a
// function value it is handed answers. A package of the module that go list or the type check
// refuses is named as what the run could not see, so the run never passes over it.
//
// A line it reports is where a setting as read is written into text; a print of text another line
// built is an end of that line. A setting can be a URL when its value reaches a URL taker (urlTakers,
// redact) as the URL or the head of its text, or any text of it reaches a method of an interface
// the module declares.
// It prints one JSON line for each such line of every file of the module ({"finding": …}), one for
// each line of a setting that reaches no URL ({"quiet": …}), one with what its planted shapes gave
// when any was not as marked and the packages it could not load ({"blind": […]}), and a summary
// ({"summary": "…"}). The planted shapes are files of their own packages (plants), read with them:
// a line marked `planted: found` must be found and one marked `planted: quiet` must not.
//
// Its files hold one concern each: loading the packages (load.go), the planted shapes
// (planted.go), the analysis's model (model.go), its rules of the language and the libraries
// (rules.go), the propagation (flow.go) and the records it prints (report.go).
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/checks/setting-prints/go/*.go <module directory>")
		os.Exit(2)
	}
	root, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	a, packages, refused := load(root, os.Args[1])
	var all []*ast.File
	for _, p := range packages {
		all = append(all, p.files...)
	}
	found := &lines{byLine: map[string]*finding{}, urlish: map[string]bool{}}
	_, sets := units(packages)
	rounds := 0
	for _, set := range sets {
		var files []*ast.File
		for _, p := range set {
			files = append(files, p.files...)
		}
		rounds = max(rounds, a.run(files))
		a.gather(found)
	}
	encoder := json.NewEncoder(os.Stdout)
	plantedFound, reported, quiet := found.write(encoder)
	blind := append(plantedBlind(plantedFound), refused...)
	a.summarize(encoder, blind, len(all), len(sets), reported, quiet, a.sinks(all), rounds)
}
