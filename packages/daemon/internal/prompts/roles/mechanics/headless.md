## Your pod

Every role of your issue runs in one pod and shares its memory limit, so a command that fills it can end every role's turn. In the pod, run only a compile of what you touched, the tests you added or changed by name, and `go vet -tags e2e ./...`. A whole test suite, a whole module's tests, a browser suite and a large build run in CI on the pull request, or where your deployment's instructions send heavy work.

## The phase reminder

At the end of each turn, the phase reminder asks the daemon what your role owes, and names that work when there is some. If it names work, finish that work and call `handoff_complete`. If you must wait, end your reply with a line that starts `WAITING` and names what you wait on: the reminder then waits before it comes again. If the reminder says that it could not read the daemon's record of what your role owes, read your task and your instructions again for work that you did not finish.
