package holdmodel

func orderSeeds() []seed {
	cs := []seed{}
	add := func(n, s string) { cs = append(cs, seed{n, ready, s}) }
	t := func(n, s string) { add("PRRT_kwDORFy7ds68"+n, s) }
	t("CUsn", wire+"open row refused :charge1 :owed :H4 :kept")
	t("F5Q2", ack+"wait "+next+"open row refuses refused1 :atLimit no :H3r :atLimit :kept ready accepts retry :H4")
	t("CUsr", wire+"start hello stale end lost :launch :kept")
	t("F5RA", ack+"wait "+next+"start hello stale end lost1 :relaunch no :launch")
	t("C-Hi", wire+"open row lost :H2 bound :kept")
	t("C-Hm", waiting+"no :H4 :unread")
	t("F5Q7", ack+"wait start open row end no!1.1 :pending repair :H4")
	t("F5RH", held+"end :H2 ok :H4")
	t("F5RM", ack+"open gate :owed :H4")
	t("F5RQ", ack+"op :op :H2")
	t("F5RT", waiting+"park wake :owed :H4")
	t("F5RX", held+"end :H2 late :H4")
	t("GiMG", waiting+"park wake!1.1 :pending :noStop repair :owed")
	t("GiMH", wire+"hello refused "+next+":again")
	t("CUs1", held+"end ok :served :H4")
	add("pause-control/stale-at-turn-end", held+"stale end :H2 error :H4 :owed")
	add("death-after-failed-takeback", waiting+"no!1.1 death :kept :H4 :owed")
	add("dirty-recovery-stale-ack", wire+"compact!1.1 ack :unread compactEnd :unread")
	add("refused-stop-unrelated", ack+"open row refuses wait bound wrongToken ready :H3r accepts retry :H4")
	for _, p := range []string{"lost end", "end lost"} {
		add("lost-vs-turn-end/"+p, wire+"start open row "+p+" :H2 ok :served")
	}
	add("unanswered-wrong-id-compaction", wire+"compact foreign ack end compactEnd :unread")
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
	add("stop-refused-ready-turn-takeover", waiting+"refuses bound :H3r ready foreign :H1r retry :H1r op :op accepts retry :H4")
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
	add("non-root-exit", waiting+"exit :claimEnd")
	add("wrong-token-before-H3-completion", "open row!1.2 :H3 :pending wrongToken :H3 :pending repair :H4")
	add("same-id-alias-negative-fanout", wire+"lost "+next+"open row detach late late1 reconnect replay :H2 replay :H4 :unread")
	add("separate-forwards-not-aliases", ack+"wait "+next+"ack1 open row no :H2 ok1 :served :H4")
	add("RS3/surviving-refusal-copy", waiting+"detach late late reconnect replay!1.1 boot reconnect row replay :unread :H4 :owed")
	add("non-wire-turn-cannot-wait-forever", "foreign release :H1 bound :H4")
	add("ended-pause-does-not-amend-second-stop", waiting+"bound unpause resume open row late :owed :H4")
	cs = append(cs,
		seed{"launch-without-ready-bound", launch, "open row :HL bound :H4 :owed"},
		seed{"fatal-redelivery-before-recovered-intent", launch, "op death!2.1 fatalReport!3.1 drain :failed"},
		seed{"working-root-start", Start(PhWork, true), "op bound :H4"},
		seed{"working-worker-start", Start(PhWork, false), "daemonDL bound :H4"},
		seed{"launching-root-start", Start(PhLaunch, true), "op ready :H4"},
	)
	return cs
}
