package holdmodel

func orderSeeds() []seed {
	cs := []seed{}
	add := func(n, s string) { cs = append(cs, seed{n, ready, s}) }
	t := func(n, s string) { add("PRRT_kwDORFy7ds68"+n, s) }
	t("CUsn", wire+"open row refused :charge1 :owed :H4 :kept")
	t("F5Q2", ack+"wait "+next+"open row refuses refused1 :atLimit no :H3r :atLimit :kept ready accepts retry :H4")
	t("CUsr", wire+"start hello stale end lost :launch :kept :noRelaunch")
	t("F5RA", ack+"wait "+next+"start hello stale end lost1 :relaunch no :launch")
	t("C-Hi", wire+"open row lost :H2 bound :kept")
	t("C-Hm", waiting+"no :H4 :unread :interrupted")
	t("F5Q7", ack+"wait start open row end no!1.1 :pending repair :H4")
	t("F5RH", held+"end :H2 ok :H4")
	t("F5RM", ack+"open gate :owed :H4")
	t("F5RQ", ack+"op :op :H2")
	t("F5RT", waiting+"park wake :owed :H4 :noLaunchCause")
	t("F5RX", held+"end :H2 late :H4")
	t("GiMG", waiting+"park wake!1.1 :pending :noStop repair :owed")
	t("GiMH", wire+"hello refused "+next+":again")
	t("CUs1", held+"end ok :served :H4 :news1")
	add("pause-control/stale-at-turn-end", held+"stale end :H2 error :H4 :owed :noLaunchCause")
	add("death-after-failed-takeback", waiting+"no!1.1 death :kept :H4 :owed :noLaunchCause")
	add("dirty-recovery-stale-ack", wire+"compact!1.1 ack :unread compactEnd :unread")
	add("refused-stop-unrelated", ack+"open row refuses wait bound wrongToken ready :H3r accepts retry :H4")
	// D3: the loss takes back the confirmation its own turn gave.
	for _, p := range []string{"lost end", "end lost"} {
		add("lost-vs-turn-end/"+p, wire+"start open row "+p+" :H2 :unread ok :served")
	}
	add("unanswered-wrong-id-compaction", wire+"compact foreign ack end compactEnd :unread :idle")
	add("C1/C2-replay-no-resend", ack+"detach late reconnect replay :unread :single")
	add("C1/C2-replay-same-id", wire+"lost detach reconnect "+next+"detach late reconnect replay :kept :alias")
	for _, by := range []string{"op row", "row op"} {
		add("session-conflict/"+by, work+"open "+by+" :op :noPause :H1")
	}
	add("S4-over-working-pause", held+"daemon :pause")
	add("S4-over-release", work+"release daemon :release")
	add("ready-before-first-turn", ack+"open row :H2 ready :H2")
	add("compact-nested-turn", wire+"compact open row foreign compactEnd :H1 end :H4 :unread")
	for _, p := range []string{"report(promptedRefused) fatal", "fatal report(promptedRefused)"} {
		add("prompted-report-order/"+p, waiting+p+" :failed :kept")
	}
	add("gate-parked-wake-success", held+"park end ok :served :owed")
	for _, op := range []string{"bound", "death", "new"} {
		add("fatal-write-before/"+op, waiting+"down fatal :pending "+op+" up repair :failed :kept")
	}
	add("duplicate-unprompted-report", waiting+"fatalReport!2.1 repeatFatal :failed :kept")
	add("supersede-lost", wire+"lost supersede :kept new :second")
	for _, n := range []string{"adoption-timeout", "negotiation-failure"} {
		add(n, "q prepare register prewire open row :H4 :unread")
	}
	add("stop-refused-ready-turn-takeover", waiting+"refuses bound :H3r ready foreign :H1r retry :H1r op :op accepts retry :H4 :byOp")
	add("StartedBy-row12-owed-stop", waiting+"bound!1.2 st :H4 :owed :row12")
	add("StartedBy-rule-repeated-and-write-fail", waiting+"rule st!1.1 :pending repair st :row12 :H2")
	add("failed-initial-settle", waiting+"down no ready :pending up repair drain :H4")
	add("H2-new-turn-deadline", waiting+"foreign bound :H4 :unread")
	add("H2-new-turn-success-cancels", waiting+"foreign ok :served :H1 end :H4")
	for _, p := range []string{held, waiting} {
		add("neverHold/"+p, p+"never :pause")
	}
	for _, p := range []struct{ n, s string }{{"HL-daemon-takes-over", "open row daemon :daemon"}, {"HL-ready-refused", "open row refuses ready :H3r accepts retry :owed"}, {"HL-registration", "op regDeadline :H4"}, {"HL-relaunch", "open row relaunchAgain :H4 :owed"}} {
		cs = append(cs, seed{p.n, launch, p.s})
	}
	add("A4-streaming-without-busy-text", ack+"open row streamLate :H1 :zeroCharge probe :H4")
	er := ready
	er.M.ErrRun = 1
	er.S = er.M
	cs = append(cs, seed{"clean-newest-prompted-report", er, "clean :noError"})
	add("delayed-fatal-after-success", held+"ok fatalReport!1.1 :pending repair :failed")
	add("expired-takeback-write-fails", waiting+"bound!1.1 :pending :noStop repair :H4 :owed")
	add("unconfirmed-forced-stop-refused", wire+"open row refuses bound :H3r accepts retry :H4")
	add("H4l/ordinary", ack+"op wait bound late :H4 :unread")
	add("H4l/takeover", waiting+"op bound late :H4 :kept")
	add("H4l/second-pause", waiting+"bound unpause resume open row late :H4 :kept")
	add("replacement-runtime-refused", waiting+"refuses stale :H4 :owed")
	add("paused-phase-daemon-controls", "phase prepare register w ack daemon wait bound :phaseEnd")
	add("non-root-exit", waiting+"exit :claimEnd :ended")
	add("wrong-token-before-H3-completion", "open row!1.2 :H3 :pending wrongToken :H3 :pending repair :H4")
	add("same-id-alias-negative-fanout", wire+"lost "+next+"open row detach late late1 reconnect replay :H2 replay :H4 :unread")
	add("separate-forwards-not-aliases", ack+"wait "+next+"ack1 open row no :H2 ok1 :served :H4")
	add("RS3/surviving-refusal-copy", waiting+"detach late late reconnect replay!1.1 boot reconnect row replay :unread :H4 :owed")
	add("non-wire-turn-cannot-wait-forever", "foreign release :H1 bound :H4")
	add("ended-pause-does-not-amend-second-stop", waiting+"bound unpause resume open row late :owed :H4")
	// Work is the agent's: a turn start, a streaming probe and a turn end each
	// end a busy answer's wait, and compaction ends with its own end.
	add("busy-then-turn-start", wire+"busyResult foreign :noWork")
	add("busy-in-turn-then-end", wire+"foreign busyResult end :idle")
	add("busy-probe-streams", wire+"busyResult stream :noWork :working")
	// A let-go replaces the process: none of the old one's work, turn, wake,
	// relaunch or send survives it, and a hold over the launch waits for ready.
	add("compact-death-relaunch", wire+"compact death :launch :noWork")
	add("compact-gone-relaunch", wire+"compact gone :launch :noWork")
	add("death-in-turn-relaunch", work+"death :launch :noAgent")
	add("gone-in-turn-relaunch", work+"gone :launch :noAgent")
	add("parked-wake-relaunch", "park wake :launch :noRelaunch")
	add("relaunch-sends-again", wire+"death :launch ready prepare :H0")
	add("relaunched-claim-holds-for-ready", wire+"death :launch op :HL")
	// A displaced process the runtime refused to stop can still start a turn;
	// resume's new process is not in it.
	add("resume-after-refused-displacement", waiting+"refuses stale :H4 foreign unpause resume :noAgent")
	// A stop's intent cancels the send it cut off; once a start row releases
	// the daemon's hold, the claim sends again at once.
	add("released-stop-sends-again", wire+"daemonDL refuses bound :H3r st :H0 prepare :H0")
	// A replay of the last report decides nothing once a report was applied.
	add("replayed-clean-keeps-error-run", "clean "+wire+"error :errRun1 report(promptedClean,replay) :errRun1")
	// A resend the shim dedupes onto a later root is that root's alias: the
	// root's answer closes it too.
	add("alias-of-later-root-closes", "q prepare register w ack no "+next+"lost1 prepare register w2 no1 :unread :noAttempt")
	// A start row that arrives while a stop's write is owed rides in that
	// write, whether the write is pending or retried after a restart.
	add("start-row-rides-pending-stop", waiting+"bound!1.2 down late st up save :row12 :H4")
	add("start-row-rides-owed-stop", waiting+"bound!1.2 boot :H4o down st up save :row12 :H4")
	// A second fatal report in a failure's lost-reply window is settled by
	// that failure, across a restart too.
	add("second-fatal-in-lost-reply-window", "q prepare register!4.1 fatalReport!2.1 down fatalReport boot up :failed")
	// The third model read's orders (dispatch://INFRA-3904/comment/
	// 7c0e8a9f-9b03-4099-a906-cbd4e94e6da0), each with the verdict the
	// architect's decision on that read requires.
	//
	// Rule 5: the agent's start waits in the shim's backlog, so no daemon has
	// read it, and the acknowledgement arms this prompt's own wait. The
	// replayed start then confirms the turn and ends that wait.
	add("third/ack-before-the-daemon-read-the-start", "q prepare register w detach start ack :unread :ownBound reconnect replay :confirmed :credited")
	// Rule 1: a parked wake and a stale-plugin relaunch are memory only
	// (machine.go:1823-1835, 1641-1648, 1775-1778). A boot forgets both, and
	// the boot's own refresh pass is what asks again.
	add("third/boot-forgets-a-parked-wake", work+"end park boot :noRelaunch :doubt park wake :doubt")
	add("third/boot-forgets-a-stale-relaunch", work+"stale :relaunch boot :noRelaunch :doubt")
	// Rule 3 (RT1): the result the old daemon's socket took is lost to this
	// daemon; only the recovery bound decides the task.
	add("third/old-socket-result-is-not-given-again", work+"end taken reconnect :doubt :kept rec :unread")
	// Rule 7 (M445): a turn credited through the first prompt holds the task
	// while that prompt is still open, so no newer task replaces it.
	add("third/credited-prompt-is-not-replaced", ack+"wait "+next+"ack1 start end wait new :kept :unread")
	cs = append(cs,
		seed{"launch-without-ready-bound", launch, "open row :HL bound :H4 :owed"},
		// A takeover's intent keeps the launch causes the stored intent
		// carries, though a restart forgot the hold that held the launch.
		seed{"intent-keeps-launch-causes-across-restart", launch, "open row refuses relaunchAgain :H3r boot :H3r ready row death :H3r retry :H4 :owed"},
		seed{"fatal-redelivery-before-recovered-intent", launch, "op death!2.1 fatalReport!3.1 drain :failed"},
		seed{"working-root-start", Start(PhWork, true), "op bound :H4"},
		seed{"working-worker-start", Start(PhWork, false), "daemonDL bound :H4"},
		// The daemon's hold ends in a start-row write whose reply is lost;
		// the release request's read-back installs it and starts a hold of
		// its own, which the old hold's deadline does not bound (depth 3,
		// Job legion-holdmodel-depth3-8fj4k at 87bd6c73).
		seed{"lost-start-row-reply-then-release", Start(PhWork, false), "daemonDL st!2.1 :daemon :pending release :release bound :H4"},
		seed{"launching-root-start", Start(PhLaunch, true), "op ready :H4"},
	)
	return cs
}
