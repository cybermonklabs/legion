# Tests named before code

## The trace

The plan traces each clause of each requirement to the test rows that prove it: a test file and a test's name, or a row of a table test. A clause is one thing the requirement says is true: "each surface shows the stored text, and an empty one shows a dash" is two clauses. A clause with no test row is not planned yet.

## Four kinds that name their list first

For each of these, the plan names the list before any task that writes code:

- **A requirement over a class** ("every surface", "all six commands"): an inventory test that lists every member, written first and failing. Example: a test that walks the router's route table and fails for each route without the new header.
- **Parsing another system's output** (a database, a command, an API, a CI log): fixtures captured from the real system, each with the command that captured it, one per shape the output takes.
- **UI state with timing** (a save in place, a debounce, a late reply): a state table, one row per state and event, each row a test.
- **A changed shared data shape** (a record field, a message, a file format): each reader and each writer of the shape, each with the test that holds it.

## What the plan reviewer asks for

The plan reviewer reads the plan against the requirement text, clause by clause. It asks for changes when a clause has no test row, or when a requirement of one of the four kinds has no inventory test, fixtures, state table or list of readers and writers. A list the plan says a later task will write is no list.
