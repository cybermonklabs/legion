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
//   - A `legion` that a test builds links it with NoNetworkLinked. That binary has no other way to
//     learn of a stand-in, so a test that needs a curl read builds a binary of its own.
const NoCurl = "/nonexistent/legion-test-names-no-curl"

// NoGitHub is the origin that the guard's read with no credential asks in a test's `legion`
// (leakguard.Options.LsRemoteOrigin): a file URL of a directory no machine has. That read is
// `git ls-remote <origin>/<owner>/<name>.git HEAD`, run with the first git on PATH. A test that
// stands in for it puts a git of its own first on PATH, which answers whatever the URL. Every other
// git, the real one, refuses this URL at once ("does not appear to be a git repository"), so the
// guard refuses the read, and nothing asks github.com. With the ordinary origin, a row whose guard
// reached that read with no stand-in git asked github.com, and the network decided the row
// (INFRA-3904). cmd/legion's TestMain takes it, and NoNetworkLinked links it.
const NoGitHub = "file:///nonexistent/legion-test-names-no-github"

// NoNetworkLinked is the linker flag that gives a `legion` that a test builds NoCurl as its guards'
// curl and NoGitHub as the origin of their read with no credential (cmd/legion's leakguardCurl and
// leakguardLsRemoteOrigin, which are empty in every other build): `go build -ldflags
// <NoNetworkLinked>`. The linker applies only the last -ldflags of a build, so a build that links
// other values passes them in the same -ldflags.
const NoNetworkLinked = "-X main.leakguardCurl=" + NoCurl + " -X main.leakguardLsRemoteOrigin=" + NoGitHub
