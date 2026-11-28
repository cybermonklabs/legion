package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// newMembersBackCommand is `legion members-back --config <legion.yaml>`: each member that the
// project's first boot on the release that stores no tree brings back, read from the daemon's
// store in one read-only transaction (daemon.ListMembersBack, over the one reading the conversion
// makes, record.ReadMembersBack). The operator runs it before the pin that carries that release is
// applied, on the store of the release before, which it reads as that release left it, and sets
// any member to keep stopped to backlog before the boot. It changes nothing.
//
// It reads its store as `legion start` reads it: the configuration's postgres_dsn, else
// LEGION_POSTGRES_DSN in the environment (config.LoadForValidation, which runs no App key command).
// It asks Dispatch nothing and calls no daemon API: the release before's API shows a claim's
// suspension source only to the operator token. A refusal passes the URL rule on its way to stderr
// (printRefusal), so it prints no password and no query of the DSN.
func newMembersBackCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members-back",
		Short: "list the members the first boot of the release that stores no tree brings back, read from the daemon's store, and change nothing",
		Args:  configOnly,
	}
	configPath := cmd.Flags().String("config", defaultConfigPath, "path to legion.yaml")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int { return runMembersBack(ctx, *configPath, stdout, stderr) })
	return cmd
}

func runMembersBack(ctx context.Context, configPath string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadForValidation(configPath, nil)
	if err != nil {
		printRefusal(stderr, "legion members-back", err)
		return 1
	}
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		printRefusal(stderr, "legion members-back", err)
		return 1
	}
	defer st.Close()
	var listing daemon.MembersBackListing
	if err := store.TransactWith(ctx, st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		listing, err = daemon.ListMembersBack(ctx, tx, cfg.Project)
		return err
	}); err != nil {
		printRefusal(stderr, "legion members-back", err)
		return 1
	}
	printMembersBack(stdout, listing)
	return 0
}

// printMembersBack writes listing: one line that names the project and what its first boot brings
// back, then one line for each member, its key, the phase it starts again in and how its tree left
// Legion's work. It names no claim, session or token.
func printMembersBack(w io.Writer, listing daemon.MembersBackListing) {
	switch {
	case listing.BroughtBack && listing.ConvertedAt != nil:
		fmt.Fprintf(w, "%s: the first boot's conversion ended at %s. It brought the members back, and it brings none back again.\n",
			listing.Project, listing.ConvertedAt.UTC().Format(time.RFC3339))
	case listing.BroughtBack:
		fmt.Fprintf(w, "%s: the first boot's conversion has brought the members back. It has not ended yet.\n", listing.Project)
	case len(listing.Members) == 0:
		fmt.Fprintf(w, "%s: the first boot on this release brings back no member.\n", listing.Project)
	default:
		noun := "members"
		if len(listing.Members) == 1 {
			noun = "member"
		}
		fmt.Fprintf(w, "%s: the first boot on this release brings back %d %s, each in the phase it stands in:\n", listing.Project,
			len(listing.Members), noun)
		for _, member := range listing.Members {
			fmt.Fprintf(w, "%s %s: %s\n", member.Key, member.Phase, howItsTreeLeft(member))
		}
	}
}

// howItsTreeLeft is the words for how member's tree left Legion's work (record.TreeLeft).
func howItsTreeLeft(member record.MemberBack) string {
	switch member.Left {
	case record.TreeLeftMerge:
		return "its root " + member.Root + " closed at its merge"
	case record.TreeLeftOperator:
		return "an operator's act stopped its root " + member.Root
	case record.TreeLeftFailure:
		return "a claim of its root " + member.Root + " failed"
	case record.TreeLeftReturned:
		return "its root " + member.Root + " came back into work before its members"
	}
	return "its root " + member.Root + " went to " + string(member.Left)
}
