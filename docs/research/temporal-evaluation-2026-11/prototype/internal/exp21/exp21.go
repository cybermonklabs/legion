// Package exp21 is experiment 21 of the Temporal evaluation: whether the handout
// reaches a process that holds what a Legion pod holds, and whether the outcome of
// work done inside that process comes back to the workflow.
//
// Experiment 19 measured the handout itself: an activity whose body returns
// activity.ErrResultPending stays Started with its worker killed, is dispatched to no
// replacement, and is completed by a process that runs no worker and holds only the
// task token. Experiment 20 held one such handout open for five minutes with nothing
// alive. Experiment 3 measured the other way of addressing one process: Temporal's
// session framework dispatches a session's activities to a <resourceID>@<hostname>
// task queue the session's own worker process made, which no other poller touches.
//
// So addressability by task-queue name and completion by token are both known. What
// was not known, and what this experiment measures, is whether either shape reaches a
// process that holds a working copy and a credential -- the two things a Legion pod
// has that the engine does not -- and whether the result of work done inside that
// process comes back to the workflow as a value.
//
// The holder is the process that stands for the pod. It makes a jj repository of its
// own under its work directory, writes a placeholder credential file, and does work
// only it could do: it writes a file in that repository and commits it with jj, so
// the outcome is a commit id that exists in no other repository. It holds no part of
// the engine's state: no history, no queue, no workflow. Its credential is a
// placeholder this experiment writes itself; no real credential is used anywhere in
// this run, so nothing here measures credential handling.
//
// Two cases, in this order:
//
//   - Case A, out of band. The activity's body runs on an ordinary worker process and
//     hands its task token out. The holder runs no Temporal worker and registers no
//     activity: the token reaches it as a file in its own work directory, it does its
//     work and calls client.CompleteActivity with the commit id.
//   - Case B, a queue named for the holder. The holder registers a worker on a task
//     queue it names for itself, in the <resourceID>@<hostname> shape experiment 3
//     saw the session framework use, and the workflow schedules the same handout
//     activity on that queue. The body runs inside the holder, and the holder
//     completes it by token from its own process.
//
// The workflow, the activity, the worker process that runs the body in case A and the
// readers of the server are experiment 19's, imported rather than repeated. This
// package holds the holder, the two cases and what they measure.
package exp21

import (
	"path/filepath"
	"time"
)

// The experiment's timings. Everything but the handout's own bound is short: both
// cases, two holder processes and a worker have to finish well inside the runner's
// bound.
const (
	dialTimeout = 30 * time.Second

	// handoutWindow is the StartToCloseTimeout both cases give the handout: four
	// hours, which is a bound the server is told to enforce and not a duration this
	// run observes. Nothing here waits for it; the run's own waits below are what
	// bound this experiment.
	handoutWindow = 4 * time.Hour
	// workflowTimeout must outlast the handout's own bound, or the workflow would
	// expire before the thing it schedules could.
	workflowTimeout = 5 * time.Hour

	workerStartWait = 40 * time.Second
	// holderStartWait covers the holder making its repository and, in case B,
	// starting its worker, before it reports itself ready.
	holderStartWait = 60 * time.Second
	bodyWait        = 30 * time.Second
	// holderWorkWait is how long the driver waits for the holder's report: the work
	// is one jj commit and one completion call, so this is generous.
	holderWorkWait = 120 * time.Second
	workflowWait   = 60 * time.Second
	// tokenWait is how long a holder waits for a task token to reach it before it
	// gives up: the driver hands case A's over within seconds of the body running,
	// and case B's body writes its own inside the holder.
	tokenWait = 120 * time.Second
)

// The environment a holder process is started with. A holder is this same experiments
// binary invoked again with the same -address, -namespace and -work-dir: the role says
// what it is, the mode says which case it serves, and the rest is what that mode
// needs. They are environment variables rather than flags for the reason experiment
// 19's are: cmd/experiments' flags belong to the experiments, not to the processes one
// experiment starts for its own measurement.
const (
	roleEnv      = "EXP21_ROLE"
	modeEnv      = "EXP21_MODE"
	dirEnv       = "EXP21_HOLDER_DIR"
	tokenFileEnv = "EXP21_TOKEN_FILE"
	reportEnv    = "EXP21_REPORT_FILE"

	// holderRole is the one role this experiment starts: the process standing for a
	// Legion pod, which holds a working copy and a placeholder credential.
	holderRole = "holder"

	// outOfBandMode is case A's holder: no worker, no registered activity, no task
	// queue. The task token reaches it as a file.
	outOfBandMode = "out-of-band"
	// queueMode is case B's holder: a worker of its own on a task queue named for
	// itself, which the activity's body then runs in.
	queueMode = "queue"

	// readyPrefix is how a holder tells the driver that it is up: what it is, where
	// it can be addressed, and that it holds its working copy and its credential
	// already. The driver starts no workflow before it has read this line.
	readyPrefix = "exp21holder"
)

// What the holder keeps in its work directory. The driver names all three before the
// holder starts, so the transcript can print them before any work is scheduled.
const (
	repoName       = "repo"
	credentialName = "credential"
	jjConfigName   = "jjconfig.toml"
	workFileName   = "handout-work.txt"
)

// credentialNote is what the credential file is, said in the transcript and written
// into the file itself. No reader of this experiment should take it for a measurement
// of credential handling.
const credentialNote = "a placeholder file this experiment writes itself: it holds no secret, is sent nowhere and reaches no service, so nothing in this run measures credential handling"

// repositoryPath is the holder's working copy: a jj repository of its own, inside its
// own work directory and nowhere near the repository this evaluation lives in.
func repositoryPath(dir string) string { return filepath.Join(dir, repoName) }

// credentialPath is the file standing in for the credential a Legion pod holds.
func credentialPath(dir string) string { return filepath.Join(dir, credentialName) }

// jjConfigPath is the jj configuration the holder writes for itself, so its commits
// depend on nothing in the pod's user configuration and write nothing outside its own
// directory.
func jjConfigPath(dir string) string { return filepath.Join(dir, jjConfigName) }

// Report is what one holder did, written to a file because the driver is another
// process. A refused completion is not a failure of the holder: it is the reading
// this experiment exists to take, so it is reported rather than raised.
type Report struct {
	PID  int    `json:"pid"`
	Host string `json:"host"`
	Mode string `json:"mode"`

	// RanWorker, TaskQueue and Identity say whether this holder was addressable
	// through Temporal at all, and under what name.
	RanWorker bool   `json:"ranWorker"`
	TaskQueue string `json:"taskQueue"`
	Identity  string `json:"identity"`

	// What the holder held before anything reached it.
	JJVersion           string `json:"jjVersion"`
	RepoPath            string `json:"repoPath"`
	CredentialPath      string `json:"credentialPath"`
	CredentialBytes     int    `json:"credentialBytes"`
	CredentialFirstLine string `json:"credentialFirstLine"`

	// How the work reached it.
	TokenReadFrom string `json:"tokenReadFrom"`
	TokenBytes    int    `json:"tokenBytes"`

	// The work only this process could do, and what it produced.
	Work           string        `json:"work"`
	CommitID       string        `json:"commitId"`
	CommitReadBack string        `json:"commitReadBack"`
	WorkTook       time.Duration `json:"workTook"`

	// What the server answered the holder's completion call.
	Accepted     bool   `json:"accepted"`
	ErrorType    string `json:"errorType"`
	ErrorMessage string `json:"errorMessage"`
}
