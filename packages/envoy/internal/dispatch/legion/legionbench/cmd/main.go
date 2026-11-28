// Command legionbench is the loader, the daemon stand-in and the drive of the two Jobs of
// dispatch://INFRA-5577 that measure on the build farm: R8's (k14, r8.sh) and R7's (k27, r7.sh).
//
//	legionbench load    -database <url> -state-url <stand-in> [-seed 5577] [-scale 1] [-ten-times] [-now <RFC 3339>] [-first-read]
//	legionbench standin -database <url> -listen 127.0.0.1:13370 [-cards 1000]
//	legionbench drive   -arm main|head -url <server> -token-file <file> -database <url> -probe-dir <packages/dispatch> \
//	                    -server-log <file> -out <dir> [-facts -pg-log <file>] [-rate 0.26] [-duration 10m] [-wait-timeout 20m]
//	legionbench compare -main <main's summary> -head <head's summary>
//
// load writes INFRA's history (package legionbench) into a database the server has migrated and
// nothing has written, and prints the rows it then holds, one table a line. Every time is relative
// to -now, the current UTC midnight by default, so two loads with one -now and one -seed hold the
// same history at the same times. -scale multiplies every count (0.05 loads in a pod in a few
// minutes); -ten-times writes ten times the stated run rate (R8's load); -first-read holds the
// load to R7's: no issue_facts row and no build in issue_facts_computed, so the server that boots
// on it reads INFRA's facts whole.
//
// standin serves INFRA's Legion daemon to the server's Legion state intake, which reads it whole as
// it starts (legionbench.StandIn), with -cards cards of the loaded issues, until it is stopped. It
// prints one STANDIN line once it serves.
//
// drive is one arm of R8 against a running server (drive.go); compare judges the two arms' median
// recompute times against R8's bound (compare.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legionbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const usage = "usage: legionbench load|standin|drive|compare [flags]; legionbench <subcommand> -h lists its flags"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	command, args := os.Args[1], os.Args[2:]
	var err error
	switch command {
	case "load":
		err = load(args)
	case "standin":
		err = standin(args)
	case "drive":
		err = drive(args)
	case "compare":
		err = compare(args)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "legionbench %s: %v\n", command, err)
		os.Exit(1)
	}
}

// load is the load subcommand (the package comment).
func load(args []string) error {
	flags := flag.NewFlagSet("load", flag.ExitOnError)
	var spec legionbench.Spec
	var database, now string
	flags.StringVar(&database, "database", "", "the Postgres URL to write into (required); the server must have migrated it")
	flags.StringVar(&spec.StateURL, "state-url", "", "the daemon INFRA's legion_settings row names (required): the stand-in's URL")
	flags.Uint64Var(&spec.Seed, "seed", 5577, "the seed of every random draw")
	flags.Float64Var(&spec.Scale, "scale", 1, "the share of INFRA's stated rate to write, above 0 and at most 1")
	flags.BoolVar(&spec.TenTimes, "ten-times", false, "write ten times the stated run rate")
	flags.BoolVar(&spec.FirstRead, "first-read", false, "leave issue_facts empty and no build in issue_facts_computed, and refuse to finish otherwise")
	flags.StringVar(&now, "now", "", "the moment every history ends at, RFC 3339; the current UTC midnight by default")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if database == "" {
		return errors.New("name the database with -database")
	}
	spec.Now = time.Now().UTC().Truncate(24 * time.Hour)
	if now != "" {
		at, err := time.Parse(time.RFC3339, now)
		if err != nil {
			return fmt.Errorf("read -now %q as a moment: %w", now, err)
		}
		spec.Now = at.UTC()
	}
	ctx := context.Background()
	opened, err := store.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer opened.Pool.Close()
	started := time.Now()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "legionbench: "+format+"\n", args...)
	}
	logf("loading seed %d at scale %g (ten times the run rate: %t, first read: %t), ending at %s, INFRA's daemon at %s",
		spec.Seed, spec.Scale, spec.TenTimes, spec.FirstRead, spec.Now.Format(time.RFC3339), spec.StateURL)
	counts, err := legionbench.Load(ctx, opened, spec, logf)
	if err != nil {
		return err
	}
	for _, line := range counts.Lines() {
		fmt.Printf("RESULT load %s\n", line)
	}
	fmt.Printf("RESULT load seconds=%.0f\n", time.Since(started).Seconds())
	return nil
}

// standin is the standin subcommand (the package comment): it reads the state once, serves it
// until SIGTERM or SIGINT, and prints one STANDIN line once it serves.
func standin(args []string) error {
	flags := flag.NewFlagSet("standin", flag.ExitOnError)
	var database, listen, project string
	var cards int
	flags.StringVar(&database, "database", "", "the loaded database (required)")
	flags.StringVar(&listen, "listen", "127.0.0.1:13370", "the address to serve on")
	flags.StringVar(&project, "project", legionbench.Project, "the project whose daemon the stand-in is")
	flags.IntVar(&cards, "cards", legionbench.DefaultCards, "how many cards the state puts on the board")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if database == "" {
		return errors.New("name the database with -database")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	opened, err := store.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	state, err := legionbench.StandInState(ctx, opened.Pool, project, cards)
	opened.Pool.Close()
	if err != nil {
		return err
	}
	handler, err := legionbench.StandIn(state)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	slots := len(state.Admission.Active)
	fmt.Printf("STANDIN serving %s's state at http://%s: %d cards, %d holding a slot, %d waiting, %d trees\n",
		project, listener.Addr(), len(state.Issues), slots, len(state.Admission.Waiting), len(state.Admission.Trees))
	if len(state.Issues) < cards {
		fmt.Printf("STANDIN the loaded history holds %d open issues with a working phase, fewer than the %d cards asked for\n",
			len(state.Issues), cards)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
