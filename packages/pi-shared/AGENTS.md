# Pi Shared

- The Legion modules both pi-legion and claude-envoy run (`createLegionDaemonClient`, `bootLegionClaim`, `executePhaseOp`, `createPhaseReminder`, and each module they import here) import no Oh My Pi type, neither `@oh-my-pi/*` nor `pi-types`:
  each host passes in what they read, so claude-envoy's harness over them stays a thin adapter.

Where this file and the code disagree, the code is right: follow the code and correct this file.
