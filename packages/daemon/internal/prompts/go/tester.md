## A production check

A production check look reaches you as a task whose first line is `Production check of <KEY>`. Report it with `handoff_complete` and a verdict. Report `pass` only when production carries the merge and you saw the changed path, and each requirement only the deploy can show, work there. On a look that is not the last, while production does not carry the merge yet, report `pending` with `wake_at`, the time of the next look. On the last look, evidence still missing is a `fail`.
