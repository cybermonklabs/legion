# Tests named before code

Part of `skill://legion-worker`.
Read it when you write a plan or review one: the planner, and the plan reviewer the planner runs.

A review that finds the cases of a requirement one at a time costs one round for each case.
A plan that names the test of each case before the code lets the implementer meet every case in one round.

## The trace

The plan traces each clause of each requirement to the test rows that prove it: a test file and a test's name, or a row of a table test.
A clause is one thing the requirement says is true, so "each surface shows the stored text, and an empty one shows a dash" has two clauses.
A clause with no test row is not planned yet.

## Four kinds that name their list first

For each of these four kinds, the plan names the list before any task that writes code.

- **A requirement over a class** ("every surface", "each form", "all six commands").
  The plan names an inventory test that lists every member of the class, written first and failing.
  A member the code leaves out then fails a test, and a member someone adds later goes into the list.
  For example: a test that walks the router's table of routes and fails for each route that does not set the new header, written before any route sets it.

- **Parsing another system's output** (a database, a command, an API, a CI log).
  The plan names fixtures captured from the real system, each with the command that captured it, so the parser meets the shapes the system really gives and not a shape someone imagined.
  For example: the output of `psql -c "select ts_headline('english', body, q) from …"` on a real database, saved as one fixture for each shape it takes (one match, two matches, a match at the end of the text).

- **UI state with timing** (a save in place, a delay before a request, a reply that arrives late).
  The plan names a state table: one row for each state and event, and each row is a test.
  For example: "editing, Enter pressed", "editing, one second with no key", "saving, the reply arrives after the next edit".

- **A changed shared data shape** (a field of a record, a message, a file format).
  The plan names each reader and each writer of the shape, each with the test that holds it, so no reader of the old shape stays.
  For example: a renamed handoff field, with the prompt that writes it, the tool that reads it, and the test of each.

## What the plan reviewer asks for

The plan reviewer reads the plan against the requirement text, clause by clause.
It asks for changes when a clause has no test row, or when a requirement of one of the four kinds has no inventory test, fixtures, state table or list of readers and writers.
A list the plan says a later task will write is no list: the list exists before the code, so the code is written against it.
