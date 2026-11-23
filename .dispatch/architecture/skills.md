---
title: Skills
parent: legion
paths: [skills]
---
The Markdown instructions that guide the architect, the phase workers, the controller, and every agent using Dispatch and Envoy.

`plain-words` is also the text of Dispatch's written rules: the server embeds a copy of it (`packages/envoy/internal/dispatch/writingcheck/plainwords/skill/`, which `go generate ./internal/dispatch/writingcheck/plainwords` makes from `packages/envoy`), and `TestTheCopyIsTheSkill` fails while the copy and the skill differ, so an edit of the skill makes the copy again in the same change.
