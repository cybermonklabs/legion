package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// census is `envoy-dispatch census`: the pre-deploy migration census (store.Census) of the
// database DATABASE_URL names, written to out. A deployment runs it, as a one-off task under the
// service's own database credentials, before it rolls the service to this binary's image. Exit 0
// when nothing refuses the pending migrations, 1 when the report refuses (every reason is in the
// report), 2 when the census could not be taken (errOut says why). It prints counts, sizes,
// versions, file names, session metadata and SQLSTATEs only; no row leaves the database.
func census(ctx context.Context, databaseURL string, out, errOut io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "census: DATABASE_URL is required")
		return 2
	}
	report, err := store.Census(ctx, databaseURL)
	if err != nil {
		message := err.Error()
		if !strings.HasPrefix(message, "census: ") {
			message = "census: " + message
		}
		fmt.Fprintln(errOut, message)
		return 2
	}
	report.Write(out)
	if report.Refused() {
		return 1
	}
	return 0
}

// versions is `envoy-dispatch versions`: what schema_migrations, on the database DATABASE_URL
// names, records of each version against the migrations this binary embeds, and what this
// binary's next boot would do with each row (store.CheckVersions), written to out. It reads in a
// read-only transaction and migrates nothing, so a deployment runs it, as a one-off task of the
// image it means to roll out, before it rolls the service. Exit 0 when every recorded version has
// a file of this image and the next boot refuses none, 1 when one has no file, a row would be
// refused or the table records nothing, 2 when the check could not be taken (errOut says why).
func versions(ctx context.Context, databaseURL string, out, errOut io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "versions: DATABASE_URL is required")
		return 2
	}
	check, err := store.CheckVersions(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(errOut, "versions: %v\n", err)
		return 2
	}
	check.Write(out)
	if !check.OK() {
		return 1
	}
	return 0
}
