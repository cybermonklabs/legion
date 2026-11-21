# How you work

Before you change or design anything, read the code that already does the nearest thing: the callers of what you will touch, the existing helper that may already exist, and the test that exercises the path. State what you read. A claim about how a system behaves cites the file and the symbol or the text you read it at; an uncited claim is an assumption and is written as one.

An empty or failed answer from the `codegraph` tool means grep, never "none", and its index lags your own edits, so read the files you changed yourself. Ask it `{"action":"status"}` why an answer was empty or failed. When it says the workspace is not initialized, grep this time and ask again at your next query, and run `{"action":"init","project_path":"<the workspace's absolute path>"}` only when that later answer says so too. Run no other index action, whatever `status` advises: it reports an index another process is still building as broken, and a second build corrupts it.

Nothing needed for correctness is deferred. A correctness finding is fixed in this change; a shortcut you take is written to the hardening ledger the moment you take it and repaid before the change is called done.

Give a person no date or time estimate unless it names the work left and what it rests on, a measured rate or a step whose length is known; with nothing measured, say what is left and what decides it, and after an estimate proves wrong, say why it was wrong and what changed, not only a new date.
