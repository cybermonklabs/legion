package config

import (
	"slices"
	"strings"
	"testing"
)

// workflowFile is minimalFile with fields as DEMO's whole entry, and minimalFile itself for no
// fields, which is a project that names neither key.
func workflowFile(fields string) string {
	if fields == "" {
		return minimalFile
	}
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {"+fields+"}\n", 1)
}

// A project names its default workflow under `projects.<KEY>.workflow` and the workflows its
// architects may choose under `projects.<KEY>.workflows` (dispatch://INFRA-3570). A project that
// names neither runs the compiled default and offers nothing, so nothing changes for it until an
// operator opts it in: the empty default is the compiled one, and the empty list offers no choice
// at all, the default's own name included. Each key takes a name of the compiled library and
// nothing else, and a name it does not hold is refused at load naming the key and the value, as
// `production_check` refuses a word that is neither `off` nor `tester`; so is a list that names a
// workflow twice, which is the file saying one thing twice.
func TestProjectWorkflowSettingsTakeTheLibrarysNamesOnly(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		workflow     string
		workflows    []string
		refusal      string
	}{
		{name: "neither key"},
		{name: "a default of the library", fields: "workflow: light", workflow: "light"},
		{name: "the compiled default named", fields: "workflow: default", workflow: "default"},
		{name: "workflows offered", fields: "workflows: [light, no-planner]", workflows: []string{"light", "no-planner"}},
		{name: "both", fields: "workflow: no-planner, workflows: [default, light, no-planner]", workflow: "no-planner",
			workflows: []string{"default", "light", "no-planner"}},
		{name: "an empty list", fields: "workflows: []"},
		{name: "a default outside the library", fields: "workflow: heavy",
			refusal: `projects.DEMO.workflow must be one of default, light, no-planner (got "heavy")`},
		{name: "the empty default", fields: `workflow: ""`,
			refusal: `projects.DEMO.workflow must be one of default, light, no-planner (got "")`},
		{name: "a list naming a workflow outside the library", fields: "workflows: [light, Light]",
			refusal: `projects.DEMO.workflows[1] must be one of default, light, no-planner (got "Light")`},
		{name: "a list naming a workflow twice", fields: "workflows: [light, light]",
			refusal: `projects.DEMO.workflows names light twice`},
		{name: "a list that is not a list", fields: "workflows: light",
			refusal: `projects.DEMO.workflows must be an array of non-empty strings`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, workflowFile(tc.fields)), noEnv)
			if tc.refusal != "" {
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("Load error = %v, want %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			project := cfg.Projects["DEMO"]
			if project.Workflow != tc.workflow || !slices.Equal(project.Workflows, tc.workflows) {
				t.Fatalf("Workflow = %q, Workflows = %q; want %q and %q", project.Workflow, project.Workflows, tc.workflow, tc.workflows)
			}
		})
	}
}
