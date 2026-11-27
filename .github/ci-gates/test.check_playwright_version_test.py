# The check_playwright_version_test filter's gate: what job test reads.
GATE = harness("check_playwright_version_test", "Test the Playwright version check",
        f"{SCRIPTS}/check-playwright-version.test.sh", [
    (f"{SCRIPTS}/check-playwright-version.sh", "the script under test"),
    (f"{SCRIPTS}/ci_images.py", "check-playwright-version.test.sh:49 symlinks it in: the script reads a row's upstream with it"),
    ("bun.lock", "its \"the repository's own tree\" case runs the real check, which resolves this repository's own Playwright out of it"),
    (".github/docker/browser.Dockerfile", "that same case: the check reads the browser image's base FROM out of it"),
    (".github/ci-images.yaml", "that same case: the check reads the row the base FROM's digest names"),
])
