package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// cutoverReadConnect bounds the connection a cutover read makes.
const cutoverReadConnect = 15 * time.Second

// newCutoverReadCommand is `legion cutover-read drain|filters --projects <KEY,…>`, plan C's two
// readings around the deploy that drops the shared wake filter (internal/intake.CutoverReader). It
// connects with NATS_URLS and the nkey seed natsauth.Seed reads (NATS_NKEY_SEED_FILE or
// NATS_NKEY_SEED), the variables the in-cluster listener's pod carries, so a Job that copies the
// listener's identity runs it under the listener's grant. It prints a line per project and the
// verdict alone on the last line, and exits 0 only on GATE OPEN, 1 on GATE SHUT and 3 on UNKNOWN.
func newCutoverReadCommand(stdout io.Writer) *cobra.Command {
	var modes []*cobra.Command
	for _, mode := range []struct {
		name, short string
		read        func(*intake.CutoverReader, context.Context, []string) intake.CutoverReading
	}{
		{"drain", "step 3: nothing but listener_ready unread on the shared wake subject past each wake durable's ack floor", (*intake.CutoverReader).Drain},
		{"filters", "after deploy 4: every daemon durable on exactly its kind's filters", (*intake.CutoverReader).Filters},
	} {
		cmd := &cobra.Command{Use: mode.name + " --projects <KEY,…>", Short: mode.short, Args: noArguments}
		var projects string
		cmd.Flags().StringVar(&projects, "projects", "", "the deployment's daemon projects, comma-separated")
		read := mode.read
		cmd.RunE = runs(func(ctx context.Context, _ []string) int {
			unknown := func(format string, args ...any) int {
				fmt.Fprintf(stdout, "UNKNOWN: "+format+"\n", args...)
				fmt.Fprintln(stdout, intake.GateUnknown)
				return 3
			}
			var named []string
			for project := range strings.SplitSeq(projects, ",") {
				if project = strings.TrimSpace(project); project != "" {
					named = append(named, project)
				}
			}
			urls := strings.Fields(strings.ReplaceAll(os.Getenv("NATS_URLS"), ",", " "))
			if len(urls) == 0 {
				return unknown("NATS_URLS names no server")
			}
			seed, err := natsauth.Seed("", os.LookupEnv)
			if err != nil {
				return unknown("%v", err)
			}
			conn, err := natsauth.Connect(urls, seed, "legion cutover-read", nats.Timeout(cutoverReadConnect))
			if err != nil {
				return unknown("connect to NATS: %v", err)
			}
			defer conn.Close()
			js, err := jetstream.New(conn)
			if err != nil {
				return unknown("open JetStream: %v", err)
			}
			reading := read(intake.NewCutoverReader(js), ctx, named)
			for _, line := range reading.Lines {
				fmt.Fprintln(stdout, line)
			}
			fmt.Fprintln(stdout, reading.Verdict)
			switch reading.Verdict {
			case intake.GateOpen:
				return 0
			case intake.GateShut:
				return 1
			}
			return 3
		})
		modes = append(modes, cmd)
	}
	return group(&cobra.Command{
		Use:    "cutover-read",
		Short:  "read the notification stream around the wake migration's last deploy (plan C's drain and filter reads)",
		Hidden: true,
	}, modes...)
}
