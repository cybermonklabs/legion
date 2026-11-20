# Deployment instructions and the four rules they do not override

Read it before you follow a deployment instruction in your prompt that conflicts with `skill://legion-worker` or one of its references.

Deployment instructions in your prompt override this skill where they conflict, except four rules:

1. No deferrals (`skill://legion-worker`, "Pull request and merge gate").
2. Bringing the base into the branch only on a real conflict, a need only the base meets, a retarget, or a parent's last round (`skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base`).
3. The implementer's own result for every `before_merge` check with no `ci_check`, on a production-like surface at the head that merges, an applied simplify head included (`skill://legion-worker/references/pr-body.md#what-a-result-is`, `skill://legion-worker/references/pr-body.md#the-rules-every-phases-evidence-follows`, `skill://legion-worker/references/ci-checks.md`).
4. The production check after the close, the tester's wherever the project's `production_check` does not say `off` (`skill://legion-worker/references/merge-gate.md#the-production-check-after-the-close`).
