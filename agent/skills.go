package agent

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Skill is one packaged skill bundle: a SKILL.md with YAML-subset
// frontmatter and a markdown body. Skills are advertised to the model as
// one-line summaries in the system prompt; the full body is loaded on
// demand through the load_skill tool, so the stable prompt stays small while
// skill bodies can be arbitrarily long.
type Skill struct {
	ID            string
	Name          string
	Description   string
	Version       int
	ToolAllowlist []string
	Body          string
}

// SkillLibrary holds the registered skills keyed by ID, preserving
// registration order for deterministic listing.
type SkillLibrary struct {
	byID map[string]*Skill
	ids  []string
}

// NewSkillLibrary builds a library from in-memory skills. Duplicate IDs
// and missing required fields are errors.
func NewSkillLibrary(skills ...*Skill) (*SkillLibrary, error) {
	lib := &SkillLibrary{byID: make(map[string]*Skill, len(skills))}
	for _, s := range skills {
		if s == nil {
			continue
		}
		if s.ID == "" || s.Name == "" || s.Description == "" {
			return nil, fmt.Errorf("skill %q: id, name and description are required", s.ID)
		}
		if _, dup := lib.byID[s.ID]; dup {
			return nil, fmt.Errorf("duplicate skill id %q", s.ID)
		}
		if s.Version == 0 {
			s.Version = 1
		}
		lib.byID[s.ID] = s
		lib.ids = append(lib.ids, s.ID)
	}
	return lib, nil
}

// Get returns the skill with the given ID, if registered. Nil-safe.
func (l *SkillLibrary) Get(id string) (*Skill, bool) {
	if l == nil {
		return nil, false
	}
	s, ok := l.byID[id]
	return s, ok
}

// List returns the registered skills in registration order. Nil-safe.
func (l *SkillLibrary) List() []*Skill {
	if l == nil {
		return nil
	}
	out := make([]*Skill, 0, len(l.ids))
	for _, id := range l.ids {
		out = append(out, l.byID[id])
	}
	return out
}

// Len reports the number of registered skills. Nil-safe.
func (l *SkillLibrary) Len() int {
	if l == nil {
		return 0
	}
	return len(l.ids)
}

// LoadSkills walks fsys at root, treating each immediate subdirectory as
// one skill package that must contain a SKILL.md. Any malformed frontmatter,
// missing SKILL.md, or duplicate ID fails the whole load so startup cannot
// silently proceed with a broken library. Subdirectories are walked in
// sorted order for deterministic registration.
func LoadSkills(fsys fs.FS, root string) (*SkillLibrary, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("read skills root %q: %w", root, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	lib := &SkillLibrary{byID: make(map[string]*Skill, len(names))}
	for _, name := range names {
		skillPath := path.Join(root, name, "SKILL.md")
		data, err := fs.ReadFile(fsys, skillPath)
		if err != nil {
			return nil, fmt.Errorf("skill package %q: %w", name, err)
		}
		skill, err := ParseSkillMarkdown(data)
		if err != nil {
			return nil, fmt.Errorf("skill package %q: %w", name, err)
		}
		if _, dup := lib.byID[skill.ID]; dup {
			return nil, fmt.Errorf("duplicate skill id %q (in package %q)", skill.ID, name)
		}
		lib.byID[skill.ID] = skill
		lib.ids = append(lib.ids, skill.ID)
	}
	return lib, nil
}

// ParseSkillMarkdown parses a SKILL.md source: a leading `---` line,
// one-level `key: value` lines, a closing `---`, then the markdown body.
// Required keys: id, name, description. Optional: version (int, default 1),
// tool_allowlist (comma-separated). Unknown keys are ignored so the schema
// can evolve without breaking older bundles.
func ParseSkillMarkdown(source []byte) (*Skill, error) {
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 {
		lines[0] = strings.TrimPrefix(lines[0], "\xEF\xBB\xBF")
	}
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return nil, fmt.Errorf("missing frontmatter: first line must be %q", "---")
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return nil, fmt.Errorf("unterminated frontmatter: no closing %q", "---")
	}

	skill := &Skill{Version: 1}
	seen := map[string]bool{}
	for i := 1; i < closeIdx; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return nil, fmt.Errorf("frontmatter line %d: expected %q, got %q", i+1, "key: value", line)
		}
		key := strings.TrimSpace(line[:colon])
		val := unquote(strings.TrimSpace(line[colon+1:]))
		if key == "" {
			return nil, fmt.Errorf("frontmatter line %d: empty key", i+1)
		}
		if seen[key] {
			return nil, fmt.Errorf("frontmatter duplicate key %q", key)
		}
		seen[key] = true
		switch key {
		case "id":
			skill.ID = val
		case "name":
			skill.Name = val
		case "description":
			skill.Description = val
		case "version":
			if val == "" {
				return nil, fmt.Errorf("frontmatter %q: empty value", key)
			}
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("frontmatter %q: not an integer: %q", key, val)
			}
			skill.Version = n
		case "tool_allowlist":
			skill.ToolAllowlist = parseCSVList(val)
		}
	}
	for _, req := range []struct{ key, val string }{
		{"id", skill.ID}, {"name", skill.Name}, {"description", skill.Description},
	} {
		if req.val == "" {
			return nil, fmt.Errorf("frontmatter missing required key %q", req.key)
		}
	}
	body := strings.Join(lines[closeIdx+1:], "\n")
	body = strings.TrimLeft(body, "\n")
	body = strings.TrimRight(body, " \t\n")
	skill.Body = body
	return skill, nil
}

func unquote(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func parseCSVList(v string) []string {
	if v == "" {
		return nil
	}
	parts := bytes.Split([]byte(v), []byte{','})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(string(p)); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SkillsSummary renders the "Available skills" block appended to a stable
// system prompt: one id / name / when-to-load entry per skill. Empty when
// the library is nil or empty.
func SkillsSummary(lib *SkillLibrary) string {
	if lib == nil || lib.Len() == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available skills\n\n")
	b.WriteString("You can load a skill's full instructions on demand by calling the ")
	b.WriteString("`load_skill` tool with its id. Load only when the user's request ")
	b.WriteString("matches one of these workflows — don't load preemptively.\n\n")
	for _, s := range lib.List() {
		b.WriteString("- id: `")
		b.WriteString(s.ID)
		b.WriteString("`\n  name: ")
		b.WriteString(s.Name)
		b.WriteString("\n  When to load: ")
		b.WriteString(s.Description)
		b.WriteString("\n")
	}
	return b.String()
}
