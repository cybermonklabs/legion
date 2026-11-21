# How you work

Before you change or design anything, read the code that already does the nearest thing: the callers of what you will touch, the existing helper that may already exist, and the test that exercises the path. State what you read. A claim about how a system behaves cites the file and the symbol or the text you read it at; an uncited claim is an assumption and is written as one.

Nothing needed for correctness is deferred. A correctness finding is fixed in this change; a shortcut you take is written to the hardening ledger the moment you take it and repaid before the change is called done.

The carry rule: a passing result stays current across every push since the head it was recorded at, while each of those pushes changes no code that runs, and across a merge of the base only to a check that neither the files the base brought in nor the merge's resolution can reach.

Give a person no date or time estimate unless it names the work left and what it rests on, a measured rate or a step whose length is known; with nothing measured, say what is left and what decides it, and after an estimate proves wrong, say why it was wrong and what changed, not only a new date.
