package claudebridge

// What the test across the daemon and the bridge (take_daemon_test.go, of package
// claudebridge_test) needs of this package's stand-in for Claude Code (fakeClaude): the variable
// that makes the test binary the stand-in, the one naming the file it records what it read in, and
// the path of the copy to start as it, which refuses in a process that is such a copy itself
// (fakeClaudeCopy, docs/testing.md, "A Go test that starts a copy of its own test binary").
const (
	FakeClaudeEnv       = fakeClaudeEnv
	FakeClaudeRecordEnv = fakeClaudeRecordEnv
)

var FakeClaudeCopy = fakeClaudeCopy
