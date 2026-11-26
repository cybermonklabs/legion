// Package omprequest reads what a real Oh My Pi offers its model, for the tests that hold a launch
// of a folder of texts to it: Read takes one request body apart, and Standin is a model stand-in
// that records every body and answers a text turn or a tool call. It is a helper for tests, kept
// out of a _test file so the untagged real Oh My Pi tests and the e2e driver alike import it, and
// it imports only the standard library and the module's stable test helpers, since the e2e
// control copies it into a checkout of main.
package omprequest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request is what one request offers the model.
type Request struct {
	// System is the system prompt: the system blocks' text, joined by a blank line.
	System string
	// Skills are the skills the system prompt lists between `<skills>` and `</skills>`, in order.
	Skills []Skill
	// Agents are the agents the `task` tool's description lists under `# Available Agents`, in
	// order.
	Agents []Agent
	// DispatchFirst is each `<dispatch-first-skill>…</dispatch-first-skill>` block the messages
	// carry, tags included.
	DispatchFirst []string
}

// Skill is one line of the skill list, `- <name>: <description>`; the description is cut as Oh My
// Pi cuts it.
type Skill struct{ Name, Description string }

// Agent is one item of the task tool's agent list, `- `<name>`…: <description>`, its description
// spanning the lines up to the next item.
type Agent struct{ Name, Description string }

const (
	dispatchFirstOpen  = "<dispatch-first-skill>"
	dispatchFirstClose = "</dispatch-first-skill>"
)

// Read takes apart one Anthropic Messages request body as Oh My Pi sends it. A body that is no
// JSON object, or holds no system prompt or no messages, is no conversation request and is refused.
func Read(body []byte) (Request, error) {
	var raw struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Request{}, fmt.Errorf("read the request: %w", err)
	}
	system, err := textBlocks(raw.System)
	if err != nil {
		return Request{}, fmt.Errorf("read the request's system prompt: %w", err)
	}
	if len(system) == 0 {
		return Request{}, errors.New("the request holds no system prompt")
	}
	if len(raw.Messages) == 0 {
		return Request{}, errors.New("the request holds no messages")
	}
	request := Request{System: strings.Join(system, "\n\n")}
	request.Skills = skillList(request.System)
	// A pane's Oh My Pi offers its built-in tools to an Anthropic Messages endpoint under a name of
	// its own spelling, `_task` (measured in the texts-folder live proof's panes, whose first requests
	// offer `_read`, `_task`, `_legion` …), where the captured fixtures' Oh My Pi offered `task`: the
	// tool is the task tool under either spelling.
	for _, tool := range raw.Tools {
		if strings.TrimLeft(tool.Name, "_") == "task" {
			request.Agents = agentList(tool.Description)
		}
	}
	for _, message := range raw.Messages {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(message, &m); err != nil {
			return Request{}, fmt.Errorf("read a message of the request: %w", err)
		}
		parts, err := textBlocks(m.Content)
		if err != nil {
			return Request{}, fmt.Errorf("read a message of the request: %w", err)
		}
		for _, text := range parts {
			request.DispatchFirst = append(request.DispatchFirst, dispatchFirstBlocks(text)...)
		}
	}
	return request, nil
}

// textBlocks is the text of content as a request writes it: one string, or a list of blocks, of
// which the text blocks count. Absent content holds none.
func textBlocks(content json.RawMessage) ([]string, error) {
	if len(content) == 0 || string(content) == "null" {
		return nil, nil
	}
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		if plain == "" {
			return nil, nil
		}
		return []string{plain}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, err
	}
	var out []string
	for _, block := range blocks {
		if block.Type == "text" {
			out = append(out, block.Text)
		}
	}
	return out, nil
}

// skillList is the skills system lists between `<skills>` and `</skills>`.
func skillList(system string) []Skill {
	_, after, ok := strings.Cut(system, "<skills>\n")
	if !ok {
		return nil
	}
	list, _, _ := strings.Cut(after, "</skills>")
	var skills []Skill
	for _, line := range strings.Split(list, "\n") {
		item, ok := strings.CutPrefix(line, "- ")
		if !ok {
			continue
		}
		name, description, _ := strings.Cut(item, ": ")
		skills = append(skills, Skill{Name: name, Description: description})
	}
	return skills
}

// agentList is the agents description lists under `# Available Agents`: an item is a line that
// starts "- " and the name in backticks, and runs to the next item, a heading, or the end.
func agentList(description string) []Agent {
	_, after, ok := strings.Cut(description, "# Available Agents\n")
	if !ok {
		return nil
	}
	var agents []Agent
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(line, "#") {
			break
		}
		if rest, ok := strings.CutPrefix(line, "- `"); ok {
			if name, tail, ok := strings.Cut(rest, "`"); ok {
				_, about, _ := strings.Cut(tail, ": ")
				agents = append(agents, Agent{Name: name, Description: about})
				continue
			}
		}
		if len(agents) > 0 && line != "" {
			last := &agents[len(agents)-1]
			last.Description += "\n" + line
		}
	}
	return agents
}

// dispatchFirstBlocks are the dispatch-first blocks text holds, tags included.
func dispatchFirstBlocks(text string) []string {
	var blocks []string
	for {
		start := strings.Index(text, dispatchFirstOpen)
		if start < 0 {
			return blocks
		}
		length := strings.Index(text[start:], dispatchFirstClose)
		if length < 0 {
			return blocks
		}
		end := start + length + len(dispatchFirstClose)
		blocks = append(blocks, text[start:end])
		text = text[end:]
	}
}
