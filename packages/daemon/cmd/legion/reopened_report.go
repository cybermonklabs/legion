package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/cipherclient"
	"github.com/spf13/cobra"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/reopenreport"
)

// newReopenedReportCommand is `legion reopened-report`, the controller's read of the requests it
// made for new work on merged issues (D17): for each issue --issue names, the row
// reopenreport.Read reads from Dispatch with the controller's own Dispatch credential, which
// --config's controller.yaml names. It writes nothing anywhere. A call with no --config, no
// --issue, an --issue that is no issue key or a positional word is a usage error, and asks
// Dispatch nothing.
func newReopenedReportCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reopened-report --config <controller.yaml> --issue <KEY>...",
		Short: "report, for each issue the controller reopened, whether its owner wrote checks or withdrew the request before the change ended",
		Args:  noArguments,
	}
	configPath := cmd.Flags().String("config", "", "path to the operator-side controller.yaml, whose Dispatch credential the report reads with (required)")
	issues := cmd.Flags().StringArray("issue", nil, "an issue the controller reopened (repeat it for each issue, at least one)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if *configPath == "" {
			return refuse(cmd, "--config is required")
		}
		if len(*issues) == 0 {
			return refuse(cmd, "--issue is required: name each issue the controller reopened")
		}
		for _, key := range *issues {
			if !legionclaim.IsIssueKey(key) {
				return refuse(cmd, "--issue %q is no issue key", key)
			}
		}
		return runReopenedReport(ctx, *configPath, *issues, os.Getenv, stdout, stderr)
	})
	return cmd
}

// runReopenedReport reads each issue's row and prints the report (writeReopenedReport). It exits
// 1 where a row could not be read, and 0 where every row could, a pending row among them. A file
// it cannot read, or one that names no Dispatch credential it can use, reads nothing: the refusal
// goes to stderr through the URL rule (printRefusal), and the report prints its unknown line with
// every total UNKNOWN, so no total reads as a count of zero. getenv is the environment the broker
// route reads this machine's identity from (reopenedReportReader).
func runReopenedReport(ctx context.Context, configPath string, issues []string, getenv func(string) string, stdout, stderr io.Writer) int {
	reader, err := reopenedReportReader(configPath, getenv)
	if err != nil {
		printRefusal(stderr, "legion reopened-report", err)
		writeUnknownReopenedReport(stdout, "config")
		return 1
	}
	rows := make([]reopenreport.Row, 0, len(issues))
	for _, key := range issues {
		rows = append(rows, reopenreport.Read(ctx, reader, key))
	}
	writeReopenedReport(stdout, rows)
	for _, row := range rows {
		if row.Category == reopenreport.NotAssessable {
			return 1
		}
	}
	return 0
}

// reopenedReportReader is the Dispatch client of the controller's file at configPath, read as
// `legion controller start` reads it (controllerStart): the strict operator-side loader, and then
// exactly one credential, the bearer `dispatch_token_file` points at, or this machine's own
// identity traded at the secrets broker (`dispatch_token_broker`, reopenedReportBroker). A file
// with no `dispatch_url`, or one that names neither credential, is refused: the report reads only
// Dispatch.
func reopenedReportReader(configPath string, getenv func(string) string) (*dispatch.HTTPClient, error) {
	absolute, err := filepath.EvalSymlinks(configPath)
	if err == nil {
		absolute, err = filepath.Abs(absolute)
	}
	if err != nil {
		return nil, fmt.Errorf("controller configuration %s could not be read: %w", configPath, err)
	}
	cfg, err := config.LoadController(absolute, "")
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.DispatchURL == "":
		return nil, errors.New("the controller configuration names no dispatch_url: the report reads each issue's Dispatch log")
	case cfg.DispatchTokenBroker:
		client, err := reopenedReportBroker(getenv)
		if err != nil {
			return nil, err
		}
		return dispatch.New(cfg.DispatchURL, cipherclient.NewTokenTransport(client, http.DefaultTransport)), nil
	case cfg.DispatchTokenFile != "":
		token, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile)
		if err != nil {
			return nil, err
		}
		return dispatch.New(cfg.DispatchURL, dispatch.BearerTransport(token, nil)), nil
	}
	return nil, errors.New("dispatch_url is configured with no Dispatch credential: set dispatch_token_file, or dispatch_token_broker: true where this machine proves an identity of its own")
}

// reopenedReportBroker is the broker client of the controller's `dispatch_token_broker` route, made
// as controllerDispatchBroker makes the start's: this machine's identity (getenv names it), the
// broker AGENT_SECRETS_URL names, a key of this run's own, and a session name of its own, new on
// every run, so the trade takes nothing from the controller's session. It declines the launcher
// credential, since the run enrols nothing. Its token transport trades at the first request and
// signs each request's proof (cipherclient.NewTokenTransport).
func reopenedReportBroker(getenv func(string) string) (*cipherclient.Client, error) {
	identity, err := cipherclient.MachineIdentity(getenv)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, fmt.Errorf("dispatch_token_broker is set, so the report trades this machine's own identity, and %s names none", cipherclient.IdentityVar)
	}
	broker := cipheraddr.New(getenv(cipherclient.BrokerURLVar))
	if broker.Empty() {
		return nil, fmt.Errorf("%s names the identity this machine proves, so %s must name the secrets broker it proves it to",
			cipherclient.IdentityVar, cipherclient.BrokerURLVar)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("make the key the report's trade is proved with: %w", err)
	}
	return cipherclient.New(cipherclient.Config{
		BrokerURL: broker, Key: key, Subject: identity, Session: "legion-reopened-report-" + uuid.NewString(),
		NoLauncherCredential: true, HTTP: cipherclient.HTTPClient(),
	})
}

// writeReopenedReport prints rows, one fact a line, each line its name, then the issue, then its
// values: `row` and the category; `start`, the accepted todo's seq= and at=; `change`, the
// generation; `end`, `merged` with pull_request= and at=, `no_pull_request` with at=, or `open`
// for a pending row; `version`, each version in the interval, with number=, actor=, at= and
// checks=, each check block it changed as id:change, or none; `withdrawal`, with request=, at=
// and the quoted reason=; and `unread`, each read that failed, quoted. A value it did not read is
// UNKNOWN. Then `total` for each category, with its count and its issues, and `assessed`, the
// count of ended rows that could be read, which the count of instruction_not_followed is over.
func writeReopenedReport(w io.Writer, rows []reopenreport.Row) {
	for _, row := range rows {
		writeReopenedRow(w, row)
	}
	assessed := 0
	for _, total := range reopenreport.Totals(rows) {
		fmt.Fprintf(w, "total %s %d", total.Category, len(total.Issues))
		for _, key := range total.Issues {
			fmt.Fprintf(w, " %s", key)
		}
		fmt.Fprintln(w)
		if total.Category.Assessed() {
			assessed += len(total.Issues)
		}
	}
	fmt.Fprintf(w, "assessed %d\n", assessed)
}

// writeReopenedRow prints one row's lines (writeReopenedReport). What it read failing quotes
// Dispatch's own words, which can name Dispatch's URL, so each passes the URL rule.
func writeReopenedRow(w io.Writer, row reopenreport.Row) {
	key := row.Issue
	fmt.Fprintf(w, "row %s %s\n", key, row.Category)
	if row.Start != nil {
		fmt.Fprintf(w, "start %s seq=%d at=%s\n", key, row.Start.Seq, reportTime(row.Start.At))
	} else {
		fmt.Fprintf(w, "start %s %s\n", key, unknownCount)
	}
	if row.Change != 0 {
		fmt.Fprintf(w, "change %s %d\n", key, row.Change)
	} else {
		fmt.Fprintf(w, "change %s %s\n", key, unknownCount)
	}
	switch {
	case row.End != nil && row.End.PullRequest != 0:
		fmt.Fprintf(w, "end %s merged pull_request=%d at=%s\n", key, row.End.PullRequest, reportTime(row.End.At))
	case row.End != nil:
		fmt.Fprintf(w, "end %s no_pull_request at=%s\n", key, reportTime(row.End.At))
	case row.Category == reopenreport.Pending:
		fmt.Fprintf(w, "end %s open\n", key)
	default:
		fmt.Fprintf(w, "end %s %s\n", key, unknownCount)
	}
	for _, version := range row.Versions {
		checks := make([]string, 0, len(version.Checks))
		for _, check := range version.Checks {
			checks = append(checks, check.ID+":"+check.Change)
		}
		changed := strings.Join(checks, ",")
		if changed == "" {
			changed = "none"
		}
		fmt.Fprintf(w, "version %s number=%d actor=%s at=%s checks=%s\n", key, version.Number, version.Actor.Name(), reportTime(version.At), changed)
	}
	if withdrawal := row.Withdrawal; withdrawal != nil {
		fmt.Fprintf(w, "withdrawal %s request=%d at=%s reason=%s\n", key, withdrawal.Request, reportTime(withdrawal.At), strconv.Quote(withdrawal.Reason))
	}
	for _, unread := range row.Unread {
		fmt.Fprintf(w, "unread %s %s\n", key, strconv.Quote(redact.URLCredentials(unread)))
	}
}

// writeUnknownReopenedReport is the report of a run that read nothing: its unknown line with the
// code of what kept it from reading, every total UNKNOWN, and assessed UNKNOWN.
func writeUnknownReopenedReport(w io.Writer, code string) {
	fmt.Fprintf(w, "unknown %s\n", code)
	for _, category := range reopenreport.Categories {
		fmt.Fprintf(w, "total %s %s\n", category, unknownCount)
	}
	fmt.Fprintf(w, "assessed %s\n", unknownCount)
}

// reportTime is a moment as the report prints it: UTC, RFC 3339, with the fraction of a second
// Dispatch gave and none where it gave none.
func reportTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}
