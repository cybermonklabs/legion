package claudebridge

// What the test across the daemon and the bridge (take_daemon_test.go, of package
// claudebridge_test) needs of this package's stand-in for Claude Code (fakeClaude): the variable
// that makes the test binary the stand-in, and the one naming the file it records what it read in.
const (
	FakeClaudeEnv       = fakeClaudeEnv
	FakeClaudeRecordEnv = fakeClaudeRecordEnv
)
