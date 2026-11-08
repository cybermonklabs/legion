package exp21

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/worker"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/exp19"
	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// holder runs this process as the experiment's holder and returns its exit code. It
// is not a run of the experiment, so nothing it does reaches a verdict line: the
// driver that started it reads what it did from its ready line and its report file.
func holder(ctx context.Context, cfg harness.Config, role string) int {
	if role != holderRole {
		fmt.Fprintf(os.Stderr, "exp21: %s=%q is not %q\n", roleEnv, role, holderRole)
		return 1
	}
	if err := serveHolder(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "exp21 holder: %v\n", err)
		return 1
	}
	return 0
}

// serveHolder is the process that stands for a Legion pod. It makes a working copy
// and a credential of its own before anything reaches it, waits for a task token,
// does the work only it could do, and answers the server with what that work
// produced.
//
// The two modes differ in one thing only: who puts the task token in the file this
// process watches. In out-of-band mode the driver writes it there, having read it
// from the body that ran in another process entirely; in queue mode the body runs in
// this process, on a task queue named for it, and writes the token itself.
func serveHolder(ctx context.Context, cfg harness.Config) error {
	dir := os.Getenv(dirEnv)
	if dir == "" {
		return fmt.Errorf("%s is required", dirEnv)
	}
	mode := os.Getenv(modeEnv)
	if mode != outOfBandMode && mode != queueMode {
		return fmt.Errorf("%s=%q is neither %q nor %q", modeEnv, mode, outOfBandMode, queueMode)
	}
	tokenFile := os.Getenv(tokenFileEnv)
	if tokenFile == "" {
		return fmt.Errorf("%s is required", tokenFileEnv)
	}
	reportFile := os.Getenv(reportEnv)
	if reportFile == "" {
		return fmt.Errorf("%s is required", reportEnv)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("make the holder's work directory %s: %w", dir, err)
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("this holder's own hostname is unknown: %w", err)
	}

	report := Report{PID: os.Getpid(), Host: host, Mode: mode, TokenReadFrom: tokenFile}

	// 1. What this process holds that the engine does not, in place before anything
	// is scheduled anywhere: a working copy of its own and a file standing in for a
	// credential.
	version, err := jjVersion(ctx)
	if err != nil {
		return err
	}
	report.JJVersion = version
	repo, err := initRepository(ctx, dir)
	if err != nil {
		return err
	}
	report.RepoPath = repo
	credential, err := writeCredential(dir)
	if err != nil {
		return err
	}
	report.CredentialPath = credential.path
	report.CredentialBytes = credential.bytes
	report.CredentialFirstLine = credential.firstLine

	// 2. The client. A holder dials the server because client.CompleteActivity is a
	// call on a client; in out-of-band mode that is the only thing it uses the
	// connection for, and it polls no task queue at all.
	c, err := harness.Dial(ctx, cfg, dialTimeout)
	if err != nil {
		return err
	}
	defer c.Close()

	// 3. Case B's queue: a name this process makes for itself, in the shape
	// experiment 3 read out of the history for a Temporal session's own queue, and a
	// worker of this process's on that name. Case A's holder does none of this.
	if mode == queueMode {
		resource := fmt.Sprintf("exp21-holder-%d-%d", os.Getpid(), time.Now().UnixNano())
		report.TaskQueue = resource + "@" + host
		report.Identity = fmt.Sprintf("%s-worker@%s", resource, host)
		w := worker.New(c, report.TaskQueue, worker.Options{Identity: report.Identity})
		exp19.RegisterActivity(w)
		if err := w.Start(); err != nil {
			return fmt.Errorf("start the holder's worker on %s: %w", report.TaskQueue, err)
		}
		defer w.Stop()
		report.RanWorker = true
	}

	// 4. The ready line. The driver starts no workflow before it has read this, so
	// the working copy, the credential and -- in queue mode -- the poll on the
	// holder's own queue are all in place before any work exists to be dispatched.
	// os.Stdout is unbuffered, so the write itself puts the line on the driver's
	// pipe; Sync then reports EINVAL for a pipe, which has nothing to sync.
	fmt.Printf("%s: pid=%d host=%s mode=%s queue=%s identity=%s ran-worker=%t repository-ready=%t credential-ready=%t\n",
		readyPrefix, report.PID, report.Host, report.Mode,
		orNone(report.TaskQueue), orNone(report.Identity), report.RanWorker, repo != "", credential.path != "")
	_ = os.Stdout.Sync()

	// 5. The work reaching this process: one task token, in a file.
	if _, err := exp19.WaitForLines(ctx, tokenFile, 1, tokenWait); err != nil {
		return fmt.Errorf("no task token reached the holder within %s: %w", tokenWait, err)
	}
	token, err := exp19.TokenForAttempt(tokenFile, 1)
	if err != nil {
		return err
	}
	report.TokenBytes = len(token)

	// 6. The work only this process could do.
	started := time.Now()
	commit, err := commitWork(ctx, dir, repo, report, credential)
	if err != nil {
		return err
	}
	report.WorkTook = time.Since(started)
	report.Work = commit.what
	report.CommitID = commit.id
	report.CommitReadBack = commit.readBack

	// 7. The outcome back, as the activity's result.
	if err := c.CompleteActivity(ctx, token, commit.id, nil); err != nil {
		report.ErrorType = fmt.Sprintf("%T", err)
		report.ErrorMessage = err.Error()
	} else {
		report.Accepted = true
	}

	return writeReport(reportFile, report)
}

// credentialFile is the placeholder the holder writes for itself: where it is, how
// big it is, and the line that says what it is.
type credentialFile struct {
	path      string
	bytes     int
	firstLine string
}

// writeCredential writes the file standing in for the credential a Legion pod holds.
// It is a placeholder on purpose and says so in its own first line, so no reader of
// the repository or of the transcript can take it for a real one.
func writeCredential(dir string) (credentialFile, error) {
	first := "EXP21-PLACEHOLDER-NOT-A-CREDENTIAL: " + credentialNote
	body := first + "\n" +
		"It exists so that the process standing for a Legion pod holds something besides its working copy.\n"
	path := credentialPath(dir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return credentialFile{}, fmt.Errorf("write the placeholder credential %s: %w", path, err)
	}
	return credentialFile{path: path, bytes: len(body), firstLine: first}, nil
}

// initRepository makes the holder's own working copy: a jj repository under its work
// directory, with a jj configuration of its own so the commit depends on nothing in
// the pod's user configuration and nothing outside this directory is written.
func initRepository(ctx context.Context, dir string) (string, error) {
	config := "[user]\nname = \"exp21 holder\"\nemail = \"exp21-holder@example.invalid\"\n"
	if err := os.WriteFile(jjConfigPath(dir), []byte(config), 0o644); err != nil {
		return "", fmt.Errorf("write the holder's jj configuration %s: %w", jjConfigPath(dir), err)
	}
	repo := repositoryPath(dir)
	if _, err := jj(ctx, dir, "git", "init", repo); err != nil {
		return "", err
	}
	return repo, nil
}

// commitMade is what the holder's work produced: the commit id, what the holder read
// back out of its own repository for that id, and the work said in one clause.
type commitMade struct {
	id       string
	readBack string
	what     string
}

// commitWork is the work only this process could do: a file written into the holder's
// own working copy, naming the process, the task token it was handed and the
// placeholder credential it holds, and committed with jj. The commit id that comes
// out exists in no other repository, so a workflow that receives it received
// something only this process could have produced.
func commitWork(ctx context.Context, dir, repo string, report Report, credential credentialFile) (commitMade, error) {
	workFile := filepath.Join(repo, workFileName)
	body := strings.Join([]string{
		"This file was written inside the process that holds this working copy.",
		fmt.Sprintf("holder-pid: %d", report.PID),
		fmt.Sprintf("holder-host: %s", report.Host),
		fmt.Sprintf("holder-mode: %s", report.Mode),
		fmt.Sprintf("holder-task-queue: %s", orNone(report.TaskQueue)),
		fmt.Sprintf("task-token-bytes: %d", report.TokenBytes),
		fmt.Sprintf("task-token-read-from: %s", report.TokenReadFrom),
		fmt.Sprintf("credential-file: %s", credential.path),
		fmt.Sprintf("credential-first-line: %s", credential.firstLine),
		fmt.Sprintf("written-at: %s", time.Now().UTC().Format(time.RFC3339Nano)),
		"",
	}, "\n")
	if err := os.WriteFile(workFile, []byte(body), 0o644); err != nil {
		return commitMade{}, fmt.Errorf("write %s in the holder's working copy: %w", workFile, err)
	}

	message := fmt.Sprintf("exp21 holder pid %d did the work handed to it", report.PID)
	if _, err := jj(ctx, dir, "--repository", repo, "commit", "--message", message); err != nil {
		return commitMade{}, err
	}
	id, err := jj(ctx, dir, "--repository", repo, "log", "--revisions", "@-", "--no-graph", "--template", "commit_id")
	if err != nil {
		return commitMade{}, err
	}
	if id == "" {
		return commitMade{}, fmt.Errorf("jj made no commit in %s: it reported an empty commit id", repo)
	}
	readBack, err := jj(ctx, dir, "--repository", repo, "log", "--revisions", id, "--no-graph",
		"--template", `commit_id ++ " " ++ description.first_line()`)
	if err != nil {
		return commitMade{}, err
	}
	return commitMade{
		id:       id,
		readBack: readBack,
		what:     fmt.Sprintf("wrote %s in its own working copy and committed it with jj as %q", workFile, message),
	}, nil
}

// jjVersion is the jj this holder commits with, printed so the transcript says what
// made the commit.
func jjVersion(ctx context.Context) (string, error) {
	out, err := jj(ctx, "", "--version")
	if err != nil {
		return "", err
	}
	return out, nil
}

// jj runs one jj command and returns its standard output with the surrounding space
// removed. The configuration it reads is the holder's own file and nothing else, and
// a failure carries what jj said on its standard error, which is where jj explains
// itself.
func jj(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", args...)
	if dir != "" {
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "JJ_CONFIG="+jjConfigPath(dir))
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("jj %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// writeReport writes what the holder did where the driver reads it.
func writeReport(path string, report Report) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode the holder's report: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
