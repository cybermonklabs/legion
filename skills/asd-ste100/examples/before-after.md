# Before and after: agent output

Each example gives the text, what was flagged, the rewrite, and the calls made on the way. Word counts are whitespace-separated tokens.

## A. Tool description (strict)

**Before:**
> This tool will attempt to synchronize state across the various backends that have been configured, and if a conflict is detected it may resolve it automatically depending on the strategy that has been set, or otherwise it will surface the conflict for manual review.

**Flagged:** two instructions in one sentence (sync, and resolve or surface); present perfect in the relative clauses ("have been configured", "has been set"); 44 words, over the 25-word cap.

**Not flagged:** "will attempt to" and "may resolve". They are hedges: the tool is not promised to succeed, and the rewrite must not promise it.

**After:**
> The tool tries to synchronize state across the configured backends. If it finds a conflict, it reads the configured strategy. If the strategy allows automatic resolution, the tool may resolve the conflict without a user. If the tool does not resolve the conflict, it reports the conflict for manual review.

The last sentence branches on whether the conflict was resolved, not on what the strategy allows: that is what "or otherwise" meant.

## B. Error message (strict)

**Before:**
> An error may have occurred while processing your request due to a possible mismatch in the expected data format, which could be caused by an outdated client version.

**Flagged:** one sentence with three claims (an error, a format mismatch, a client version); 28 words.

**Not flagged:** "may have occurred" and "could be caused by". The system does not know what went wrong, and both hedges report that.

**After:**
> Your request may have failed. The cause may be a data format that does not match what the server expects. An outdated client can cause this mismatch. Check your client version.

Wrong rewrites read better: "The request failed" asserts a failure the system only suspects, and "an outdated client is the most common cause" invents a frequency the input never gave. "May have failed" keeps a compound verb form because modality wins over the tense rule.

## C. Instruction to an agent (strict)

**Before:**
> Once the upstream job has completed and assuming no errors were raised, the downstream agent should proceed to consume the output artifact, though it is worth noting that partial artifacts are sometimes produced under timeout conditions.

**Flagged:** present perfect ("has completed"); stacked subordinate clauses ("assuming...", "though it is worth noting..."); three facts in one sentence; 36 words, over the 20-word cap.

**After:**
> Wait for the upstream job to finish with no errors. Then read the output artifact. Warning: a timeout can produce a partial artifact. Check that the artifact is complete before you use it.

Two calls, stated rather than hidden:
- "should proceed to consume" became the imperative "read". That is right for an instruction to the agent that acts, and wrong in descriptive text.
- The last sentence is new content: it makes the warning actionable. Add a sentence like it only when the user asks for the reasoning, and name it there as added content. Drop it if the source's silence was deliberate.

## D. README prose (STE-flavored)

**Before:**
> Our caching layer is designed to slot seamlessly into your existing stack with minimal friction and no vendor lock-in; it leverages semantic similarity to dramatically reduce the cache misses that traditionally plague LLM workloads.

**Flagged:** marketing claims without measurement ("seamlessly", "minimal friction", "dramatically"); a semicolon joining two ideas; soft phrasing ("is designed to slot into", "leverages"); 34 words.

**After:**
> A normal cache matches requests by exact text, so a small change in wording causes a cache miss. This cache compares the meaning of a new prompt against the prompts it already holds. It runs alongside your current stack and stores no data outside it.

Flavored mode kept the explanatory rhythm and did not force one term per concept. It still cut the marketing words, the semicolon and the length.
