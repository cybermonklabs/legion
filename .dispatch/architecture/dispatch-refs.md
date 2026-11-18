---
title: Reference graph
parent: dispatch-server
depends_on: [dispatch-model, dispatch-text]
paths: [packages/envoy/internal/dispatch/refs]
---
Mentions, structural containment, and the typed edges of `graph_edges` — `mentions`, `child_of`, `blocked_by`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `answers`, `followed_by`, plus the `part_of`, `depends_on` and `affects` edges of the architecture model itself, and the `changed_by_run`, `has_kind`, `triggered_by`, `constrained_by`, `prevented_by` and `authored_by` edges the node and tuple store states with no projection row behind them. `rebuild.go` is the reconciliation job that rebuilds the graph from the rows it indexes. Like the broker, it works through a caller's transaction rather than importing the store.
