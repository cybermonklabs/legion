---
title: Oh My Pi plugin interface
parent: legion
depends_on: [envoy-client]
paths: [packages/pi-shared]
---
The private workspace package both Oh My Pi plugins bundle (`@legion/pi-shared`): the versioned in-process interface the Envoy plugin publishes and the Legion plugin reads, and the modules both entries inline — the role-claim bridge, the injected-user-turn record, the subagent check, the host types, the jj session attribution overlay every harness's pane commits through — with the real-binary test harness and the shared skills-guard and cross-import rules.

The handoff record's Dispatch read (`src/handoff-record.ts`) names no program of its own: its caller passes the `User-Agent` it sends, so each harness's `legion` tool names itself (`pi-legion/<version>`, `claude-envoy/<version>`). That read still calls `/api/v1`, which is deprecated from the first `legion-envoy` release that carries pull request #4274, the release that first serves `/api/v2`, and which ends at the instant a deployment sets in `DISPATCH_API_V1_END`; the `User-Agent` is how Dispatch's access log tells such a caller apart until it moves.
