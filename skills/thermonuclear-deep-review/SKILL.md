---
name: thermonuclear-deep-review
description: Use when asked for a thermonuclear or deep review, or for a security and correctness audit, of a branch or PR diff. Covers bugs, breaking changes, security issues, developer-experience regressions and feature-gate leaks.
disable-model-invocation: true
---

# Thermonuclear Deep Review

Make your own scratch as `skill://thermonuclear-code-quality` says, and delete only the paths you made: every role of this issue writes into one shared `$TMPDIR`.

## Verdict

The first line of the report is the verdict, in exactly this form, because a PR gate reads only that line. `<sha>` is the short sha of the head you reviewed, or the sha your brief names when you have no shell:

- `Verdict: CHANGES at <sha>` when the report has a Medium or High finding.
- `Verdict: APPROVE at <sha>` when it has none. Minor findings do not block.

Trace each finding end to end before you rate it. Minor changes no behavior, hides no error and breaks no gate. Any other finding is Medium, or High when the report holds its trace: developers who get false High findings stop listening.

## Scope

Audit the range your brief names, or the checked-out branch against its base when it names none. Report issues in code the pull request (the checked-out branch against its base) adds or modifies, in the unchanged consumers check 2 finds, and in the PR body's claims (check 6), because the author fixes only the pull request's diff and that diff never shows them. A range limits what you must read, never what you report: a defect you see in code the pull request changed before the range starts is a finding.

## Checks

1. **Breakage.** Trace each change through its callers and contracts, across packages and modules.
2. **Consumers.** For each behavior the diff changes (log level, status code, exception type, metric or field name, payload shape, identity or uniqueness key, timing, retry, default), name its consumers and show that each still works. Tracing callers misses these, many outside the diff, the package or the repository:
   - alert and monitor predicates (a monitor on `status:error` stops when a line goes down to WARNING)
   - log-level contracts, structured-log field names, log-derived metric filters
   - status codes and error-type strings in request logs, webhooks and audit records
   - identity or uniqueness keys that a persistence layer arbitrates on
   - dashboards, SLOs, saved queries, downstream parsers

   If you cannot find a consumer, say so. A change that removes its own alarm is High, because it also removes the signal that would catch it.
3. **Developer experience.** Find changes to how developers run or build the code locally, which no test catches: where or how the code reads secrets, new or renamed environment variables, remapped ports or networking, a new script that must run for a feature to keep working. A new alternative way to run or build, or a new package-manager dependency, is no break unless it needs a step the README or AGENTS.md does not list.
4. **Feature leaks.** Trace each feature flag or internal-only check and its callers: a leak ships a feature to users a gate excludes.
5. **Intended breakage.** A high-risk effect the PR body names and bounds (who it affects, how to undo it) is not a defect. Report one it does not bound, or one that removes a safeguard or feature gate.
6. **PR body claims.** Attack each safety or correctness claim in the PR body, since the diff does not show whether prose is true. First:
   - Order, not coverage: after a pipeline sanitizes, can any later pass create or recreate what the sanitizer neutralized?
   - An input called safe because another program escapes it depends on that program: rate the finding as if it did not.

## Security Guidelines

For each row whose surface the diff touches, answer with a file:line citation.

| Tag | If the diff touches… | Answer, with file:line |
| --- | --- | --- |
| `authz` | an authorization or refusal check, or a new route, command, or tool | who may call it, who may not, where the diff enforces that, and what the unauthorized caller gets |
| `secret` | a token, grant, secret file, credential helper, or its lifetime | what widened: who can read it, for how long, in which process |
| `untrusted-input` | a subprocess, argv, path, template, or query built from text an outside party controls (an issue body, a PR comment, a webhook payload, model output) | the boundary that neutralizes it, and whether any later pass can re-create what it neutralized (order, not coverage) |
| `prompt` | a prompt that embeds untrusted text into an agent's instructions | what delimits the untrusted region, and what the agent may do if it obeys that text (OWASP LLM01 prompt injection, LLM06 excessive agency) |
| `supply-chain` | a dependency, lockfile, base image, or GitHub Action | the version, the pin (a digest or a SHA checked against its tag), and the permissions the workflow runs with |
| `sandbox` | a sandbox or pod manifest, a capability, or a network policy | which isolation property changed, and against whom |
| `agent-def` | an agent definition, skill, role prompt, `.omp/` config, or `AGENTS.md` | whether this diff can steer its own reviewers, and, if so, what a reviewer must check by hand |
| `transport` | TLS or certificate verification, a signature, an HMAC, a randomness source, or a read or write that untrusted input sizes with no bound | what changed, the check or bound it relies on, and the ASVS identifier it maps to |

- Cite an OWASP ASVS v5.0.0 identifier where one applies (`v5.0.0-1.2.5` style).
- A security finding with no stated exploit path is hardening, ranked Minor. One that states an exploit path is a finding at its real priority.
- Report at most two hardening items, ranked, and put the rest in one paragraph.
- Start each security finding with its row's tag, `Security[<tag>]:`.
- End the report with one `Security:` line: each touched row's tag and its answer with file:line, or `Security: no sensitive surface in this diff.` when the diff touches none.

## Findings

- Never report a finding whose open question you can settle by reading code. When you cannot settle it, say what you searched and where.
- Rate each finding; the lead disposes of it (act on, consider, noted or dismissed). Give each the trace, the consumer it breaks and what it depends on, and put on the must-fix list only what must be fixed before this change ships.
- Do not review a diff you wrote: hand it to a session that did not write it.

## PR discussion

Only after your own audit, and only with a Medium or High finding on a branch that has a PR:

1. Read the PR's comments and review comments, bots' included, with `gh` or `glab`.
2. Check their findings in the code: a comment is not evidence. Include the valid ones you missed; for one you share, add their evidence if yours lacks it.
3. Mark each finding that came from that discussion.
