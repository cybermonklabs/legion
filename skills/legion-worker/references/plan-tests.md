# Tests named before code

A plan traces each clause of each requirement to the test rows that prove it: a test file and a test's name, or a row of a table test. A clause with no test row is not planned yet.

For four kinds the plan names the list before any task that writes code: a requirement over a class needs an inventory test that lists every member; parsing another system's output needs fixtures captured from that system, each with its command; UI state with timing needs a state table, one row a test; a changed shared data shape needs each reader and writer, each with its test.
