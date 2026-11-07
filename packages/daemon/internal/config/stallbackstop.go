package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// stallBackstopKey is the per-role bound of the daemon's own stall backstop: how long after a
// claim's recorded last step (supervise.StepRecord) the daemon ends the turn that step belongs to
// and brings the session back with its task still owed (internal/daemon's backstopStalledTurns).
// It is a mapping of a Legion role to a decimal number of hours, the shape `worker_backends` and
// `role_config_files` give a per-role key, and like them it may name the controller.
const stallBackstopKey = "worker_stall_backstop_hours"

// StallLimits are the three numbers of the worker's own stall watch as this daemon's environment
// sets them, which it passes on to every session it starts (internal/daemon's
// stallLimitVariables): the grace over a call's own declared deadline, the bound for a call no
// deadline judges, and the gap between the recovery ladder's steps. Each is read from the same
// set of strings the plugin reads it from (`packages/pi-shared/src/stall-limits.ts`,
// integerSetting; stallMilliseconds): a whole number of milliseconds of 0 or more, and the
// plugin's own default for a variable the deployment leaves out or writes badly, which is the
// default the plugin would then run on.
//
// The daemon holds them for one purpose: its backstop bound must stay above the whole ladder a
// working plugin runs, and the file is refused for a bound below it (checkStallBackstops). They
// are no legion.yaml key, so no settings block carries them (api's settingsExcluded).
type StallLimits struct {
	// CallBound is LEGION_STALL_CALL_BOUND_MS where the deployment set one, and nil where it set
	// none. It is a pointer because one value set there is every role's — the daemon copies one
	// name from its own environment to every session — while the default it replaces is the
	// role's own (StallCallBoundFor), and nothing else tells the two apart.
	CallBound *time.Duration
	// CallGrace is LEGION_STALL_CALL_GRACE_MS, StallCallGrace where the deployment set none.
	CallGrace time.Duration
	// Step is LEGION_STALL_STEP_MS, StallStep where the deployment set none.
	Step time.Duration
}

// CallBoundFor is the bound the daemon reckons role's backstop from: the deployment's one value
// for every role where it set one, and the role's own default (StallCallBoundFor) where it set
// none — or where it set 0.
//
// 0 is not a bound of zero. It is the worker's call watch switched off for that worker: no call
// is judged overdue by the bound, so nothing is told and no step of the recovery ladder runs
// (`packages/pi-shared/src/stall-limits.ts`, the header over StallLimits). Reckoning the
// daemon's own numbers from that zero would make an implementer's backstop nine minutes, and
// nine minutes is a bound on healthy work: the Design sentence the backstop's floor comes from —
// its bound is set above the worker's whole ladder, so a working plugin always finishes first —
// is read for its purpose and not its arithmetic, and with the watch off there is no step of the
// plugin's for the daemon to finish after. All a nine-minute backstop could reach is a healthy
// worker in a legitimate long call. A deployment that switches the worker's watch off has asked
// the worker to stop acting; it has not asked the daemon to start ending healthy turns. So both
// the backstop's default and its floor read the role's own default bound here, exactly as though
// the deployment had set no bound at all.
func (l StallLimits) CallBoundFor(role claim.Role) time.Duration {
	if l.CallBound != nil && *l.CallBound > 0 {
		return *l.CallBound
	}
	return StallCallBoundFor(role)
}

// resolveStallLimits reads the three numbers from the daemon's own environment, as the plugin
// reads them from a worker's: a whole number of milliseconds of 0 or more, else the default. A
// value the plugin would refuse is the default here too, because the default is what the worker
// that reads the same variable will run on.
func resolveStallLimits(env func(string) string) StallLimits {
	limits := StallLimits{CallGrace: StallCallGrace, Step: StallStep}
	if bound, ok := stallMilliseconds(env("LEGION_STALL_CALL_BOUND_MS")); ok {
		limits.CallBound = &bound
	}
	if grace, ok := stallMilliseconds(env("LEGION_STALL_CALL_GRACE_MS")); ok {
		limits.CallGrace = grace
	}
	if step, ok := stallMilliseconds(env("LEGION_STALL_STEP_MS")); ok {
		limits.Step = step
	}
	return limits
}

// maxStallMilliseconds is the largest number of milliseconds a time.Duration holds, about 292
// years of them: the one place this reader and the plugin's part company (stallMilliseconds).
const maxStallMilliseconds = float64(math.MaxInt64 / int64(time.Millisecond))

// stallMilliseconds reads one LEGION_STALL_*_MS value from the same set of strings the plugin's
// own reader takes it from (`packages/pi-shared/src/stall-limits.ts`, integerSetting): the value
// is trimmed of JavaScript's whitespace and read as JavaScript's Number (jsNumber), and taken
// only when that is a whole number of 0 or more. Everything else answers false, which leaves the
// default standing, because the default is what the worker reading the same variable then runs
// on: an unset, empty or blank value, a fraction, a negative number, Infinity, a numeric
// separator, and every other string Number answers NaN for.
//
// Reading it any other way would have the daemon compute its floor from a bound no worker runs:
// `0x10`, ` 5 ` and `1.44e7` are each a number to the plugin and none of them to strconv.Atoi.
//
// The one string the two take differently is one too large for a time.Duration
// (maxStallMilliseconds), which the plugin takes and this leaves at the default: the daemon
// cannot hold the number at all, and no backstop bound could be held above a ladder of a hundred
// thousand years.
func stallMilliseconds(value string) (time.Duration, bool) {
	ms, ok := jsNumber(value)
	if !ok || ms != math.Trunc(ms) || ms < 0 || ms > maxStallMilliseconds {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// jsNumber is JavaScript's Number over a setting's string: its value, and false where Number
// answers NaN. Neither of Go's own readers is that set — strconv.Atoi refuses ` 5 `, `0x10` and
// `1.44e7`, each of which the plugin takes, and strconv.ParseFloat takes `nan`, `inf` and `1_0`,
// none of which it does — so the grammar is checked here and strconv is asked only for the
// value.
//
// A blank string is NaN rather than Number's own 0, because the plugin's reader answers NaN for
// one before it reads a number at all (integerSetting's `raw.trim() === ""`), and a variable set
// to spaces must be the default rather than the watch switched off.
func jsNumber(raw string) (float64, bool) {
	s := strings.TrimFunc(raw, jsSpace)
	if s == "" {
		return 0, false
	}
	if len(s) > 2 && s[0] == '0' {
		if base := jsBase(s[1]); base != 0 {
			// A sign and an underscore are both refused here as JavaScript refuses them in a
			// based literal: ParseUint permits neither when it is given its base.
			digits, err := strconv.ParseUint(s[2:], base, 64)
			if err != nil {
				return 0, false
			}
			return float64(digits), true
		}
	}
	if !jsDecimal(s) {
		return 0, false
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// jsBase is the base JavaScript reads a `0x`, `0o` or `0b` literal in, and 0 for every other
// second character.
func jsBase(prefix byte) int {
	switch prefix {
	case 'x', 'X':
		return 16
	case 'o', 'O':
		return 8
	case 'b', 'B':
		return 2
	}
	return 0
}

// jsSpace is a rune JavaScript trims from a string before it reads a number: its WhiteSpace and
// its LineTerminator. Go's unicode.IsSpace is a different set — it holds U+0085, which
// JavaScript does not trim — so the set is written out, with the space separators it shares.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', '\u2028', '\u2029', '\ufeff':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// jsDecimal is whether s is JavaScript's StrDecimalLiteral: an optional sign, then `Infinity` or
// digits with an optional fraction and an optional exponent, and nothing else after it. The
// value itself is strconv.ParseFloat's; this is the fence that keeps the strings ParseFloat
// reads and JavaScript does not from ever reaching it.
func jsDecimal(s string) bool {
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	if s == "Infinity" {
		return true
	}
	mantissa, exponent, exponentiated := strings.Cut(s, "e")
	if !exponentiated {
		mantissa, exponent, exponentiated = strings.Cut(s, "E")
	}
	if exponentiated {
		if exponent != "" && (exponent[0] == '+' || exponent[0] == '-') {
			exponent = exponent[1:]
		}
		if !jsDigits(exponent) {
			return false
		}
	}
	whole, fraction, dotted := strings.Cut(mantissa, ".")
	if whole == "" && fraction == "" {
		return false
	}
	if whole != "" && !jsDigits(whole) {
		return false
	}
	if dotted && fraction != "" && !jsDigits(fraction) {
		return false
	}
	return true
}

// jsDigits is whether s is one or more decimal digits and nothing else. A numeric separator is
// no digit: JavaScript's Number refuses `1_000`, where Go's own literals allow it.
func jsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// StallLadderFor is the whole of what a working plugin can legitimately take for role before
// anything is left for the daemon to do, and so the floor of the daemon's own backstop bound
// (checkStallBackstops): the longest a call of that worker can run unjudged, the grace over it,
// and the two gaps between the ladder's three recovery steps.
//
// The longest a call can run is the larger of two numbers, because a worker judges a call by
// whichever of them applies to it: the bound it holds a call no deadline judges to
// (StallLimits.CallBoundFor), and the hour the host clamps a declared deadline to
// (stalldefaults.go's StallDeclaredDeadlineCap). A deployment that shortens the bound to half an
// hour has not shortened the deadline a `pty` call may declare, so a floor reckoned from the
// bound alone would let the daemon end a working call an hour into its declared `timeout 3600`.
func (c Config) StallLadderFor(role claim.Role) time.Duration {
	return max(c.StallLimits.CallBoundFor(role), StallDeclaredDeadlineCap) + c.StallLimits.CallGrace + 2*c.StallLimits.Step
}

// StallBackstopFor is how long after a claim's recorded last step the daemon ends role's turn
// itself: the bound `worker_stall_backstop_hours` names for that role, and twice the role's own
// bound (StallLimits.CallBoundFor) where the file names none, which is what the approved spec's
// Design says — the daemon's backstop bound is twice the role's, so the worker acts first
// wherever it can act at all. That is eight hours for an implementer and a reviewer and two for
// every other role and the controller, on the shipped numbers.
//
// It is never below the floor every named bound must clear (StallLadderFor), which twice the
// role's bound is only where a deployment has stretched the grace or the step gaps past the
// bound itself: a default below that floor would have the daemon do the very thing the file is
// refused for. 0 is the backstop off for that role, which only the file can ask for: no sweep
// acts on a role whose bound is 0.
func (c Config) StallBackstopFor(role claim.Role) time.Duration {
	if bound, named := c.WorkerStallBackstops[role]; named {
		return bound
	}
	return max(2*c.StallLimits.CallBoundFor(role), c.StallLadderFor(role))
}

// readStallBackstops is `worker_stall_backstop_hours`: a mapping of a Legion role
// (RoleConfigRoles, the controller among them) to the number of hours the daemon's backstop waits
// after that role's last recorded step, a decimal among them as every other hours key is
// (readHours). 0 is the backstop off for that role and is the one reason this key reads its own
// numbers rather than calling readHours, which refuses 0 because every key it serves arms a timer
// that a zero would fire at once. A role named twice, a name that is not a Legion role, a value
// that is no number and a negative value are each refused naming the entry.
func readStallBackstops(value *yaml.Node, key string) (map[claim.Role]time.Duration, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a role to the hours the daemon's backstop waits, e.g. {implementer: 6, tester: 0}", key)
	}
	bounds := map[claim.Role]time.Duration{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, hoursNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(RoleConfigRoles, role) {
			return nil, fmt.Errorf("%s names %q, which is not a Legion role (%s)", key, roleNode.Value, runtime.JoinRoles(RoleConfigRoles))
		}
		if _, seen := bounds[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		field := key + "." + string(role)
		var hours float64
		if hoursNode.Kind != yaml.ScalarNode || (hoursNode.Tag != "!!int" && hoursNode.Tag != "!!float") || hoursNode.Decode(&hours) != nil {
			return nil, fmt.Errorf("%s must be a number of hours, or 0 to switch the backstop off for that role", field)
		}
		if hours < 0 {
			return nil, fmt.Errorf("%s must be 0 (the backstop off for that role) or a positive number of hours", field)
		}
		if hours > maxTimerSeconds/3600 {
			return nil, fmt.Errorf("%s must be at most %d", field, maxTimerSeconds/3600)
		}
		bound := time.Duration(math.Round(hours * float64(time.Hour)))
		if hours > 0 && bound <= 0 {
			return nil, fmt.Errorf("%s must be 0 (the backstop off for that role) or a positive number of hours", field)
		}
		bounds[role] = bound
	}
	return bounds, nil
}

// checkStallBackstops refuses a backstop bound below the whole ladder the role's own plugin runs
// (StallLadderFor), once the daemon's environment is settled: the longest a call of that worker
// can legitimately run — the larger of the bound a call no deadline judges runs on and the hour
// the host clamps a declared deadline to — plus the grace over it and the two gaps between the
// ladder's steps. A shorter bound would have the daemon end a turn while the plugin it exists to
// back up is still recovering it, or while a call of its own declared deadline is still running,
// and the daemon can see neither, so the floor is the whole of what keeps the two apart. The
// message names which of the two numbers the floor was reckoned from, since neither is in the
// file the operator is reading. 0 is the backstop off for that role and is refused nothing.
func checkStallBackstops(cfg Config) error {
	for _, role := range RoleConfigRoles {
		bound, named := cfg.WorkerStallBackstops[role]
		if !named || bound == 0 {
			continue
		}
		ladder := cfg.StallLadderFor(role)
		if bound >= ladder {
			continue
		}
		called := cfg.StallLimits.CallBoundFor(role)
		from := fmt.Sprintf("the %s bound a call no deadline judges runs on", called)
		if StallDeclaredDeadlineCap > called {
			from = fmt.Sprintf("the hour the host clamps a call's own declared deadline to, longer here than the %s bound a call no deadline judges runs on", called)
		}
		return fmt.Errorf("%s.%s (%s) is below the whole stall ladder the %s's own plugin runs (%s + %s + 2 × %s = %s, where the first number is %s): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role",
			stallBackstopKey, role, bound, role,
			max(called, StallDeclaredDeadlineCap), cfg.StallLimits.CallGrace, cfg.StallLimits.Step, ladder, from)
	}
	return nil
}
