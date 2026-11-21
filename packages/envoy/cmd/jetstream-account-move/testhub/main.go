// Command testhub runs, until it is stopped, a hub on one layout of the JetStream account move with
// a stand-in auth service (cmd/jetstream-account-move/internal/accounthub), for
// scripts/jetstream-account-probe.test.sh: it makes the notification stream and the bucket the
// probe's control publish names, and writes, into --dir, the hub's URL (url), the token of a
// listener (listener-token, natsgrant.Listener's grant) and of an identity the JetStream API is
// denied (session-token, natsgrant.Session's), and the seed of the operator of the account that
// holds JetStream (remover-seed: JETSTREAM's, or $G's on the merge base's layout), then ready.
//
//	testhub --testdata <infra/core/testdata> --layout merge-base|accounts|accounts-without-api-import --dir <dir>
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/cmd/jetstream-account-move/internal/accounthub"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "testhub:", err)
		os.Exit(1)
	}
}

func run() error {
	testdata := flag.String("testdata", "", "packages/envoy/infra/core/testdata")
	layoutName := flag.String("layout", "accounts", "merge-base, accounts or accounts-without-api-import")
	dir := flag.String("dir", "", "the directory the hub's URL and credentials are written to")
	flag.Parse()
	if *testdata == "" || *dir == "" {
		return fmt.Errorf("--testdata and --dir are required")
	}
	layout, place := accounthub.Accounts, accounthub.ClientAccount
	switch *layoutName {
	case "merge-base":
		layout, place = accounthub.MergeBase, accounthub.GlobalAccount
	case "accounts":
	case "accounts-without-api-import":
		layout = accounthub.AccountsWithoutAPIImport
	default:
		return fmt.Errorf("no layout %q", *layoutName)
	}
	keys, err := accounthub.NewKeys()
	if err != nil {
		return err
	}
	session, err := natsgrant.Session("s1")
	if err != nil {
		return err
	}
	grants := map[string]natsgrant.Permissions{"listener": natsgrant.Listener("m1"), "session": session}
	hub, err := accounthub.Start(*testdata, layout, keys, filepath.Join(*dir, "jetstream"), place, grants)
	if err != nil {
		return err
	}
	defer hub.Stop()

	operator, err := accounthub.ConnectSeed(hub.URL, keys.OperatorSeed)
	if err != nil {
		return fmt.Errorf("the operator: %w", err)
	}
	js, err := operator.JetStream()
	if err != nil {
		return err
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()}); err != nil {
		return err
	}
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "envoy_session_consumers"}); err != nil {
		return err
	}
	operator.Close()

	for name, content := range map[string]string{
		"url": hub.URL, "listener-token": "listener", "session-token": "session", "remover-seed": keys.OperatorSeed,
	} {
		if err := os.WriteFile(filepath.Join(*dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(*dir, "ready"), nil, 0o600); err != nil {
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	return nil
}
