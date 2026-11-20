# Every turn: root claims and free slots

Read this on every turn, for the skill's every-turn steps 1 and 6: the trees waiting on a root claim, and the count of free slots.

## Trees waiting on a root claim

A root architect whose claim on its root was refused waits, and nothing tells it when the holder lets go. `dispatch read` each root in `admission.trees` still at `phase: admitted`. When `Claimed by:` is `nobody` or ends `· not running`, `envoy_publish` `<KEY>: the claim is free; claim it again.` to `notifications.role.<issues.<KEY>.architect.locator.claim>`.

## Free slots

A slot is one issue the daemon started, a root or a child. Free slots are `admission.cap`, less every issue in `admission.active`, less each issue of `admission.waiting` in neither `admission.active` nor `admission.blocked`. A returned issue goes before an unstarted one whatever their ranks, by design ([Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label)).

A move to `backlog` or `icebox` takes an issue out of the line, and a re-rank of the issues ahead of an issue that has not started raises that issue only among the issues that have not started: neither act moves it ahead of one issue whose work came back.
