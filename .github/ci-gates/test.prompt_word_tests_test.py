# The prompt_word_tests_test filter's gate: what job test reads. The harness runs the search
# over a worktree of the checkout's own HEAD with planted tests, and its checks read only the
# planted files and the clean copy, so the rest of the tree decides none of them.
GATE = harness("prompt_word_tests_test", "Test the search for tests of prompt words", "scripts/prompt-word-tests.test.sh", [
    ("scripts/prompt-word-tests.sh", "the script under test (scripts/prompt-word-tests.test.sh:27 names it)"),
    (f"{DAEMON}/internal/workflow/phase_status_test.go",
     "scripts/prompt-word-tests.test.sh:45 names it: the clean cases copy it as the test of structured values"),
    (f"{DAEMON}/internal/workflow/wake.go",
     "the search reads the name WakeTask from it, which the planted task case's row must carry"),
])
