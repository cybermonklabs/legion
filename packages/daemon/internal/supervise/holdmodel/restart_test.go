package holdmodel

func restartSeeds() []seed {
	var cs []seed
	add := func(n, s string) { cs = append(cs, seed{n, ready, s}) }
	for _, p := range []struct{ n, s string }{{"send", wire}, {"outstanding", ack}, {"running", work}} {
		for _, result := range []string{"ok", "no"} {
			for _, after := range []bool{false, true} {
				pre := p.s + "boot :doubt "
				order := "backlog"
				if after {
					pre += "reconnect hello " + result
					order = "live"
				} else {
					pre += result + " reconnect hello replay"
				}
				end := " :single :served"
				if result == "no" {
					end = " :single :unread "
					if p.n == "running" {
						end += "end "
					}
					end += next + ":again"
				}
				add("D10/"+p.n+"/"+order+"/"+result, pre+end)
			}
		}
	}
	add("RS1/boot-pause-row", work+"open row boot end ok reconnect row replay replay :served :single :H4")
	add("RS2/readopt-live-turn", work+"boot reconnect stream :provisional end ok :served :single")
	add("RT1/old-socket/H0", work+"end taken reconnect :doubt rec "+next+":again :rt1")
	add("RT2/overflow/H0", work+"end boot ok overflow reconnect rec "+next+":again :rt2")
	add("RT3/process-ended", work+"gone ready reconnect "+next+":again :rt3")
	add("RT4/accepted-but-not-durable", work+"end ok!3.1 reconnect rec "+next+":again :rt4")
	add("RT5/late-success", work+"end boot reconnect rec "+next+"ok :again :rt5 :kept")
	add("lost-send/cost-at-bound", wire+"lost open row bound unpause resume "+next+":again :lostCost")
	add("RS3/unconsumed-copy", waiting+"late!1.1 boot reconnect row :doubt bound :unread :owed")
	add("RS4/stop-commit-reply-lost", waiting+"bound!2.2 boot :H4 :owed")
	add("M1/crash-after-prepare", "q prepare boot reconnect prepare register w1 :single")
	add("M1/crash-before-registration", "q prepare register!3.1 reconnect prepare register w1 :single")
	add("M1/registered-never-written", "q prepare register boot reconnect :doubt rec prepare register w1 :single")
	add("M1/wire-before-outcome", wire+"boot ok reconnect replay :single :served")
	legacy := ready
	legacy.S.Task = Task{L: 1, ID: 1, Unknown: true}
	legacy.M = legacy.S
	cs = append(cs, seed{"D12/nullable-pre-migration-row", legacy, "boot reconnect :doubt rec :unread prepare register w :single"})
	add("earlier-id/prior-news", ack+"wait boot reconnect ok :single :served")
	add("Q-first-on-restored-working", work+"boot new :doubt :kept")
	add("streaming-confirms-restored-unconfirmed", wire+"boot reconnect stream :provisional")
	add("unnamed-start-after-restore", wire+"boot reconnect foreign :provisional")
	add("end-pause-leaves-H0-bound", waiting+"boot unpause :H0 :doubt reconnect rec :unread")
	add("reboot-does-not-extend-bound", ack+"boot boot reconnect rec :unread")
	add("accepted-fatal-crash-before-write", waiting+"fatal!3.1 drain :failed :kept")
	add("fatal-no-task-durable-redelivery", "open fatalReport!3.1 drain failEffects :failed")
	add("lost-queue-reply-adopt-meta", "open q!2.1 row :owed :kept")
	add("lost-retirement-reply-no-resurrection", waiting+"ok!2.1 repair :served :H4")
	add("wake-consumed-crash-before-commit", "foreign open row park wake!5.1 :H4o save :owed")
	cs = append(cs, seed{"HL-launch-crash-before-commit", launch, "open row ready!5.2 :H4o save :owed"})
	add("stop-permanent-write-error", waiting+"bound!1.2 :H4o down save :H4o up save :H4 :owed")
	add("stop-failed-precommit-crash", waiting+"bound!1.2 boot :H4o save :owed")
	add("detached-alias-response", wire+"lost "+next+"ack1 start named1 no :alias end :unread")
	add("lost-exact-success-before-boot", wire+"lost ok :single :served")
	return cs
}
