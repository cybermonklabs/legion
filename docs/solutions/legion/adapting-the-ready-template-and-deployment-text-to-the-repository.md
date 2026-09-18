---
title: "Adapting the READY template and deployment text to the repository"
category: legion
tags:
  - ready-template
  - ci-line
  - deployment-instructions
  - spec-precedence
  - discrepancies
date: 2026-09-18
status: active
module: legion-worker
related_issues:
  - "SL200C-1"
  - "sjawhar/legion-smoke#83"
---

# Adapting the READY template and deployment text to the repository

Two places where the standing Legion text did not match the repository on SL200C-1, and the
rulings that let the tree proceed without a human ask.

## The CI line names workflows the repository does not have

The READY template's CI line reads `` `Tests` run <run-id> — jobs lint, typecheck, test ... `PR Title`
run <run-id> ``. `sjawhar/legion-smoke` has neither; it has `ci` (job `check`) and `fail-on-demand`
(job `gate`). Name the real workflows by job and run id, and keep the template's intent (every
workflow green at the head):

```
**CI:** `ci` run 35390926442 — job check success at <head>; `fail-on-demand` run 35390926560 — job gate success at <head>.
```

Two details a future worker will hit:

- A pushed branch with an open pull request runs every workflow twice per head, once on `push` and
  once on `pull_request`. List with `legion gh -- run list --repo <owner>/<repo> --branch legion/<KEY>
  --json databaseId,name,status,conclusion,headSha,event`, cite the `pull_request` pair, and mention
  the `push` pair so a reader does not wonder about four runs.
- When no workflow runs the proof command, say so on the CI line (`CI is a bare gate; the
  acceptance proof is the E2E lines below`) so the reviewer does not mistake green CI for the proof.

## Deployment text says "one file", the spec says two

The project's deployment instructions read `keep every change to the one file the issue names`;
the issue title, spec version 2, and the architect's assignment all named a module and its test.
Ruling recorded in `plan.json.discrepancies[0]` and repeated in the implementer's completion
report: the spec is the issue's primary document and is more specific; the deployment text is the
generic sandbox rule. Two files, one commit.

The pattern: when a standing instruction and the spec disagree, check whether the spec, the
architect's assignment, and the issue title agree with each other. If they do, the spec wins, the
planner records the discrepancy and ruling in the plan handoff, and the implementer names it in
the completion report to the architect. Open a `dispatch_ask` only when the sources themselves
disagree or the answer needs human authority.
