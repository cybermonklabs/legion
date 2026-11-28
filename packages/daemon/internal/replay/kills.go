package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// GuardKill is one kill of a process of an issue pod by the pod's memory guard, as one of its two
// records gives it: when, in which pod, the role whose process it was ("none" for a process
// outside every role) and the process's pid. A pod is named as the launcher names it,
// `legion-<project>-<issue>`, which is the part of the role's claim token before its role
// (claim.Token.Cut), so a kill of the daemon's line is named as the launcher names it.
type GuardKill struct {
	At   time.Time
	Pod  string
	Role string
	PID  int
}

// KillMatch is a window's two records of the memory guard's kills matched kill by kill
// (MatchKills): how many kills both hold, and each kill that one of them holds and the other
// lacks, in time order.
type KillMatch struct {
	Both         int
	LauncherOnly []GuardKill
	DaemonOnly   []GuardKill
}

// killMatchWithin is how far apart in time the two records of one kill may stand: the launcher
// writes its line as it ends the process and reports the kill to the daemon at once, and the
// daemon writes its line when the fact's transaction commits, so the two stand seconds apart, and
// one pod's process of one role and pid is ended once in a minute at the most.
const killMatchWithin = time.Minute

// MatchKills matches the two records of w's memory guard kills of its project's issue pods kill
// by kill: the launcher's lines (launcher/log.jsonl's `launcher: memory guard: ended`), the kills
// signal's record, and the daemon's lines of the kills the launchers reported (GuardKillLine) of
// the memory guard's own cause. Two kills match where they name the same pod, role and pid, at
// most killMatchWithin apart, each kill of the launcher's taking, in time order, the nearest kill
// of the daemon's that no earlier one took. It answers how many kills both hold and each kill one
// of them lacks. A window collected before collect kept the daemon's kill lines holds none of
// them, so every kill of its launcher's lines is one the daemon's lines lack; collect it again to
// read both. It refuses a window with no launcher's lines, which holds no record of the kills, and
// a line either reader refuses.
func MatchKills(w Window) (KillMatch, error) {
	if w.Launcher == nil {
		return KillMatch{}, errors.New("replay: the window holds no launcher's lines, the record of the memory guard's kills")
	}
	launcher, err := launcherKills(w.Launcher)
	if err != nil {
		return KillMatch{}, err
	}
	daemon, err := daemonKills(w.Log)
	if err != nil {
		return KillMatch{}, err
	}
	type identity struct {
		pod, role string
		pid       int
	}
	unmatched := make(map[identity][]int)
	for i, kill := range daemon {
		id := identity{kill.Pod, kill.Role, kill.PID}
		unmatched[id] = append(unmatched[id], i)
	}
	taken := make([]bool, len(daemon))
	var match KillMatch
	for _, kill := range launcher {
		id := identity{kill.Pod, kill.Role, kill.PID}
		nearest, gap := -1, killMatchWithin
		for _, i := range unmatched[id] {
			if apart := daemon[i].At.Sub(kill.At).Abs(); !taken[i] && apart <= gap {
				nearest, gap = i, apart
			}
		}
		if nearest < 0 {
			match.LauncherOnly = append(match.LauncherOnly, kill)
			continue
		}
		taken[nearest] = true
		match.Both++
	}
	for i, kill := range daemon {
		if !taken[i] {
			match.DaemonOnly = append(match.DaemonOnly, kill)
		}
	}
	return match, nil
}

// launcherKills is each kill the launcher's lines record, one for each memory guard line `ended`
// (MemoryGuardLine.Kill), in time order. It refuses a memory guard line ParseMemoryGuard refuses.
func launcherKills(lines []LauncherLine) ([]GuardKill, error) {
	var kills []GuardKill
	for i, line := range lines {
		guard, isGuard, err := ParseMemoryGuard(line.Text)
		if err != nil {
			return nil, fmt.Errorf("replay: line %d of %s: %w", i+1, launcherFile, err)
		}
		if !isGuard || !guard.Kill() {
			continue
		}
		pid, _ := guard.PID()
		kills = append(kills, GuardKill{At: line.Time, Pod: line.Pod, Role: guard.Role(), PID: pid})
	}
	slices.SortStableFunc(kills, func(a, b GuardKill) int { return a.At.Compare(b.At) })
	return kills, nil
}

// daemonKills is each kill of the memory guard's own cause the daemon's lines in log record
// (GuardKillLine), in time order. It refuses a line ParseLogLine refuses.
func daemonKills(log []json.RawMessage) ([]GuardKill, error) {
	var kills []GuardKill
	for i, raw := range log {
		line, err := ParseLogLine(raw)
		if err != nil {
			return nil, fmt.Errorf("replay: line %d of %s: %w", i+1, logFile, err)
		}
		told, ok := line.(GuardKillLine)
		if !ok || told.Cause != "" {
			continue
		}
		pod, _, _ := told.Claim.Token.Cut()
		kills = append(kills, GuardKill{At: told.Time, Pod: pod, Role: string(told.Claim.Role), PID: told.PID})
	}
	slices.SortStableFunc(kills, func(a, b GuardKill) int { return a.At.Compare(b.At) })
	return kills, nil
}
