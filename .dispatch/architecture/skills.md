---
title: Skills
parent: legion
paths: [skills]
---
The Markdown instructions that guide the architect, the phase workers, the controller, and every agent using Dispatch and Envoy.

An experiment arm's folder of texts (`projects.<KEY>.experiments.<NAME>.variants[].texts`) can replace a skill's files for the roles its experiment reaches: the folder's `skills/<skill>/<path>` stands for this tree's `skills/<skill>/<path>` as the plugins ship it under `dist/skills`, `dispatch-first` among them, and the launch's extension root offers the folder's copy under the skill's plain name (`daemon`, `pi-envoy`).
A launch whose folder holds a skill file no plugin ships, or one whose digest is not the shipped file's, runs Legion's own texts instead, and each agent and skill a folder's text names is held to what the worker image ships, read by the same reader the boot gate reads this tree's texts with (`internal/promptrefs`).
