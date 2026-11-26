package holdmodel

import "fmt"

func proofSeeds() []seed {
	var cs []seed
	add := func(n, s string) { cs = append(cs, seed{n, ready, s}) }
	o := "TestIndepPauseOrders/"
	for _, v := range []string{"", "end foreign "} {
		add(o+"ready-in-working/"+v, held+v+"ready :H1 error :kept")
	}
	for _, p := range []string{o + "gate-and-row-at-same-instant/", "TestIndepCompareGateOrders/"} {
		add(p+"gate-first", ack+"open park start end gate :H4 row error :owed :kept")
		add(p+"row-first", ack+"open park start row end gate row error :owed :kept")
	}
	for _, arm := range []string{"record-news", "clear-error-run", "confirm-from-result"} {
		pre := waiting
		if arm == "confirm-from-result" {
			pre = ack + "wait " + next + "busy1 open row probe "
		}
		s := ready
		if arm == "clear-error-run" {
			s.M.ErrRun = 1
			s.S = s.M
		}
		cs = append(cs, seed{o + "success-write-failed/" + arm, s, pre + "ok!1.1 :pending repair :served :H4 :noError"})
	}
	add(o+"replayed-refusal-of-older-prompt", ack+"wait "+next+"ack1 start1 open row end no1 detach late reconnect replay :H4 :kept")
	add(o+"replay-before-foreign-start", ack+"detach late reconnect replay foreign open row end :H4 :unread")
	for _, a := range []string{"no", "late", "start end error"} {
		add(o+"answered-send-confirms-later-foreign-turn/"+a, wire+a+" :rotated foreign open row end :unread :H4")
	}
	add(o+"busy-probe-overtakes-result", ack+"wait "+next+"start open row end busy1 :H1 probe :H2 error :kept :H4")
	add(o+"queued-start-despite-pause", waiting+"st new :kept error :owed")
	for _, by := range []string{"release", "op", "daemon"} {
		add(o+"another-stop-over-waiting-pause/"+by, waiting+by+" :H2 error :H4 :kept")
	}
	add(o+"fatal-unprompted-report-over-hold", waiting+"fatalReport error :failed :kept")
	cs = append(cs, seed{o + "resumable-root-exits-over-hold", Start(PhIdle, true), waiting + "exit error :H4 :kept"})
	add("TestIndepPauseEdges/late-stop-after-busy-unconfirmed", ack+"wait "+next+"busy1 probe open row :H2 ok :H4 :served")
	add("TestIndepPauseEdges/prompted-fatal-error", waiting+"fatal :failed :kept")
	for _, group := range []string{"TestIndepPauseRestart/", "TestIndepCompareRestartOutcome/"} {
		for _, a := range []struct{ n, p, x string }{{"provider", waiting, "error"}, {"no-turn", waiting, "no"}, {"waited-out", ack + "wait start open row end ", "no"}, {"late", waiting, "late"}, {"replayed", held + "end detach late reconnect ", "replay"}} {
			add(group+a.n, a.p+a.x+"!1.1 :pending :noStop boot row :kept bound :H4 :owed")
		}
	}
	// The four sites in report-C §10.3, not just the pause case that motivated them.
	for _, q := range []string{"q", "phase"} {
		for _, pause := range []bool{false, true} {
			base := q + " prepare register w ack start "
			hold := ""
			if pause {
				hold = "open row "
			}
			id := fmt.Sprintf("/%s/pause=%t", q, pause)
			for _, by := range []string{"op", "daemon", "release"} {
				// Only the daemon's own stop ends a phase (O1, S4): under a
				// pause's hold the daemon's request moves no holder.
				kept := " :kept"
				if q == "phase" && by == "daemon" && !pause {
					kept = " :phaseEnd"
				}
				add("F1/suspendNow/"+by+id, base+hold+"end "+by+" :H2 error :H4"+kept)
				add("F2/turnEnded/"+by+id, base+hold+by+" end :H2 ok :served :H4")
			}
			add("F3/queue"+id, base+hold+"end new :kept error :unread")
			add("F4/unpromptedTurnEnded"+id, base+hold+"end fatalReport :failed :kept")
		}
	}
	return cs
}
