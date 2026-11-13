package config

import (
	"testing"
)

// stalled_tree_notice_hours refuses what every sibling key in hours refuses, and in the same
// words: a bound of zero, which would read as a notice for every tree of the project at every
// pass rather than as the notice turned off; a negative number, which is no bound at all; a
// number above the timer bound; and anything that is not a number, a quoted number included.
// Every other key in hours has such a test (linger_hours, production_check_hours, the two
// pod-update keys), and the refusals are the deployment's one guard against a daemon that tells
// its controller a tree has stopped the moment the tree is opened.
func TestStalledTreeNoticeHoursRefuses(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"0", "stalled_tree_notice_hours must be a positive number"},
		{"-6", "stalled_tree_notice_hours must be a positive number"},
		{".nan", "stalled_tree_notice_hours must be a positive number"},
		{"596.5", "stalled_tree_notice_hours must be at most 596"},
		{".inf", "stalled_tree_notice_hours must be at most 596"},
		{"soon", "stalled_tree_notice_hours must be a number"},
		{"\"6\"", "stalled_tree_notice_hours must be a number"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"stalled_tree_notice_hours: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
