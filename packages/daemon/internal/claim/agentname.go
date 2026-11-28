package claim

import (
	"strconv"
	"strings"
)

// maxAgentName is the longest name of an agent, in bytes: the bound of roleGrammar, which an
// agent's name takes.
const maxAgentName = 32

// FirstAgentNumber is the number of the first agent of a role that takes a numbered name
// (NumberedAgentName): the first agent of a role takes the role's own name (RoleAgentName), so
// the next is `<role>-2`.
const FirstAgentNumber = 2

// RoleAgentName is the name an agent takes from the role of its first step where no other agent
// holds it (record.NameAgents): the role, cut to maxAgentName bytes.
func RoleAgentName(role Role) Agent {
	name := string(role)
	if len(name) > maxAgentName {
		name = name[:maxAgentName]
	}
	return Agent(name)
}

// NumberedAgentName is the name of the n-th agent of role, from FirstAgentNumber, where the role's
// own name is held (record.NameAgents): the role, cut so that the name holds at most maxAgentName
// bytes, a dash and n.
func NumberedAgentName(role Role, n int) Agent {
	suffix := "-" + strconv.Itoa(n)
	name := string(role)
	if len(name)+len(suffix) > maxAgentName {
		name = name[:maxAgentName-len(suffix)]
	}
	return Agent(name + suffix)
}

// NumberedAgentRole is the role of roles whose numbered agent name is (NumberedAgentName, from
// FirstAgentNumber), and false where it is none's: the rule read back, so a name an agent of an
// issue's plan can take beside its role's own name is told from one no agent can take. Only a
// workflow role (IsRole) has agents of a plan: the controller's claim names no agent of an issue.
// It is also the rule that keeps a role from being named so: the settings loader refuses a role of
// the deployment, and plan.Validate a role of a plan, whose name it reads as another role's, since
// an agent of each role would then hold one claim token. A role is no numbered agent of itself: a
// name that is one of roles is read as the numbered agent of another of them only.
func NumberedAgentRole(name string, roles []Role) (Role, bool) {
	cut := strings.LastIndexByte(name, '-')
	if cut < 0 {
		return "", false
	}
	n, err := strconv.Atoi(name[cut+1:])
	if err != nil || n < FirstAgentNumber {
		return "", false
	}
	for _, role := range roles {
		if role != Role(name) && IsRole(role) && NumberedAgentName(role, n) == Agent(name) {
			return role, true
		}
	}
	return "", false
}
