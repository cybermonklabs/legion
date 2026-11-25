# Notes for the daemon-go job's steps

The derivations and the history of `.github/workflows/ci-job-daemon-go.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the race step is a job of its own

The workflow holds two jobs, which run at the same time: daemon-go-race runs the race step, and
daemon-go runs the plain step, the race-impossible step and the Stage 1 proof. The plain step's
package list is every package of the module less the race step's five (and the text checks), as
`.github/scripts/ci_job_reads.py` builds both from one list, so the two steps share no package and
nothing orders them.

In one job their times added up. Over the 158 green daemon-go jobs of ci.yaml runs on 2026-11-22
from 13:46Z to 18:10Z (jobs API; the sample is the job list read at 18:57Z), the race step took
26.8 min at the median (36.3 at most) and the plain step 12.0 (15.0), after 1.4 min of setup, for
a job of 41.0 min. At the same time, the race job alone sets the wall time, and the second runner
costs the setup it repeats, about 1.4 min.

The split makes no test do less work. The race step's time is one package's: internal/daemon took
1,352 s of the step's 1,422 s in run 45048924209, and the four other packages of the step ended
inside it. That package's tests are where the rest of the time is.
