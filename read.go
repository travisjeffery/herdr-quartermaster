package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// showNotesTailLines is how many recent non-empty note lines show --json
// surfaces in notes_tail alongside the full notes field.
const showNotesTailLines = 5

// showOutput is the JSON shape emitted by `quartermaster show <bead> --json`.
// Core field names match `bd show --json` (`id`, `title`, `status`, `notes`,
// and metadata); `notes_tail` provides the last few non-empty note lines for
// compact verification reads.
type showOutput struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Status       string       `json:"status"`
	Priority     int          `json:"priority"`
	Type         string       `json:"issue_type,omitempty"`
	Assignee     string       `json:"assignee,omitempty"`
	Parent       string       `json:"parent,omitempty"`
	Labels       []string     `json:"labels,omitempty"`
	Description  string       `json:"description,omitempty"`
	Notes        string       `json:"notes,omitempty"`
	NotesTail    []string     `json:"notes_tail,omitempty"`
	CreatedAt    time.Time    `json:"created_at,omitzero"`
	UpdatedAt    time.Time    `json:"updated_at,omitzero"`
	ClosedAt     time.Time    `json:"closed_at,omitzero"`
	CloseReason  string       `json:"close_reason,omitempty"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// memoryOutput is the JSON shape emitted by `quartermaster memory get <key> --json`.
type memoryOutput struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func showCmd(show func(string) (Bead, error), pos []string, asJSON bool, out io.Writer) error {
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return errors.New("usage: quartermaster show <bead> [--json]")
	}
	b, err := show(pos[0])
	if err != nil {
		return err
	}
	if asJSON {
		payload := buildShowOutput(b)
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n", data)
		return err
	}
	_, err = io.WriteString(out, formatBeadShow(b))
	return err
}

func memoryCmd(recall func(string) (string, error), pos []string, asJSON bool, out io.Writer) error {
	if len(pos) == 0 {
		return errors.New("usage: quartermaster memory get <key> [--json]")
	}
	if pos[0] != "get" {
		return fmt.Errorf("unknown memory command %q (writes stay in bd; usage: quartermaster memory get <key> [--json])", pos[0])
	}
	if len(pos) != 2 || strings.TrimSpace(pos[1]) == "" {
		return errors.New("usage: quartermaster memory get <key> [--json]")
	}
	key := pos[1]
	val, err := recall(key)
	if err != nil {
		return err
	}
	if asJSON {
		data, err := json.MarshalIndent(memoryOutput{Key: key, Value: val}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n", data)
		return err
	}
	_, err = fmt.Fprintln(out, strings.TrimSuffix(val, "\n"))
	return err
}

func buildShowOutput(b Bead) showOutput {
	lines := noteLines(b.Notes)
	var tail []string
	if len(lines) > 0 {
		tail = lines[max(0, len(lines)-showNotesTailLines):]
	}
	return showOutput{
		ID:           b.ID,
		Title:        b.Title,
		Status:       b.Status,
		Priority:     b.Priority,
		Type:         b.Type,
		Assignee:     b.Assignee,
		Parent:       b.Parent,
		Labels:       b.Labels,
		Description:  b.Description,
		Notes:        b.Notes,
		NotesTail:    tail,
		CreatedAt:    b.CreatedAt,
		UpdatedAt:    b.UpdatedAt,
		ClosedAt:     b.ClosedAt,
		CloseReason:  b.CloseReason,
		Dependencies: nonEmptyDeps(b.Dependencies),
	}
}

func nonEmptyDeps(deps []Dependency) []Dependency {
	var out []Dependency
	for _, d := range deps {
		if d.DependsOn != "" || d.Type != "" {
			out = append(out, d)
		}
	}
	return out
}

func formatBeadShow(b Bead) string {
	var s strings.Builder
	head := []string{b.ID, b.Status, fmt.Sprintf("P%d", b.Priority)}
	if b.Type != "" {
		head = append(head, b.Type)
	}
	if b.Assignee != "" {
		head = append(head, "assignee "+b.Assignee)
	}
	if b.Parent != "" {
		head = append(head, "parent "+b.Parent)
	}
	if len(b.Labels) > 0 {
		head = append(head, "labels "+strings.Join(b.Labels, ", "))
	}
	fmt.Fprintf(&s, "%s\n", strings.Join(head, " · "))
	if b.Title != "" {
		fmt.Fprintf(&s, "%s\n", b.Title)
	}
	var when []string
	if !b.CreatedAt.IsZero() {
		when = append(when, "created "+b.CreatedAt.UTC().Format(time.RFC3339))
	}
	if !b.UpdatedAt.IsZero() {
		when = append(when, "updated "+b.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if !b.ClosedAt.IsZero() {
		when = append(when, "closed "+b.ClosedAt.UTC().Format(time.RFC3339))
	}
	if len(when) > 0 {
		fmt.Fprintf(&s, "%s\n", strings.Join(when, " · "))
	}
	if r := strings.TrimSpace(b.CloseReason); r != "" {
		fmt.Fprintf(&s, "\nClose reason:\n%s\n", r)
	}
	if d := strings.TrimSpace(b.Description); d != "" {
		fmt.Fprintf(&s, "\nDescription:\n%s\n", d)
	}
	if deps := nonEmptyDeps(b.Dependencies); len(deps) > 0 {
		s.WriteString("\nDependencies:\n")
		for _, dep := range deps {
			if dep.Type != "" {
				fmt.Fprintf(&s, "- %s (%s)\n", dep.DependsOn, dep.Type)
			} else {
				fmt.Fprintf(&s, "- %s\n", dep.DependsOn)
			}
		}
	}
	if lines := noteLines(b.Notes); len(lines) > 0 {
		s.WriteString("\nNotes:\n")
		for _, l := range lines {
			fmt.Fprintf(&s, "- %s\n", l)
		}
	}
	return s.String()
}
