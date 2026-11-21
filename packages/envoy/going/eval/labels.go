package eval

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// The labels of labels.json are the calibration of the judge (R13): JudgeCalibrationAnswers
// answers of answer tasks, each labelled pass or fail against its task's facts by two agents of
// different model families, each on its own, and then agreed between them with the discussion
// that agreed it. The eval program puts each answer to the judge and prints how many of the
// agreed labels the judge's verdict meets, against JudgeAgreementFloor.

// The two labels an answer takes.
const (
	LabelPass = "pass"
	LabelFail = "fail"
)

// Labels is labels.json: who labelled, and each labelled answer.
type Labels struct {
	// Labellers are the two agents that labelled every answer, by the name each label gives, and
	// the model family each runs on.
	Labellers []Labeller       `json:"labellers"`
	Answers   []LabelledAnswer `json:"answers"`
}

// Labeller is one agent that labelled: its name, its model and the model's family.
type Labeller struct {
	Name   string `json:"name"`
	Model  string `json:"model"`
	Family string `json:"family"`
}

// LabelledAnswer is one answer of an answer task, its label and how the label was reached.
type LabelledAnswer struct {
	// Task is the task the answer answers, whose words and facts the judge reads it against.
	Task string `json:"task"`
	// Answer is the answer as the bar shows it.
	Answer string `json:"answer"`
	// Labels is each labeller's own label, by its name, with its reason, given before the two
	// compared.
	Labels []Label `json:"labels"`
	// Label is the label the two agreed, and Discussion how: empty where they gave it alike.
	Label      string `json:"label"`
	Discussion string `json:"discussion,omitempty"`
}

// Label is one labeller's label of an answer.
type Label struct {
	By    string `json:"by"`
	Label string `json:"label"`
	Why   string `json:"why"`
}

// LoadLabels reads labels.json from fsys (Files, for the committed labels) and refuses a file that
// is not the calibration R13 asks for: other than JudgeCalibrationAnswers answers, two labellers
// of one model family, an answer to no answer task of tasks, a label other than pass and fail, an
// answer each labeller did not label, or two own labels that differ with no discussion.
func LoadLabels(fsys fs.FS, tasks []Task) (*Labels, error) {
	var labels Labels
	if err := decodeFile(fsys, "labels.json", &labels); err != nil {
		return nil, err
	}
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if len(labels.Answers) != JudgeCalibrationAnswers {
		fail("it holds %d answers; the judge is calibrated on %d", len(labels.Answers), JudgeCalibrationAnswers)
	}
	if len(labels.Labellers) != 2 || strings.EqualFold(labels.Labellers[0].Family, labels.Labellers[1].Family) {
		fail("the answers are labelled by two agents of different model families, and these are %v", labels.Labellers)
	}
	names := make([]string, len(labels.Labellers))
	for i, l := range labels.Labellers {
		names[i] = l.Name
	}
	for i, answer := range labels.Answers {
		task := slices.IndexFunc(tasks, func(t Task) bool { return t.ID == answer.Task })
		switch {
		case task < 0:
			fail("answer %d answers %q, which is no task", i+1, answer.Task)
		case tasks[task].Kind.Changes():
			fail("answer %d answers %s, a task the judge does not grade", i+1, answer.Task)
		}
		if answer.Label != LabelPass && answer.Label != LabelFail {
			fail("answer %d is labelled %q, not pass or fail", i+1, answer.Label)
		}
		var by []string
		alike := true
		for _, l := range answer.Labels {
			by = append(by, l.By)
			if l.Label != LabelPass && l.Label != LabelFail {
				fail("answer %d is labelled %q by %s, not pass or fail", i+1, l.Label, l.By)
			}
			alike = alike && l.Label == answer.Label
		}
		slices.Sort(by)
		if sorted := slices.Sorted(slices.Values(names)); !slices.Equal(by, sorted) {
			fail("answer %d is labelled by %v, not once by each of %v", i+1, by, sorted)
		}
		if !alike && strings.TrimSpace(answer.Discussion) == "" {
			fail("answer %d's labels differ, and no discussion says how %s was agreed", i+1, answer.Label)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("eval: labels.json: %s", strings.Join(problems, "; "))
	}
	return &labels, nil
}
