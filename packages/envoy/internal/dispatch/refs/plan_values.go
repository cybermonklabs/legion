package refs

import (
	"fmt"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The values a plan node holds of its opening line (R18 of the issue tree's spec): each attribute
// plan_task declares that points at nothing is a value of the node under the attribute's name,
// whose value is the text the line writes for it, but for a `minutes` attribute, `size`, which the
// node holds as its whole number of minutes (pmdoc.Minutes), so `1h30m` is the value `90`. A
// pointer attribute - a `block[]` one, as serves, needs and parent are, or an `ask` one - stays a
// tuple of its kind, which the reference pass states. So the store keeps an attribute a later
// schema declares on plan_task with no change here. planBlockValues states each value and
// retracts it once the line stops writing it; the ordered read (PlanOrder) and the tree read
// answer them.
const (
	// AttributePlanSkills is the skills a plan node's implementer should read, a JSON array of
	// skill names as its opening line writes a `string[]` attribute.
	AttributePlanSkills = "skills"
	// AttributePlanDocs is the repository paths a plan node's implementer should read, a JSON
	// array of paths as its opening line writes a `string[]` attribute.
	AttributePlanDocs = "docs"
)

// planValueNames is the name of each attribute the installed schema declares on plan_task that is
// a value of a plan node (planValueKind) and that its opening line writes (InMarkdown), in
// declaration order: what heldBlockValues reads of every block a pass places, so a value a block
// holds is retracted once its line stops writing it, or once the block is no plan node.
func planValueNames() []string {
	attributes, _ := pmdoc.BlockTypeAttributes(planTaskType)
	names := make([]string, 0, len(attributes))
	for _, attribute := range attributes {
		if planValueKind(string(attribute.Kind)) && attribute.InMarkdown() {
			names = append(names, attribute.Name)
		}
	}
	return names
}

// planValueKind reports whether an attribute of kind is a value of a plan node: one that points at
// nothing, which is every kind but `block[]` and `ask`.
func planValueKind(kind string) bool {
	return kind != string(pmdoc.BlockAttributeBlockList) && kind != string(pmdoc.BlockAttributeAsk)
}

// blockPlanValues is the value of each attribute block's opening line writes that is a value of
// its node (planValueKind), by the attribute's name, as the render read the line
// (model.ArtifactBlock.Attributes): the text the line writes, and the whole number of minutes a
// `minutes` attribute's text stands for. It is nil for a block of any other type and for a
// plan_task whose line writes none, and it refuses a duration the schema does not take, which no
// version the parse admitted holds.
func blockPlanValues(block model.ArtifactBlock) (map[string]string, error) {
	if block.Type != planTaskType {
		return nil, nil
	}
	var values map[string]string
	for _, attribute := range block.Attributes {
		if !planValueKind(attribute.Kind) {
			continue
		}
		value := attribute.Value
		if attribute.Kind == string(pmdoc.BlockAttributeMinutes) {
			minutes, err := pmdoc.Minutes(value)
			if err != nil {
				return nil, fmt.Errorf("plan node %s writes the attribute %s: %w", block.ID, attribute.Name, err)
			}
			value = strconv.Itoa(minutes)
		}
		if values == nil {
			values = make(map[string]string, len(block.Attributes))
		}
		values[attribute.Name] = value
	}
	return values, nil
}
