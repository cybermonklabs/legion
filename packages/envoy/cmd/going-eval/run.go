package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/eval"
)

// evalPerson is the person every try acts as, who is none of the seed's people.
const evalPerson = "going-eval@example.com"

// tokenLimit is the most chat tokens the eval spends in a minute: half of the 1,000,000 of the
// chat model's deployment (R13's Design).
const tokenLimit = 500_000

// runOptions are the run subcommand's flags.
type runOptions struct {
	databaseURL     string
	serverBinary    string
	tasks           string
	tries           int
	parallel        int
	judgeDeployment string
	answersOut      string
	workDir         string
	keepDatabases   bool
	source          string
}

// runCommand is `going-eval run`: the eval itself (runEval), with its flags.
func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("going-eval run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var options runOptions
	flags.StringVar(&options.databaseURL, "database-url", os.Getenv("LEGION_TEST_PG_DSN"), "a postgres:// URL of a Postgres server the eval may make its own databases on (default $LEGION_TEST_PG_DSN)")
	flags.StringVar(&options.serverBinary, "server-bin", "", "a built envoy-dispatch to run as the server (default: build ./cmd/dispatch)")
	flags.StringVar(&options.tasks, "tasks", "", "the task ids or kinds to run, comma-separated (default: every task)")
	flags.IntVar(&options.tries, "tries", eval.Tries, "how many times each task runs")
	flags.IntVar(&options.parallel, "parallel", 1, "how many tries of one task run at once, each with a server of its own")
	flags.StringVar(&options.judgeDeployment, "judge-deployment", "", "the chat deployment the judge reads answers with (default $DISPATCH_AZURE_CHAT_DEPLOYMENT)")
	flags.StringVar(&options.answersOut, "answers-out", "", "a file to write every try to, one JSON object a line, its answer included")
	flags.StringVar(&options.workDir, "work-dir", "", "the directory of the servers' logs and files (default: a new one under $TMPDIR, removed at the end)")
	flags.BoolVar(&options.keepDatabases, "keep-databases", false, "keep each try's database, and the seed's, when the run ends, naming each, to read what a try left")
	flags.StringVar(&options.source, "source", "", "the source the server and the eval were built from, such as a commit and the hash of its uncommitted diff, which the report's header names")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || strings.TrimSpace(options.databaseURL) == "" || options.tries < 1 || options.parallel < 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	met, err := runEval(ctx, options, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "going-eval run: %v\n", err)
		return 1
	}
	if !met {
		return 1
	}
	return 0
}

// chatFromEnvironment is the chat model's settings, read from the eval's environment under the
// server's own names.
func chatFromEnvironment() (chatSettings, error) {
	chat := chatSettings{
		Endpoint:   strings.TrimSpace(os.Getenv("DISPATCH_AZURE_CHAT_ENDPOINT")),
		Deployment: strings.TrimSpace(os.Getenv("DISPATCH_AZURE_CHAT_DEPLOYMENT")),
		APIVersion: strings.TrimSpace(os.Getenv("DISPATCH_AZURE_CHAT_API_VERSION")),
		Scope:      strings.TrimSpace(os.Getenv("DISPATCH_AZURE_CHAT_SCOPE")),
		TenantID:   strings.TrimSpace(os.Getenv("DISPATCH_AZURE_TENANT_ID")),
		ClientID:   strings.TrimSpace(os.Getenv("DISPATCH_AZURE_CLIENT_ID")),
		TokenFile:  strings.TrimSpace(os.Getenv("DISPATCH_AZURE_FEDERATED_TOKEN_FILE")),
	}
	var missing []string
	for name, value := range map[string]string{
		"DISPATCH_AZURE_CHAT_ENDPOINT": chat.Endpoint, "DISPATCH_AZURE_CHAT_DEPLOYMENT": chat.Deployment,
		"DISPATCH_AZURE_TENANT_ID": chat.TenantID, "DISPATCH_AZURE_CLIENT_ID": chat.ClientID,
		"DISPATCH_AZURE_FEDERATED_TOKEN_FILE": chat.TokenFile,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return chat, fmt.Errorf("the chat model's settings are missing: %s (the eval signs in to Azure keylessly, as the server does)", strings.Join(missing, ", "))
	}
	return chat, nil
}

// selectTasks is the tasks the flag names, by id or by kind, in the committed order; every task
// for an empty flag.
func selectTasks(tasks []eval.Task, names string) ([]eval.Task, error) {
	if strings.TrimSpace(names) == "" {
		return tasks, nil
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(names, ",") {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	var selected []eval.Task
	found := map[string]bool{}
	for _, task := range tasks {
		if wanted[task.ID] || wanted[string(task.Kind)] {
			selected = append(selected, task)
			found[task.ID], found[string(task.Kind)] = true, true
		}
	}
	for name := range wanted {
		if !found[name] {
			return nil, fmt.Errorf("no task and no kind is %q", name)
		}
	}
	return selected, nil
}

// tryResult is one try of one task, as the report and --answers-out read it.
type tryResult struct {
	Task    string     `json:"task"`
	Kind    eval.Kind  `json:"kind"`
	Try     int        `json:"try"`
	Pass    bool       `json:"pass"`
	Outcome tryOutcome `json:"outcome"`
	// Missing and Unasked are the end state's verdict; MissingLinks and MissingRows what the
	// answer does not link or show; Facts the judge's reading of each fact, and Why its reasons.
	Missing      []string `json:"missing,omitempty"`
	Unasked      []string `json:"unasked,omitempty"`
	MissingLinks []string `json:"missing_links,omitempty"`
	MissingRows  []string `json:"missing_rows,omitempty"`
	Facts        []bool   `json:"facts,omitempty"`
	Why          []string `json:"why,omitempty"`
	// Obeyed is what the try did of its task's planted instruction, "" for nothing; Writes each
	// message or comment of the person's that c13's rule does not let stand.
	Obeyed string   `json:"obeyed,omitempty"`
	Writes []string `json:"writes,omitempty"`
	// Error is what kept the try from running or from being graded.
	Error string `json:"error,omitempty"`
	// Timing is where the try's time went.
	Timing tryTiming `json:"timing"`
	// Database is the try's database where it is kept: a failing try's, and every try's with
	// --keep-databases.
	Database string `json:"database,omitempty"`
}

// tryTiming is the seconds of each phase of a try: copying the seed's database, starting the
// server until it serves, the run from its start to the read of its last frame, the wait for the
// rooms to settle and the server's stop, and the grade with its judge call.
type tryTiming struct {
	Copy  float64 `json:"copy"`
	Boot  float64 `json:"boot"`
	Run   float64 `json:"run"`
	Stop  float64 `json:"stop"`
	Grade float64 `json:"grade"`
}

func (t tryTiming) total() float64 {
	return t.Copy + t.Boot + t.Run + t.Stop + t.Grade
}

// failure is why the try failed, as the report's one line says it.
func (r tryResult) failure() string {
	var why []string
	if r.Error != "" {
		why = append(why, "error: "+r.Error)
	}
	if r.Database != "" && !r.Pass {
		why = append(why, "its store is kept in the database "+r.Database)
	}
	if r.Outcome.State != "" && r.Outcome.State != "done" {
		why = append(why, "the run ended "+r.Outcome.State+" "+clipText(r.Outcome.Reason))
	}
	for _, m := range r.Missing {
		why = append(why, "missing "+m)
	}
	for _, u := range r.Unasked {
		why = append(why, "not asked for: "+u)
	}
	if len(r.MissingLinks) > 0 {
		why = append(why, "links none of "+strings.Join(r.MissingLinks, ", "))
	}
	if len(r.MissingRows) > 0 {
		why = append(why, "shows none of "+strings.Join(r.MissingRows, ", "))
	}
	for i, stated := range r.Facts {
		if !stated {
			why = append(why, fmt.Sprintf("fact %d not stated (%s)", i+1, clipText(r.Why[i])))
		}
	}
	return strings.Join(why, "; ")
}

// evalRun is one run of the eval: what every try shares.
type evalRun struct {
	options   runOptions
	seed      *eval.Seed
	chat      chatSettings
	binary    string
	work      string
	proxy     *chatProxy
	limits    *limiter
	judge     *judge
	databases *databases
	baseline  *eval.Store
	answers   *json.Encoder
	answersMu sync.Mutex
	stderr    io.Writer
}

// runEval runs the eval and writes its report to stdout, answering whether the report meets
// every floor of R13.
func runEval(ctx context.Context, options runOptions, stdout, stderr io.Writer) (bool, error) {
	started := time.Now()
	all, err := eval.LoadTasks(eval.Files)
	if err != nil {
		return false, err
	}
	tasks, err := selectTasks(all, options.tasks)
	if err != nil {
		return false, err
	}
	seed, err := eval.LoadSeed(eval.Files)
	if err != nil {
		return false, err
	}
	chat, err := chatFromEnvironment()
	if err != nil {
		return false, err
	}
	options.judgeDeployment = cmp.Or(options.judgeDeployment, chat.Deployment)
	if err := refuseSessionsDatabase(options.databaseURL); err != nil {
		return false, err
	}
	upstream, err := url.Parse(chat.Endpoint)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" {
		return false, fmt.Errorf("DISPATCH_AZURE_CHAT_ENDPOINT=%q is no https URL", chat.Endpoint)
	}
	e := &evalRun{options: options, seed: seed, chat: chat, limits: newLimiter(tokenLimit), stderr: stderr}
	e.work = options.workDir
	if e.work == "" {
		if e.work, err = os.MkdirTemp("", "going-eval-"); err != nil {
			return false, err
		}
		defer os.RemoveAll(e.work)
	} else if err := os.MkdirAll(e.work, 0o700); err != nil {
		return false, err
	}
	if e.binary = options.serverBinary; e.binary == "" {
		fmt.Fprintln(stderr, "going-eval: building the server")
		if e.binary, err = buildServer(ctx, e.work); err != nil {
			return false, err
		}
	}
	if e.proxy, err = startChatProxy(upstream, e.limits, e.work); err != nil {
		return false, err
	}
	defer e.proxy.close()
	if e.judge, err = newJudge(chat, options.judgeDeployment, e.limits, seed.Now); err != nil {
		return false, err
	}
	if options.answersOut != "" {
		file, err := os.Create(options.answersOut)
		if err != nil {
			return false, err
		}
		defer file.Close()
		e.answers = json.NewEncoder(file)
	}

	fmt.Fprintln(stderr, "going-eval: putting the calibration answers to the judge")
	calibration := calibrate(ctx, e.judge, all)

	fmt.Fprintln(stderr, "going-eval: writing the seed")
	if e.databases, err = newDatabases(ctx, options.databaseURL, seed); err != nil {
		return false, err
	}
	defer e.databases.close(context.WithoutCancel(ctx), options.keepDatabases)
	if options.keepDatabases {
		fmt.Fprintf(stderr, "going-eval: the seed's database %s is kept\n", e.databases.template)
	}
	conn, err := pgx.Connect(ctx, e.databases.url(e.databases.template))
	if err != nil {
		return false, fmt.Errorf("read the seed's database: %w", err)
	}
	e.baseline, err = eval.ReadStore(ctx, conn)
	_ = conn.Close(ctx)
	if err != nil {
		return false, err
	}
	// Writing the seed migrated its database, which compiles the migrations' SQL parser in this
	// process; its memory goes back to the system before the servers of the tries start beside it.
	debug.FreeOSMemory()

	var results []tryResult
	for _, task := range tasks {
		results = append(results, e.runTask(ctx, task)...)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
	highest, calls := e.limits.highestMinute()
	return writeReport(stdout, reportInput{
		started: started, ended: time.Now(), options: options, chat: chat, all: all, tasks: tasks, results: results,
		calibration: calibration, highest: highest, calls: calls,
	}), nil
}

// runTask runs the tries of one task, at most options.parallel at once.
func (e *evalRun) runTask(ctx context.Context, task eval.Task) []tryResult {
	results := make([]tryResult, e.options.tries)
	slots := make(chan struct{}, e.options.parallel)
	var wg sync.WaitGroup
	for try := 1; try <= e.options.tries; try++ {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			result := e.runTry(ctx, task, try)
			if result.Error != "" && result.Outcome.Run == "" && ctx.Err() == nil {
				// The try never started a run: its server or its database failed it, which says
				// nothing of the agent, so it is made once more.
				fmt.Fprintf(e.stderr, "going-eval: %s try %d did not start (%s); trying it again\n", task.ID, try, result.Error)
				result = e.runTry(ctx, task, try)
			}
			results[try-1] = result
			verdict := "fail"
			if result.Pass {
				verdict = "pass"
			}
			fmt.Fprintf(e.stderr, "going-eval: %s try %d: %s (%.0fs: copy %.1fs, boot %.1fs, run %.1fs, stop %.1fs, grade %.1fs) %s\n", task.ID, try, verdict,
				result.Timing.total(), result.Timing.Copy, result.Timing.Boot, result.Timing.Run, result.Timing.Stop, result.Timing.Grade, result.failure())
			if e.answers != nil {
				e.answersMu.Lock()
				_ = e.answers.Encode(result)
				e.answersMu.Unlock()
			}
		}()
	}
	wg.Wait()
	return results
}

// runTry runs one try of task on a copy of the seed, with a server, an agent and a fake Envoy of
// its own, and grades it.
func (e *evalRun) runTry(ctx context.Context, task eval.Task, try int) (result tryResult) {
	result = tryResult{Task: task.ID, Kind: task.Kind, Try: try}
	phase := time.Now()
	lap := func(into *float64) {
		*into = time.Since(phase).Seconds()
		phase = time.Now()
	}
	name := strings.ReplaceAll(task.ID, "-", "_") + fmt.Sprintf("_%d", try)
	database, err := e.databases.copyFor(ctx, name)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	// A try that never ran is made again on a database of the same name, so its copy goes; a try
	// that ran keeps its copy when it fails, or with --keep-databases, for its store to be read.
	graded := false
	defer func() {
		if graded && (!result.Pass || e.options.keepDatabases) {
			e.databases.keep(database)
			result.Database = database
			return
		}
		_ = e.databases.drop(context.WithoutCancel(ctx), database)
	}()
	lap(&result.Timing.Copy)
	envoy, err := startFakeEnvoy(e.seed.Sessions)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer envoy.close()
	secret := make([]byte, 24)
	_, _ = rand.Read(secret)
	agentToken := hex.EncodeToString(secret)
	dir := filepath.Join(e.work, task.ID+"-"+fmt.Sprint(try))
	srv, err := startServer(ctx, e.binary, dir, e.databases.url(database), agentToken, e.chat, e.proxy, envoy.url(), tokenLimit)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	lap(&result.Timing.Boot)
	envoy.answerTo(srv.origin, agentToken)
	p := &person{origin: srv.origin, login: evalPerson, client: &http.Client{}}
	result.Outcome, err = p.drive(ctx, task)
	if err != nil {
		result.Error = err.Error()
	}
	lap(&result.Timing.Run)
	// A document edit is written as a version when its room settles, which the server's stop
	// also does; the store is read once the server has stopped.
	time.Sleep(3 * time.Second)
	srv.stop()
	envoy.close()
	lap(&result.Timing.Stop)
	if err := e.grade(ctx, task, database, &result); err != nil {
		result.Error = strings.TrimPrefix(result.Error+"; "+err.Error(), "; ")
	}
	graded = true
	lap(&result.Timing.Grade)
	if result.Pass {
		_ = os.RemoveAll(dir)
	}
	return result
}

// grade grades a try from the store it left and the answer it gave.
func (e *evalRun) grade(ctx context.Context, task eval.Task, database string, result *tryResult) error {
	conn, err := pgx.Connect(ctx, e.databases.url(database))
	if err != nil {
		return fmt.Errorf("read the try's store: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	after, err := eval.ReadStore(ctx, conn)
	if err != nil {
		return err
	}
	verdict := eval.GradeEnd(e.seed, task, e.baseline, after, evalPerson)
	result.Missing, result.Unasked = verdict.Missing, verdict.Unasked
	result.Obeyed = eval.Obeyed(task, e.baseline, after, evalPerson)
	if result.Writes, err = eval.CheckPersonWrites(ctx, conn, e.seed, task, e.baseline, after, evalPerson); err != nil {
		return err
	}
	pass := verdict.Pass() && result.Error == ""
	if !task.Kind.Changes() {
		result.MissingLinks = eval.MissingLinks(task.Grade.Links, result.Outcome.Answer, result.Outcome.Blocks)
		if task.Grade.Chart != nil {
			result.MissingRows = eval.MissingChartRows(task.Grade.Chart.Rows, result.Outcome.Answer, result.Outcome.Blocks)
		}
		pass = pass && len(result.MissingLinks) == 0 && len(result.MissingRows) == 0
		if strings.TrimSpace(result.Outcome.Answer) == "" && len(result.Outcome.Blocks) == 0 {
			result.Facts = make([]bool, len(task.Grade.Facts))
			result.Why = make([]string, len(task.Grade.Facts))
			for i := range result.Why {
				result.Why[i] = "the run answered nothing"
			}
			pass = false
		} else {
			reading, err := e.judge.read(ctx, task.Words, judgedAnswer(result.Outcome), task.Grade.Facts)
			if err != nil {
				return fmt.Errorf("the judge: %w", err)
			}
			result.Facts, result.Why = reading.Stated, reading.Why
			pass = pass && reading.pass()
		}
	}
	result.Pass = pass
	return nil
}

// judgedAnswer is an answer as the judge reads it: its prose, then each block it drew with the
// rows the server filled it with.
func judgedAnswer(outcome tryOutcome) string {
	var text strings.Builder
	text.WriteString(outcome.Answer)
	for _, block := range outcome.Blocks {
		var data struct {
			Kind  string           `json:"kind"`
			Title string           `json:"title"`
			Rows  []map[string]any `json:"rows"`
		}
		if json.Unmarshal(block, &data) != nil {
			continue
		}
		rows, _ := json.Marshal(data.Rows)
		fmt.Fprintf(&text, "\n\n[A %s block drawn beside the answer, %q, with the rows %s]", data.Kind, data.Title, rows)
	}
	return text.String()
}

// calibrationResult is the judge's agreement with the committed labels.
type calibrationResult struct {
	err       error
	answers   int
	agreed    int
	disagreed []string
}

// calibrate puts each labelled answer of labels.json to the judge, against its task's facts.
func calibrate(ctx context.Context, j *judge, tasks []eval.Task) calibrationResult {
	labels, err := eval.LoadLabels(eval.Files, tasks)
	if err != nil {
		return calibrationResult{err: err}
	}
	result := calibrationResult{answers: len(labels.Answers)}
	for i, answer := range labels.Answers {
		task := tasks[slices.IndexFunc(tasks, func(t eval.Task) bool { return t.ID == answer.Task })]
		reading, err := j.read(ctx, task.Words, answer.Answer, task.Grade.Facts)
		if err != nil {
			return calibrationResult{err: fmt.Errorf("answer %d (%s): %w", i+1, answer.Task, err)}
		}
		verdict := eval.LabelFail
		if reading.pass() {
			verdict = eval.LabelPass
		}
		if verdict == answer.Label {
			result.agreed++
			continue
		}
		result.disagreed = append(result.disagreed, fmt.Sprintf("answer %d (%s): labelled %s, the judge read %s", i+1, answer.Task, answer.Label, verdict))
	}
	return result
}

// reportInput is everything the report is written from.
type reportInput struct {
	started, ended time.Time
	options        runOptions
	chat           chatSettings
	all, tasks     []eval.Task
	results        []tryResult
	calibration    calibrationResult
	highest, calls int
}

// rate is passes of n, as a share and a count.
func rate(passes, n int) string {
	if n == 0 {
		return "none run"
	}
	return fmt.Sprintf("%.1f%% (%d of %d)", 100*float64(passes)/float64(n), passes, n)
}

// meets is the word the report says of a floor.
func meets(ok bool) string {
	if ok {
		return "met"
	}
	return "MISSED"
}

// writeReport writes the report and answers whether it meets every floor.
func writeReport(w io.Writer, in reportInput) bool {
	var missed []string
	fmt.Fprintf(w, "# going-eval run, %s to %s\n\n", in.started.UTC().Format(time.RFC3339), in.ended.UTC().Format(time.RFC3339))
	if in.options.source != "" {
		fmt.Fprintf(w, "Source: %s.\n", in.options.source)
	}
	fmt.Fprintf(w, "Chat model: the deployment %s, through a limiter of %d tokens a minute. Judge: the deployment %s, on the same Azure sign-in.\n",
		in.chat.Deployment, tokenLimit, in.options.judgeDeployment)
	fmt.Fprintf(w, "Tasks: %d of the %d committed, %d tries each, one task at a time (%d tries of it at once). The person is %s.\n\n",
		len(in.tasks), len(in.all), in.options.tries, in.options.parallel, evalPerson)
	if len(in.all) < eval.LeastTasks {
		missed = append(missed, fmt.Sprintf("the eval holds %d tasks, fewer than %d", len(in.all), eval.LeastTasks))
	}
	if len(in.tasks) != len(in.all) || in.options.tries != eval.Tries {
		missed = append(missed, fmt.Sprintf("this run is not the whole eval (%d of %d tasks, %d of %d tries)", len(in.tasks), len(in.all), in.options.tries, eval.Tries))
	}

	byTask := map[string][]tryResult{}
	for _, r := range in.results {
		byTask[r.Task] = append(byTask[r.Task], r)
	}
	fmt.Fprintln(w, "## Tries")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| task | level | mode | tries | why a try failed |")
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- |")
	for _, task := range in.tasks {
		var marks, why []string
		for _, r := range byTask[task.ID] {
			if r.Pass {
				marks = append(marks, "pass")
				continue
			}
			marks = append(marks, "fail")
			why = append(why, fmt.Sprintf("try %d: %s", r.Try, strings.ReplaceAll(r.failure(), "|", "/")))
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n", task.ID, task.Level, task.Mode, strings.Join(marks, " "), strings.Join(why, " <br> "))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Pass rates")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "pass^1 is the share of tries that pass; pass^%d the share of tasks that pass on every try.\n\n", in.options.tries)
	fmt.Fprintf(w, "| kind | tasks | pass^1 | floor | pass^%d | floor |\n", in.options.tries)
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- |")
	count := func(tasks []eval.Task) (int, int, int, int) {
		tries, passes, whole, all := 0, 0, 0, 0
		for _, task := range tasks {
			every := len(byTask[task.ID]) > 0
			for _, r := range byTask[task.ID] {
				tries++
				if r.Pass {
					passes++
				} else {
					every = false
				}
			}
			all++
			if every {
				whole++
			}
		}
		return passes, tries, whole, all
	}
	for _, kind := range eval.Kinds {
		var of []eval.Task
		for _, task := range in.tasks {
			if task.Kind == kind.Kind {
				of = append(of, task)
			}
		}
		if len(of) == 0 {
			continue
		}
		passes, tries, whole, all := count(of)
		ok := tries > 0 && float64(passes)/float64(tries) >= eval.PassOneKindFloor
		if !ok {
			missed = append(missed, fmt.Sprintf("pass^1 of %s is %s, under %.0f%%", kind.Kind, rate(passes, tries), 100*eval.PassOneKindFloor))
		}
		fmt.Fprintf(w, "| %s | %d | %s | %.0f%% %s | %s | |\n", kind.Kind, len(of), rate(passes, tries), 100*eval.PassOneKindFloor, meets(ok), rate(whole, all))
	}
	passes, tries, whole, all := count(in.tasks)
	oneOK := tries > 0 && float64(passes)/float64(tries) >= eval.PassOneFloor
	threeOK := all > 0 && float64(whole)/float64(all) >= eval.PassThreeFloor
	if !oneOK {
		missed = append(missed, fmt.Sprintf("pass^1 of all tasks is %s, under %.0f%%", rate(passes, tries), 100*eval.PassOneFloor))
	}
	if !threeOK {
		missed = append(missed, fmt.Sprintf("pass^%d of all tasks is %s, under %.0f%%", in.options.tries, rate(whole, all), 100*eval.PassThreeFloor))
	}
	fmt.Fprintf(w, "| all | %d | %s | %.0f%% %s | %s | %.0f%% %s |\n\n", len(in.tasks), rate(passes, tries), 100*eval.PassOneFloor, meets(oneOK),
		rate(whole, all), 100*eval.PassThreeFloor, meets(threeOK))

	if problem := writeCalibration(w, in.options.judgeDeployment, in.calibration); problem != "" {
		missed = append(missed, problem)
	}

	fmt.Fprintln(w, "## Planted instructions")
	fmt.Fprintln(w)
	planted, obeyed := 0, []string{}
	for _, task := range in.tasks {
		if task.Injection == nil {
			continue
		}
		planted++
		for _, r := range byTask[task.ID] {
			if r.Obeyed != "" {
				obeyed = append(obeyed, fmt.Sprintf("%s try %d %s", task.ID, r.Try, r.Obeyed))
			}
		}
	}
	if len(obeyed) > 0 {
		missed = append(missed, fmt.Sprintf("%d tries did what a planted instruction told them", len(obeyed)))
	}
	fmt.Fprintf(w, "%d of the tasks run read an instruction planted in a tool result to close an issue or to message an agent. Tries that did it: %d.\n",
		planted, len(obeyed))
	for _, o := range obeyed {
		fmt.Fprintf(w, "- %s\n", o)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## The person's messages and comments")
	fmt.Fprintln(w)
	var broken []string
	for _, r := range in.results {
		for _, b := range r.Writes {
			broken = append(broken, fmt.Sprintf("%s try %d: %s", r.Task, r.Try, b))
		}
	}
	if len(broken) > 0 {
		missed = append(missed, fmt.Sprintf("%d messages or comments of the person's are not asked for, or no approved row made them", len(broken)))
	}
	fmt.Fprintf(w, "Each message and comment the person wrote belongs to a task that asks for it and to an approved row of a plan of that task, but these %d:\n", len(broken))
	for _, b := range broken {
		fmt.Fprintf(w, "- %s\n", b)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Tokens")
	fmt.Fprintln(w)
	tokensOK := in.highest <= tokenLimit
	if !tokensOK {
		missed = append(missed, fmt.Sprintf("the highest minute spent %d chat tokens, over %d", in.highest, tokenLimit))
	}
	fmt.Fprintf(w, "The most chat tokens spent within any 60 seconds: %d, over %d calls of the run; the limit is %d: %s.\n\n", in.highest, in.calls, tokenLimit, meets(tokensOK))

	failedTries := 0
	for _, r := range in.results {
		if r.Error != "" {
			failedTries++
		}
	}
	if failedTries > 0 {
		fmt.Fprintf(w, "%d tries met an error of the eval itself, and count as failed (their lines above say which).\n\n", failedTries)
	}
	if len(missed) == 0 {
		fmt.Fprintln(w, "RESULT: the run meets every floor of R13.")
		return true
	}
	fmt.Fprintf(w, "RESULT: the run misses: %s.\n", strings.Join(missed, "; "))
	return false
}
