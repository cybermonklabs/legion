# A check CI proves

A check that carries `ci_check` names the GitHub check run that proves it at the head, as GitHub writes the run's name (a job of a called workflow is `<caller> / <job>`). No role runs it or records a result for it: Dispatch refuses one (`CHECK_PROVED_BY_CI`). `dispatch results --issue <issue>` reads the run at the pull request's head and prints its derived result and the line for the pull request body's `Checks` block.

A run with no result is still going (`not_completed`), missing at the head (`absent`), or concluded without proving anything (`neutral`, `skipped`, `cancelled`). A job the repository runs on a pull request only under a label stays `skipped` or `absent` until the implementer adds that label and pushes. Any other run that no name answers means the check's words are wrong: send it to your architect.
