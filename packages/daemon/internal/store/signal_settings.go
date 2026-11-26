package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// The settings of admission by signals (S3 of dispatch://INFRA-5899): per project, the switch and
// the limit of each of the six series, each with who set it last, when and the person's cited
// reason, as migrations/20261126072843_signal_settings.up.sql defines them (signal_settings). A
// project with no row is a fresh project: admission by signals off, and no limit. A person's change
// is applied as a fact (intake.SignalSettingChanged), and admission writes it in that fact's
// transaction (ChangeSignalSettingTx) before the fact's pass, which reads it, so a change is in
// force at once and a restart keeps it. The project is the project's Dispatch key.

// The parts of the provider's signal, which reads its headroom and its refusals apart: every
// surface of admission by signals names those two series by the signal provider and these parts.
const (
	PartHeadroom = "headroom"
	PartRefusals = "refusals"
)

// SignalSwitchSetting is the setting the switch of admission by signals is kept under, beside the
// limit of each series under the series' name of signals.Names().
const SignalSwitchSetting = "switch"

// SignalPart is the signal and the part every surface of admission by signals names a series by,
// for its name of signals.Names(): the provider's two series are the provider's headroom and
// refusals, and each other series is its own signal, with no part. It is the one map between the
// two namings, and SignalSeries is its inverse.
func SignalPart(series string) (signal, part string) {
	switch series {
	case signals.Provider:
		return signals.Provider, PartHeadroom
	case signals.Refusals:
		return signals.Provider, PartRefusals
	}
	return series, ""
}

// SignalSeries is the series of signals.Names() that signal and part name (SignalPart), and whether
// they name one: the provider needs a part, and every other signal takes none.
func SignalSeries(signal, part string) (string, bool) {
	for _, series := range signals.Names() {
		if named, of := SignalPart(series); named == signal && of == part {
			return series, true
		}
	}
	return "", false
}

// SignalSet is who changed a setting of admission by signals, when, and the person's cited reason
// for it: the controller on a person's word, or the operator. Its setter is `setBy` on the wire, as
// an entry of the record of changes names it (SignalChange.SetBy).
type SignalSet struct {
	By     PauseActor `json:"setBy"`
	At     time.Time  `json:"at"`
	Reason string     `json:"reason"`
}

// SignalSetting is one setting as the project holds it: its value (the switch 1 on and 0 off; a
// limit, nil where a person cleared it) and its last change.
type SignalSetting struct {
	Value *float64
	Set   SignalSet
}

// SignalSettings is a project's settings of admission by signals: the switch, nil before a person
// first set it, and each series' limit that a person set or cleared, by its name of
// signals.Names().
type SignalSettings struct {
	Switch *SignalSetting
	Limits map[string]SignalSetting
}

// On says whether admission by signals is on: a fresh project's is off.
func (s SignalSettings) On() bool {
	return s.Switch != nil && s.Switch.Value != nil && *s.Switch.Value == 1
}

// Limit is the limit set on series, and whether one is: a series no person set a limit of, and one
// whose limit a person cleared, has none.
func (s SignalSettings) Limit(series string) (float64, bool) {
	setting, set := s.Limits[series]
	if !set || setting.Value == nil {
		return 0, false
	}
	return *setting.Value, true
}

// SignalSettingChange is a person's change of one setting: Setting is SignalSwitchSetting or a
// series of signals.Names(), Value the new value (1 or 0 for the switch; the limit, nil to clear
// it), and Set who made it, when and why.
type SignalSettingChange struct {
	Setting string
	Value   *float64
	Set     SignalSet
}

// Validate says why change cannot be written, nil where it can: the setting is the switch or a
// series, the switch is 1 or 0, a limit is in its series' range (signals.CheckLimit, the one range
// of each), the actor is the controller with its session or the operator with none, the moment is
// set and the reason is not blank.
func (c SignalSettingChange) Validate() error {
	switch {
	case c.Setting == SignalSwitchSetting:
		if c.Value == nil || *c.Value != 0 && *c.Value != 1 {
			return fmt.Errorf("the switch of admission by signals is 1 or 0")
		}
	case slices.Contains(signals.Names(), c.Setting):
		if c.Value != nil {
			if err := signals.CheckLimit(c.Setting, *c.Value); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%q is no setting of admission by signals: it is the switch or a series of %v", c.Setting, signals.Names())
	}
	if err := c.Set.By.validate(); err != nil {
		return fmt.Errorf("a setting of admission by signals: %w", err)
	}
	if c.Set.At.IsZero() {
		return fmt.Errorf("a change of a setting of admission by signals names its moment")
	}
	if strings.TrimSpace(c.Set.Reason) == "" {
		return fmt.Errorf("a change of a setting of admission by signals needs the person's cited reason")
	}
	return nil
}

// entry is the change's entry of the record of changes (S6): the switch's, or the limit of its
// series, with who made it and why.
func (c SignalSettingChange) entry() SignalChange {
	by, reason := c.Set.By, c.Set.Reason
	entry := SignalChange{At: c.Set.At, What: SignalChangeLimit, Value: c.Value, SetBy: &by, Reason: &reason}
	if c.Setting == SignalSwitchSetting {
		entry.What = SignalChangeSwitch
		return entry
	}
	entry.Signal, entry.Part = seriesNames(c.Setting)
	return entry
}

// SignalSettingsTx is project's settings of admission by signals, read through tx.
func SignalSettingsTx(ctx context.Context, tx pgx.Tx, project string) (SignalSettings, error) {
	rows, err := tx.Query(ctx, `select setting, value, set_at, by_kind, by_session, reason from signal_settings where project = $1`, project)
	if err != nil {
		return SignalSettings{}, fmt.Errorf("read the settings of admission by signals of %s: %w", project, err)
	}
	settings := SignalSettings{Limits: map[string]SignalSetting{}}
	var name string
	var setting SignalSetting
	if _, err := pgx.ForEachRow(rows, []any{&name, &setting.Value, &setting.Set.At, &setting.Set.By.Kind, &setting.Set.By.Session, &setting.Set.Reason}, func() error {
		setting.Set.At = setting.Set.At.UTC()
		if name == SignalSwitchSetting {
			kept := setting
			settings.Switch = &kept
		} else {
			settings.Limits[name] = setting
		}
		setting = SignalSetting{}
		return nil
	}); err != nil {
		return SignalSettings{}, fmt.Errorf("read the settings of admission by signals of %s: %w", project, err)
	}
	return settings, nil
}

// ChangeSignalSettingTx writes change as project's, in the transaction of the fact it is applied as,
// where it moves the setting, and the change's entry of the record of changes with it
// (AppendSignalChangesTx), so the setting and its entry commit together or not at all. A change to
// the value the setting holds already, the switch turned on while it is on or a limit cleared that
// no person set, writes nothing, and it answers whether it wrote. A change Validate refuses is an
// error.
func ChangeSignalSettingTx(ctx context.Context, tx pgx.Tx, project string, change SignalSettingChange) (bool, error) {
	if err := change.Validate(); err != nil {
		return false, err
	}
	if change.Value == nil {
		var held bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from signal_settings where project = $1 and setting = $2 and value is not null)`,
			project, change.Setting).Scan(&held); err != nil {
			return false, fmt.Errorf("read the setting %s of %s: %w", change.Setting, project, err)
		}
		if !held {
			return false, nil
		}
	}
	tag, err := tx.Exec(ctx, `insert into signal_settings (project, setting, value, set_at, by_kind, by_session, reason)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (project, setting) do update set value = excluded.value, set_at = excluded.set_at, by_kind = excluded.by_kind,
			by_session = excluded.by_session, reason = excluded.reason
		where signal_settings.value is distinct from excluded.value`,
		project, change.Setting, change.Value, change.Set.At, string(change.Set.By.Kind), change.Set.By.Session, change.Set.Reason)
	if err != nil {
		return false, fmt.Errorf("write the setting %s of %s: %w", change.Setting, project, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := AppendSignalChangesTx(ctx, tx, project, []SignalChange{change.entry()}); err != nil {
		return false, err
	}
	return true, nil
}

// SignalSettingsView is a project's settings of admission by signals as the routes that change them
// answer: whether it is on, who set the switch last, when and why (null before a person first did),
// and each of the six series in the order of signals.Names(), named by its signal and part
// (SignalPart) with its limit (null for none) and its last change (null before a person first set
// or cleared it). They are read from the settings themselves, so a setting whose entry has left the
// state's newest entries reads as it stands.
type SignalSettingsView struct {
	On     bool              `json:"on"`
	Switch *SignalSet        `json:"switch"`
	Limits []SignalLimitView `json:"limits"`
}

// SignalLimitView is one series' limit of SignalSettingsView.
type SignalLimitView struct {
	Signal string     `json:"signal"`
	Part   *string    `json:"part"`
	Limit  *float64   `json:"limit"`
	Set    *SignalSet `json:"set"`
}

// View is the settings as SignalSettingsView shows them.
func (s SignalSettings) View() SignalSettingsView {
	view := SignalSettingsView{On: s.On(), Limits: make([]SignalLimitView, 0, len(signals.Names()))}
	if s.Switch != nil {
		set := s.Switch.Set
		view.Switch = &set
	}
	for _, series := range signals.Names() {
		signal, part := seriesNames(series)
		limit := SignalLimitView{Signal: *signal, Part: part}
		if setting, set := s.Limits[series]; set {
			last := setting.Set
			limit.Limit, limit.Set = setting.Value, &last
		}
		view.Limits = append(view.Limits, limit)
	}
	return view
}

// seriesNames is series' signal and part (SignalPart) as an entry and a view carry them: the part
// null where the series has none.
func seriesNames(series string) (*string, *string) {
	signal, part := SignalPart(series)
	if part == "" {
		return &signal, nil
	}
	return &signal, &part
}
