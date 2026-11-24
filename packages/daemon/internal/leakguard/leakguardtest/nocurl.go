package leakguardtest

// NoCurl is the curl of every leak guard in a test's `legion`, unless the test names a stand-in: a
// path with no file. A guard that has no curl does not make its read through curl. It refuses that
// read as one it cannot make, names this path, runs no curl and reads no token for it. Then it goes
// on as a guard with no curl does. A `legion` that runs the system's /usr/bin/curl sends the read's
// token to api.github.com. That happens where the guard's REST read through a test's stand-in gh
// fails on its connection, or where the guard's own bound stops it, which a loaded machine could
// do before that bound took the test's deadline. GitHub's answer then decides the row
// (INFRA-3904).
//
// Two kinds of `legion` take it:
//   - cmd/legion's test binary, in its own process and in each run of it as `legion`, takes it in
//     its TestMain. A test there names a stand-in with LEGION_CMD_TEST_CURL, or with
//     leakguardCurl in that process.
//   - A `legion` that a test builds links it with NoCurlLinked. That binary has no other way to
//     learn of a stand-in, so a test that needs a curl read builds a binary of its own.
const NoCurl = "/nonexistent/legion-test-names-no-curl"

// NoCurlLinked is the linker flag that sets NoCurl as the guards' curl of a `legion` that a test
// builds (cmd/legion's leakguardCurl, which is empty in every other build): `go build -ldflags
// <NoCurlLinked>`. The linker applies only the last -ldflags of a build, so a build that links
// other values passes them in the same -ldflags.
const NoCurlLinked = "-X main.leakguardCurl=" + NoCurl
