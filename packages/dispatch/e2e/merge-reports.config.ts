// The config the merge job of .github/workflows/dispatch-browser.yaml joins the legs' blob reports
// under (`bunx playwright merge-reports -c`, .github/scripts/dispatch-browser-merge-reports.sh).
// Its one setting is the directory every blob's test paths are read against, which is the suite's
// own: `testDir: "."` beside this file, as e2e/playwright.config.ts has it.
//
// The legs record their paths under two checkouts. The shard and alone legs run in their jobs'
// container, whose checkout is /__w/<repo>/<repo>, and the embedder and two-process legs on the
// runner, whose checkout is /home/runner/work/<repo>/<repo>. `merge-reports` refuses blobs of two
// test directories ("Blob reports being merged were recorded with different test directories")
// unless it is given a config, and then reads every blob's paths against that config's test
// directory (`rootDirOverride`, playwright/lib/cli/reportActions.js:57). So a row's file is named
// the same in the merged report whichever machine ran it.
//
// It is not e2e/playwright.config.ts itself: that config imports the suite's helpers, which read
// the contracts build the merge job does not make.
export default { testDir: "." };
