//go:build ignore

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// plantedFile is one file of the planted shapes: the path it stands at, as one more file of the
// package the run names, and its text.
type plantedFile struct{ name, text string }

// plantedFiles are the planted shapes' files. A line marked `planted: <verdict>` must be listed with
// that verdict (judged, unjudged or wrapper), and a line marked neither must not be listed. Each
// blind spot of the program has its line: the forms a hand-kept list missed (ConnectWithOptions of
// pgx and of pgconn), a setting in a package whose other openers are judged (the plants stand in
// such a package, and only what the call passes judges it), a wrapper's caller, each parse of the
// formatter that does not guard the opener after it and each refusal by pgsetting.Refuse alone
// (dbOpenersPlantedGuards and dbOpenersPlantedGoto), a file no build context compiles
// (plantedUnbuilt), and a call one build judges and another does not (plantedValue and
// plantedValueOther define its argument's function apart). Each file is one that only a build tag
// builds, so a run that leaves such a file out reads none of them. Their order is the order the
// search reads them in: the first is read in the build of its tag, and that build compiles
// plantedValue too, while plantedValueOther needs a build of its own, which comes after it.
var plantedFiles = []plantedFile{
	{"scripts/checks/db-openers.planted.go", planted},
	{"scripts/checks/db-openers.planted_unbuilt.go", plantedUnbuilt},
	{"scripts/checks/db-openers.planted_value.go", plantedValue},
	{"scripts/checks/db-openers.planted_value_other.go", plantedValueOther},
}

const planted = `//go:build db_openers_planted

package main

import (
	"context"
	"database/sql"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/pgsetting"
)

const dbOpenersPlantedLocal = "postgres://127.0.0.1:5432/planted"

var dbOpenersPlantedGlobal = os.Getenv("DATABASE_URL")

type dbOpenersPlantedConfig struct{ url string }

func (c *dbOpenersPlantedConfig) reset() { c.url = os.Getenv("OTHER_DATABASE_URL") }

func dbOpenersPlantedOpen(ctx context.Context) {
	_, _ = pgx.Connect(ctx, os.Getenv("DATABASE_URL")) // planted: unjudged
	_, _ = pgx.ConnectWithOptions(ctx, os.Getenv("DATABASE_URL"), pgx.ParseConfigOptions{}) // planted: unjudged
	_, _ = pgconn.ConnectWithOptions(ctx, os.Getenv("DATABASE_URL"), pgconn.ParseConfigOptions{}) // planted: unjudged
	_, _ = pgconn.Connect(ctx, os.Getenv("DATABASE_URL")) // planted: unjudged
	_, _ = pgxpool.New(ctx, os.Getenv("DATABASE_URL")) // planted: unjudged
	_, _ = sql.Open("pgx", os.Getenv("DATABASE_URL")) // planted: unjudged
	parsed, _ := pgx.ParseConfig(os.Getenv("DATABASE_URL")) // planted: unjudged
	_, _ = pgx.ConnectConfig(ctx, parsed) // planted: judged
	_, _ = pgx.Connect(ctx, dbOpenersPlantedLocal) // planted: judged
	read, err := pgsetting.ParseConfig("DATABASE_URL", os.Getenv("DATABASE_URL"))
	if err == nil {
		_, _ = pgx.ConnectConfig(ctx, read) // planted: judged
	}
	field := struct{ url string }{os.Getenv("DATABASE_URL")}
	_, _ = pgxpool.ParseConfig(field.url) // planted: unjudged
	dbOpenersPlantedWrapper(ctx, os.Getenv("DATABASE_URL")) // planted: unjudged
	dbOpenersPlantedWrapper(ctx, dbOpenersPlantedLocal) // planted: judged
	open := pgxpool.New // planted: unjudged
	_ = open
	_, _ = pgx.Connect(ctx, os.Getenv("DB_OPENERS_PLANTED_A_SETTING_WHOSE_NAME_RUNS_PAST_THE_120_BYTES_A_LINE_OF_THE_SEARCH_PRINTS")+os.Getenv("DATABASE_URL")) // planted: unjudged
	_, _ = pgx.Connect(ctx, dbOpenersPlantedValue()) // planted: unjudged
}

func dbOpenersPlantedWrapper(ctx context.Context, url string) {
	_, _ = pgx.Connect(ctx, "postgres://"+url) // planted: wrapper
}

// dbOpenersPlantedGuards holds parses of the formatter, and refusals, before an opener. A parse
// (pgsetting.ParseConfig or ParsePoolConfig) guards the opener only when its error ends the
// function, it runs on every path to the opener, and the opener passes the very value it parsed,
// which no line writes between the two. pgsetting.Refuse alone guards nothing: a value it admits can
// still be one pgx cannot parse, whose parse error quotes the user part.
func dbOpenersPlantedGuards(ctx context.Context, check bool) error {
	other := os.Getenv("OTHER_DATABASE_URL")
	checked := os.Getenv("DATABASE_URL")
	if _, err := pgsetting.ParseConfig("DATABASE_URL", checked); err != nil {
		return err
	}
	_, _ = pgx.Connect(ctx, other) // planted: unjudged
	byIf := os.Getenv("DATABASE_URL")
	if _, err := pgsetting.ParseConfig("DATABASE_URL", byIf); err != nil {
		return err
	}
	_, _ = pgx.Connect(ctx, byIf) // planted: judged
	byErr := os.Getenv("DATABASE_URL")
	_, err := pgsetting.ParsePoolConfig("DATABASE_URL", byErr)
	if err != nil {
		return err
	}
	if check {
		_, _ = pgx.Connect(ctx, byErr) // planted: judged
	}
	refused := os.Getenv("DATABASE_URL")
	if err := pgsetting.Refuse("DATABASE_URL", refused); err != nil {
		return err
	}
	_, _ = pgx.Connect(ctx, refused) // planted: unjudged
	raw := os.Getenv("DATABASE_URL")
	if pgsetting.Refuse("DATABASE_URL", raw) != nil {
		return nil
	}
	_, _ = pgconn.ParseConfig(raw) // planted: unjudged
	settings := struct{ url, other string }{os.Getenv("DATABASE_URL"), os.Getenv("OTHER_DATABASE_URL")}
	if _, err := pgsetting.ParseConfig("DATABASE_URL", settings.url); err != nil {
		return err
	}
	settings.other = os.Getenv("OTHER_DATABASE_URL")
	_, _ = pgx.Connect(ctx, settings.url) // planted: judged
	dropped := os.Getenv("DATABASE_URL")
	_, _ = pgsetting.ParseConfig("DATABASE_URL", dropped)
	_, _ = pgx.Connect(ctx, dropped) // planted: unjudged
	logged := os.Getenv("DATABASE_URL")
	if _, err := pgsetting.ParseConfig("DATABASE_URL", logged); err != nil {
		_ = err.Error()
	}
	_, _ = pgx.Connect(ctx, logged) // planted: unjudged
	branch := os.Getenv("DATABASE_URL")
	if check {
		if _, err := pgsetting.ParseConfig("DATABASE_URL", branch); err != nil {
			return err
		}
	}
	_, _ = pgx.Connect(ctx, branch) // planted: unjudged
	again := os.Getenv("DATABASE_URL")
	if _, err := pgsetting.ParseConfig("DATABASE_URL", again); err != nil {
		return err
	}
	again = os.Getenv("OTHER_DATABASE_URL")
	_, _ = pgx.Connect(ctx, again) // planted: unjudged
	field := struct{ url string }{os.Getenv("DATABASE_URL")}
	if _, err := pgsetting.ParseConfig("DATABASE_URL", field.url); err != nil {
		return err
	}
	field = struct{ url string }{os.Getenv("OTHER_DATABASE_URL")}
	_, _ = pgx.Connect(ctx, field.url) // planted: unjudged
	looped := os.Getenv("DATABASE_URL")
	if _, err := pgsetting.ParseConfig("DATABASE_URL", looped); err != nil {
		return err
	}
	for range 2 {
		_, _ = pgx.Connect(ctx, looped) // planted: unjudged
		looped = os.Getenv("OTHER_DATABASE_URL")
	}
	aliased := os.Getenv("DATABASE_URL")
	alias := &aliased
	if _, err := pgsetting.ParseConfig("DATABASE_URL", aliased); err != nil {
		return err
	}
	*alias = os.Getenv("OTHER_DATABASE_URL")
	_, _ = pgx.Connect(ctx, aliased) // planted: unjudged
	closed := os.Getenv("DATABASE_URL")
	reread := func() { closed = os.Getenv("OTHER_DATABASE_URL") }
	if _, err := pgsetting.ParseConfig("DATABASE_URL", closed); err != nil {
		return err
	}
	reread()
	_, _ = pgx.Connect(ctx, closed) // planted: unjudged
	enclosed := os.Getenv("DATABASE_URL")
	if _, err := pgsetting.ParseConfig("DATABASE_URL", enclosed); err != nil {
		return err
	}
	later := func() {
		_, _ = pgx.Connect(ctx, enclosed) // planted: unjudged
	}
	later()
	if _, err := pgsetting.ParseConfig("DATABASE_URL", dbOpenersPlantedGlobal); err != nil {
		return err
	}
	_, _ = pgx.Connect(ctx, dbOpenersPlantedGlobal) // planted: unjudged
	method := dbOpenersPlantedConfig{os.Getenv("DATABASE_URL")}
	if _, err := pgsetting.ParseConfig("DATABASE_URL", method.url); err != nil {
		return err
	}
	method.reset()
	_, _ = pgx.Connect(ctx, method.url) // planted: unjudged
	pointed := &dbOpenersPlantedConfig{os.Getenv("DATABASE_URL")}
	if _, err := pgsetting.ParseConfig("DATABASE_URL", pointed.url); err != nil {
		return err
	}
	_, _ = pgx.Connect(ctx, pointed.url) // planted: unjudged
	var otherError error
	untested := os.Getenv("DATABASE_URL")
	_, untestedError := pgsetting.ParseConfig("DATABASE_URL", untested)
	if otherError == nil {
		_ = untestedError
	}
	_, _ = pgx.Connect(ctx, untested) // planted: unjudged
	another := os.Getenv("DATABASE_URL")
	_, anotherError := pgsetting.ParseConfig("DATABASE_URL", another)
	if otherError != nil {
		return otherError
	}
	_ = anotherError
	_, _ = pgx.Connect(ctx, another) // planted: unjudged
	unread := os.Getenv("DATABASE_URL")
	_, unreadError := pgsetting.ParseConfig("DATABASE_URL", unread)
	_, _ = pgx.Connect(ctx, unread) // planted: unjudged
	_ = unreadError
	late := os.Getenv("DATABASE_URL")
	_, _ = pgx.Connect(ctx, late) // planted: unjudged
	if _, err := pgsetting.ParseConfig("DATABASE_URL", late); err != nil {
		return err
	}
	return nil
}

// dbOpenersPlantedGoto holds a parse of the formatter that a goto jumps past.
func dbOpenersPlantedGoto(ctx context.Context, skip bool) error {
	jumped := os.Getenv("DATABASE_URL")
	if skip {
		goto open
	}
	if _, err := pgsetting.ParseConfig("DATABASE_URL", jumped); err != nil {
		return err
	}
open:
	_, _ = pgx.Connect(ctx, jumped) // planted: unjudged
	return nil
}
`

// plantedUnbuilt is a file no build context compiles (it needs a release of Go that does not
// exist), which the search reads in a build of its own with the host's files.
const plantedUnbuilt = `//go:build db_openers_planted && db_openers_planted_unbuilt && go1.999

package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5"
)

func dbOpenersPlantedUnbuilt(ctx context.Context) {
	_, _ = pgx.Connect(ctx, os.Getenv("DATABASE_URL")) // planted: unjudged
}
`

// plantedValue is dbOpenersPlantedValue in the build of the planted shapes' tag: a constant, so a
// call that passes its answer is judged there.
const plantedValue = `//go:build db_openers_planted && !db_openers_planted_other

package main

func dbOpenersPlantedValue() string { return "postgres://127.0.0.1:5432/planted" }
`

// plantedValueOther is dbOpenersPlantedValue in a build of its own, which compiles the planted
// shapes too: a setting, so the same call is unjudged there, and a call unjudged in any build is
// unjudged.
const plantedValueOther = `//go:build db_openers_planted && db_openers_planted_other

package main

import "os"

func dbOpenersPlantedValue() string { return os.Getenv("DATABASE_URL") }
`

var plantedMark = regexp.MustCompile(`// planted: (judged|unjudged|wrapper)$`)

// plantedBlind is what the planted shapes gave that was not as marked, given the verdict and the
// arguments of each planted line the run listed, by file and line. An argument is listed whole, as
// a judgement of the list names it: the text the line passes, which a ), a comma or the line's end
// follows (one planted line passes more than 120 bytes, the most a line of the search prints).
func plantedBlind(listed map[string]map[int]string, arguments map[string]map[int][]string) []string {
	var blind []string
	for _, f := range plantedFiles {
		for i, line := range strings.Split(f.text, "\n") {
			n := i + 1
			got, ok := listed[f.name][n]
			mark := plantedMark.FindStringSubmatch(line)
			switch {
			case mark != nil && !ok:
				blind = append(blind, fmt.Sprintf("the planted line %s:%d was not listed", f.name, n))
			case mark != nil && got != mark[1]:
				blind = append(blind, fmt.Sprintf("the planted line %s:%d was listed %s, not %s", f.name, n, got, mark[1]))
			case mark == nil && ok:
				blind = append(blind, fmt.Sprintf("the planted line %s:%d, which is not marked, was listed %s", f.name, n, got))
			}
			code, _, _ := strings.Cut(line, " // planted:")
			code = strings.Join(strings.Fields(code), " ")
			for _, argument := range arguments[f.name][n] {
				at := strings.Index(code, argument)
				if argument == "" || at < 0 || (at+len(argument) < len(code) && !strings.ContainsRune("),", rune(code[at+len(argument)]))) {
					blind = append(blind, fmt.Sprintf("the planted line %s:%d was listed with the argument %q, which is not the whole text it passes", f.name, n, argument))
				}
			}
		}
	}
	return blind
}
