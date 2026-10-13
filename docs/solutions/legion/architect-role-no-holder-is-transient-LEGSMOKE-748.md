---
title: "An Envoy publish refused with `no holder for role` while the architect re-incarnates is transient: complete the phase and re-send later"
category: legion
tags:
  - envoy
  - architect
  - handoff-complete
  - phase-worker
date: 2026-10-13
status: active
module: legion-workflow
related_issues:
  - "LEGSMOKE-748"
  - "sjawhar/legion-smoke#569"
---

# An Envoy publish refused with `no holder for role` while the architect re-incarnates is transient: complete the phase and re-send later

`envoy_publish` to `notifications.role.legion-<project>-<key>-architect` can be refused with
`no holder for role …` even though the issue record shows the architect's claim as `working`: the
architect's container is between incarnations and has not re-claimed its role yet.

- Do not block the phase on it, and do not treat it as a lost architect. `handoff_complete` is
  delivered through the daemon, which publishes the completion to the architect's role when it is
  held again; complete the phase as normal.
- Keep the report text and send it on your next turn, when a new task or message arrives; by then
  the role has a holder. Confirm with `envoy_role_get` if you need to know before sending.
- Record the refusal in your final message so the next phase knows the architect received the
  completion but not the Envoy report.

## Evidence

- LEGSMOKE-748, 2026-10-13. At 14:50Z the implementer's `envoy_publish` to
  `notifications.role.legion-legsmoke-legsmoke-748-architect` was refused twice with
  `no holder for role legion-legsmoke-legsmoke-748-architect`; `envoy_role_get` answered the same.
  The issue record at 14:15Z showed the architect's incarnation as `…/1`; the record read at
  14:58Z showed `…/2` with `capabilities.measuredAt` 14:53:38Z, so the architect was being
  re-created at the time of the refusal.
- `handoff_complete` at 14:50Z was accepted (`{}`), the tester started, and the next
  `envoy_publish` to the same topic at 15:02Z was accepted (`holder 01a13587-…`).
