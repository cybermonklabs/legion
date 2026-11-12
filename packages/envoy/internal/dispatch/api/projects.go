package api

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// projectListQuery counts every project's open asks in one grouped read, in place of a subquery
// per project row. An ask belongs to the project of its issue, else of its document, as the
// inbox reads it; a project with no open ask has no row of that count, which the coalesce reads
// as the 0 the subquery answered. It is named here rather than written inside the handler so
// the plan test explains the statement the route runs.
const projectListQuery = `
		select p.key, p.name, coalesce(open_asks.total, 0), p.gated_steps, p.created_at
		from projects p
		left join (
			select coalesce(i.project_key, ar.project_key) as project_key, count(*) as total
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where a.state = 'open'
			  and (i.key is null or i.closed_at is null)
			group by coalesce(i.project_key, ar.project_key)
		) open_asks on open_asks.project_key = p.key
		order by p.key
	`

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), projectListQuery)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	projects := []model.Project{}
	for rows.Next() {
		var project model.Project
		if err := rows.Scan(&project.Key, &project.Name, &project.OpenAsks, &project.GatedSteps, &project.CreatedAt); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, projects)
}

// createProjectInput is the body of POST /api/v1/projects. DefaultAssignee is optional in the
// body and never optional in the row: a call that names none takes the person creating the
// project, and only a caller acting for nobody has to name one (createProject below).
type createProjectInput struct {
	Key             string       `json:"key" required:"true"`
	Name            string       `json:"name" required:"true"`
	DefaultAssignee string       `json:"default_assignee"`
	Actor           *model.Actor `json:"actor"`
}

// createdProject is the answer of POST /api/v1/projects: the project as created, and the
// default assignee the create stored, so the caller knows whom the project's unowned work lands
// on without a second read. The project list leaves the default out: GET
// /api/v1/settings/project-default-assignees, which needs the person scope, is the one read of
// every project's default, so a credential that route refuses reads no default off the list
// either. The project.created event carries the project alone.
type createdProject struct {
	model.Project
	DefaultAssignee string `json:"default_assignee"`
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	var input createProjectInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Key = strings.TrimSpace(input.Key)
	input.Name = strings.TrimSpace(input.Name)
	if !projectKeyPattern.MatchString(input.Key) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("key", input.Key))
		return
	}
	if input.Name == "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, "project name is required")
		return
	}
	// Any bearer writes a name into the project list every dashboard reads, and no route renames a
	// project, so a name is capped as an issue title is and shows a reader what it says.
	if length := itemblocks.Units16(input.Name); length > contracts.ProjectNameMax {
		capExceeded(w, "name", length, contracts.ProjectNameMax)
		return
	}
	if problem := projectNameProblem(input.Name); problem != "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, problem)
		return
	}
	// Every project names a person. The column refuses nothing
	// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql) because the
	// chain that decides who an issue's question reaches ends here, so the default is resolved
	// in two steps: the login the call names, then the person the creating credential acts for,
	// which is actorPerson (issue_assignee.go), the step defaultAssignee opens with, read off
	// the same actor.
	//
	// actingPerson is deliberately not called for it: it refuses a caller holding no `person`
	// scope (server.go), and this route is scopeDeployment (routes_table.go) — the deployment's
	// own token creates projects, and that token holds no scope at all. Such a caller acts for
	// nobody, so it must name the person itself, which is the refusal below. Whichever step
	// answers, the login is held to somebody who has signed in (allowedLogin), as PUT
	// /api/v1/projects/{key}/default-assignee holds one: a typo never becomes a stored default,
	// and neither does the owner a broker-issued token names, which the broker decides rather
	// than the people table.
	projectDefault := strings.TrimSpace(input.DefaultAssignee)
	if projectDefault == "" {
		if login, acts := actorPerson(actor); acts {
			projectDefault = login
		}
	}
	if projectDefault == "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest,
			"default_assignee is required: this credential acts for no person, so the call must name "+
				"the person a project's unowned work lands on")
		return
	}
	projectDefault, err := s.allowedLogin(r.Context(), projectDefault)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var created createdProject
	if err := tx.QueryRow(r.Context(), `
		insert into projects (key, name, default_assignee) values ($1, $2, $3)
		returning key, name, gated_steps, default_assignee, created_at
	`, input.Key, input.Name, projectDefault).Scan(
		&created.Key, &created.Name, &created.GatedSteps, &created.DefaultAssignee, &created.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			writeError(w, "PROJECT_EXISTS", http.StatusConflict, "a project with this key already exists")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(created.Key).event("project.created", actor, created.Project))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, created)
}

// projectNameProblem refuses a project name that would mislead its reader: one holding a control
// character, one holding a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), the
// controls a Trojan Source spoof reorders text with, and one with no visible character: no letter,
// number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and
// U+2800, and no code point Go's Unicode tables leave unassigned other than a noncharacter or a
// reserved default-ignorable (visibleCharacter). Every other format character and combining mark
// does visible work in real names and stays beside a visible one: the joiner inside an emoji, a
// flag's tag characters, a direction mark (U+200E, U+200F, U+061C) in mixed-direction text, a soft
// hyphen, an accent written as a combining mark. It answers "" for a name it takes.
func projectNameProblem(name string) string {
	visible := false
	for _, character := range name {
		switch {
		case unicode.Is(unicode.Cc, character):
			return fmt.Sprintf("name holds U+%04X, a control character", character)
		case (character >= 0x202A && character <= 0x202E) || (character >= 0x2066 && character <= 0x2069):
			return fmt.Sprintf("name holds U+%04X, a bidi control that reorders the text a reader sees", character)
		case visibleCharacter(character):
			visible = true
		}
	}
	if !visible {
		return "name has no visible character"
	}
	return ""
}

// blankGlyphs are characters Unicode files as letters or symbols that render as blank space: the
// Hangul fillers (U+115F, U+1160, U+3164, U+FFA0, category Lo) and the Braille pattern blank
// (U+2800, So). A name of only these looks empty wherever it is shown.
var blankGlyphs = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// visibleCharacter reports whether the character is one a reader sees: a letter, number,
// punctuation or symbol (Unicode L, N, P, S) other than the blank fillers, or a code point Go's
// Unicode tables leave unassigned (Cn), since a Unicode release newer than those tables may have
// made it a character the reader's device draws. Two kinds of unassigned code point are not
// visible: a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF), which
// Unicode's stability policy guarantees is never assigned, and a reserved default-ignorable
// (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF), the ranges Unicode sets aside for characters that
// render as nothing; that reservation is Unicode's allocation practice, not a
// stability guarantee. The two property tables hold exactly those unassigned code points. A code
// point a future Unicode assigns outside them as whitespace or a format character counts as visible
// here until Go's tables learn it. Combining marks, format characters, private-use characters (Co)
// and whitespace are not visible. A surrogate (Cs) never reaches here: ranging over a Go string
// yields U+FFFD for invalid UTF-8, which an encoded surrogate is, and JSON decoding writes a lone
// surrogate escape as U+FFFD too.
func visibleCharacter(character rune) bool {
	switch {
	case unicode.In(character, unicode.L, unicode.N, unicode.P, unicode.S):
		return !blankGlyphs[character]
	case unicode.Is(unicode.Cn, character):
		return !unicode.In(character, unicode.Noncharacter_Code_Point, unicode.Other_Default_Ignorable_Code_Point)
	}
	return false
}
