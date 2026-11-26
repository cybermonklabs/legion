**The definition and the measurement.** The definition of this number is dispatch://INFRA-3117/artifact/method-md. Its measurement over cybermonklabs/legion is dispatch://INFRA-3117/artifact/analysis-md.

**How it is measured.** A change is the merge commit of a merged pull request on the default branch of a project's default repository. Its group is the first of the project's code groups (`code_groups` in its delivery settings) with a head-branch prefix that starts the pull request's head branch, or with the login that opened it. A group with no prefix and no login takes every change that no group before it took. A commit that merged no pull request is no change.

Dispatch reads the patch of each first-parent commit from GitHub and replays the patches in commit order (`survival.Measure`). A line is its commit, its file and its line number when it was added, never its text. A rename keeps a line alive. A line is alive at its change's 30-day mark when no commit dated at or before the mark removed it. Dispatch keeps the counts and no patch.

A change's day is the UTC date of its commit time. Dispatch measures a day once, two days after the 30-day mark of the day's last change. For each code group on each measured day, `fold.BuildDays` writes the series `survival:<group>`: the lines that the group's changes added, the lines of those alive at their marks, the count of changes, and the count of changes with no issue. `fold.FoldSurvival` adds the lines of a window's measured days and divides the two sums. It never takes the mean of the daily shares. The series `status` counts the measured, marked and blocked days of a window and the days that still wait (`fold.FoldStatus`).

A change counts for the project of its issue, as Dispatch's attribution gives the issue of its pull request. A change with no issue counts for each project whose default repository holds it. The `via_repository` figure counts those changes.

**What the number is not.**

- It is the replay's reading, not blame. The replay reads 1.2 points above blame for Legion and 4.8 for the other agent at 30 days, and the gap is uneven.
- It reads replacement and never whether the work was right. A line that someone improved reads like a line that was wrong.
- An edited line is a dead line. One changed character kills the original line and credits the editor.
- The diff is GitHub's, with three lines of context, no whitespace rule and no copy detection.

**When it reads "no data".** A window reads "no data", never 0, when it holds no measured day. It also reads "no data" when it holds a day that a force-push marked, or a day that a commit blocks because GitHub did not give its patch whole. A project whose default repository the GitHub App cannot read reads "no data" too. A project with no code group has no group series.

**What it does not count.** Commits that merged no pull request, which add no line to a group. A line that such a commit removes is dead all the same. Binary files and submodules are not counted.
