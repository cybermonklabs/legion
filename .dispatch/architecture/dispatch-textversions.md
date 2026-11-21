---
title: Keyword fill of past text versions
parent: dispatch-server
depends_on: [dispatch-api, dispatch-duty, dispatch-events, dispatch-store]
paths: [packages/envoy/internal/dispatch/textversions]
---
`internal/dispatch/textversions` is the keyword fill: it writes the keyword table of each past version of the searchable text (`search_text_versions` and its cursor row `search_text_versions_fill`, which `dispatch-store` holds) from the event log, and it is the one writer of both tables.
It runs in a serving process, under the hold `search-text-versions-fill`, so one process writes at a time and the others stand by.
It starts only after the server listens and the registry of live server processes admits the process, so a process that the registry refuses writes nothing.
It reads the event log up to the safe horizon in batches of at most 500 events and 2 MB of text, and after each batch it rests for as long as the batch ran.
A batch counts the payload of each event the rules read, and of each version an event starts the text its vector reads besides: the markdown of the primary document version an issue's row names, and the whole body of a post whose event cut it.
Before each batch it pauses while an answers read that its process serves took longer than 800 ms within the last 10 s, or has run longer than 800 ms (`sessionanswers.Latency`).
A reset of the event log (`events.EventLogWasReset`) empties both tables and starts the cursor again at the first event.
After each batch it stores the reach, which `store.TextVersionsAt` reads before it answers a cut.

It imports `store` for the pool, the reach and the lease callers, `duty` for its hold, `events` (`dispatch-events`) for the horizon and the reset check, and `sessionanswers` (`dispatch-api`) for the pause.
Its tests write the issues, the events and the rows they check through `textversions/textversionstest`, which imports no `textversions`.
