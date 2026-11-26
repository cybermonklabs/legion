**The definition and the measurement.** The definition of this number is dispatch://INFRA-3117/artifact/method-md. Its measurement over cybermonklabs/legion is dispatch://INFRA-3117/artifact/analysis-md.

**How it is measured.** The changes, their groups, their days and the replay are those of code-survival-30d. When a later change removes a line of a change at or before that change's 30-day mark, the removal is of one of four kinds:

- Rework: the later change is of the same group, for a different issue.
- Iteration: the later change is of the same group, for the same issue.
- No issue: the later change is of the same group, and one of the two changes has no issue.
- Other: the later change is of another group or of no group, or the line was removed by a commit that merged no pull request.

This number counts rework alone: the lines of a group's changes that a later change of the same group for a different issue removed within 30 days. For each code group on each measured day, `fold.BuildDays` writes the series `rework:<group>`, `iteration:<group>`, `noissue:<group>` and `other:<group>`. Each holds the lines removed in its kind and the lines that the group's changes added. `fold.FoldRework` adds the lines of a window's measured days and divides the two sums. It never takes the mean of the daily shares. The series `status` is that of code-survival-30d (`fold.FoldStatus`).

**How it differs from Rework share.** Rework share counts pull requests whose title starts with fix, revert or hotfix, over the merged pull requests. This eval counts lines and reads no title. A line counts when a later change of the same group for a different issue removed it within 30 days of the change that added it.

**The 8.2 per cent is a different reading.** The 8.2 per cent of dispatch://INFRA-3117/artifact/analysis-md is read at the tip with no age limit, over Legion's merges of any age, and is not this number. This eval reads each change at its own 30-day mark.

**What the number is not.**

- It is the replay's reading, not blame. The replay reads 1.2 points above blame for Legion and 4.8 for the other agent at 30 days, and the gap is uneven.
- It reads replacement and never whether the work was right. A line that someone improved reads like a line that was wrong.
- An edited line is a dead line. One changed character kills the original line and credits the editor.
- The diff is GitHub's, with three lines of context, no whitespace rule and no copy detection.

**When it reads "no data".** A window reads "no data", never 0, when it holds no measured day. It also reads "no data" when it holds a day that a force-push marked, or a day that a commit blocks because GitHub did not give its patch whole. A project whose default repository the GitHub App cannot read reads "no data" too. A project with no code group has no group series.

**What it does not count.** Removals after a change's 30-day mark, and the kinds iteration, no issue and other, which have series of their own.
