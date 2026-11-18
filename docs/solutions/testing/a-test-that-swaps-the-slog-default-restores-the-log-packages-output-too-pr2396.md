---
title: "A test that swaps the slog default restores the log package's output too"
category: testing
tags:
  - go
  - slog
  - log-capture
  - forward-merge
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2396"
---

# A test that swaps the slog default restores the log package's output too

- `slog.SetDefault` also points the `log` package's output at the new handler and clears its flags.
  Setting the previous slog default back undoes neither. After such a test, every `log.Print`
  writes into the test's finished buffer, not into stderr.
- In `packages/envoy`, capture the default logger only through `packages/envoy/logtest`
  (`SetDefault`, or `Swap` when the test restores early). It saves and restores slog's default,
  `log.Writer()` and `log.Flags()`. The package is public, so `packages/daemon` can import it too.
- A test that restores the defaults itself rather than through it restores the same three
  values, in the test's cleanup.
- After every forward merge, search main's new test files for a plain `slog.SetDefault(` and
  move each one onto the helper. A merge with no conflict still brings in new captures that
  restore only slog.

## Evidence

- `packages/envoy/logtest/logtest.go:29` (`SetDefault`) and `:36` (`Swap`).
- The merge of main at fa7c2a72 brought four plain captures. These were `dashboard_test.go`,
  two in `routes/assets_test.go` and `docs/render_reuse_test.go`. The tester's red tests
  (1b7eace4) failed there with the log package writing to a slog handler with flags 0, not to
  `*os.File` with flags 3. Commit 1d3a1d3d moved the four captures onto `logtest.SetDefault`.
- `packages/daemon/internal/api/server_test.go:146-152`: the daemon's own restore of all three
  values.
