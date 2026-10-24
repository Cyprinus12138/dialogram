package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Names and limits from the Agent Skills specification
// (https://agentskills.io/specification).
const (
	skillFileName               = "SKILL.md"
	skillFileNameLower          = "skill.md" // also accepted by the reference validator
	maxSkillNameLength          = 64
	maxSkillDescriptionLength   = 1024
	maxSkillCompatibilityLength = 500
)

// Discovery bounds. A skills root rarely nests skills more than a category
// deep; the caps stop a misplaced root (a home directory, a repository
// root) from turning startup into a full tree walk.
const (
	maxSkillDepth     = 4
	maxSkillResources = 1000
)

// specSkillFields are the frontmatter fields the specification defines.
var specSkillFields = []string{"name", "description", "license", "compatibility", "metadata", "allowed-tools"}

// Skill is one Agent Skills package (https://agentskills.io/specification):
// a directory holding a SKILL.md, which is YAML frontmatter followed by
// markdown instructions, plus any bundled files (scripts/, references/,
// assets/, …).
//
// dialogram follows the specification's three tiers of progressive
// disclosure. The catalog in the stable system prompt carries Name and
// Description only. The model loads Body through the load_skill tool when a
// task matches, and reads bundled files one at a time through the same
// tool when the instructions point at them.
type Skill struct {
	// Name identifies the skill: the frontmatter `name`, or the directory
	// name when the frontmatter omits it.
	Name string
	// Description says what the skill does and when to use it. It is the
	// only text the model reads before deciding to load the skill.
	Description string
	// License, Compatibility and Metadata are the optional frontmatter
	// fields of the same names. Compatibility is shown to the model with
	// the instructions; License and Metadata are for the application.
	License       string
	Compatibility string
	Metadata      map[string]string
	// AllowedTools is the experimental `allowed-tools` field: tools the
	// skill expects to run without asking. dialogram records it and does
	// not act on it; approval policy stays with the interceptors.
	AllowedTools []string
	// Body is the markdown after the frontmatter: the instructions.
	Body string
	// Dir is the skill directory inside the file system the skill was
	// loaded from. Empty for a skill built in code.
	Dir string
	// Resources lists the bundled files the model may read, relative to
	// Dir and sorted. SKILL.md, hidden files and symlinks are excluded.
	Resources []string
	// Diagnostics lists the problems found while loading that did not stop
	// the skill from loading: specification violations, and bundled files
	// beyond the readable limit. Empty for a spec-clean skill.
	Diagnostics []string

	fsys fs.FS
}

func (s *Skill) diag(msg string) { s.Diagnostics = append(s.Diagnostics, msg) }

// SkillLibrary holds the registered skills keyed by name, in registration
// order for deterministic listing.
type SkillLibrary struct {
	byName map[string]*Skill
	names  []string
}

// NewSkillLibrary builds a library from skills built in code or loaded one
// at a time with LoadSkill. A skill without a name or a description, or two
// skills with the same name, are errors.
func NewSkillLibrary(skills ...*Skill) (*SkillLibrary, error) {
	lib := &SkillLibrary{byName: make(map[string]*Skill, len(skills))}
	for _, s := range skills {
		if s == nil {
			continue
		}
		if err := lib.add(s); err != nil {
			return nil, err
		}
	}
	return lib, nil
}

func (l *SkillLibrary) add(s *Skill) error {
	if s.Name == "" || strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("skill %q: name and description are required", s.Name)
	}
	if prev, dup := l.byName[s.Name]; dup {
		return fmt.Errorf("duplicate skill name %q (%s and %s)", s.Name, skillOrigin(prev), skillOrigin(s))
	}
	l.byName[s.Name] = s
	l.names = append(l.names, s.Name)
	return nil
}

func skillOrigin(s *Skill) string {
	if s.Dir == "" {
		return "built in code"
	}
	return "in " + strconv.Quote(s.Dir)
}

// Get returns the skill with the given name, if registered. Nil-safe.
func (l *SkillLibrary) Get(name string) (*Skill, bool) {
	if l == nil {
		return nil, false
	}
	s, ok := l.byName[name]
	return s, ok
}

// List returns the registered skills in registration order. Nil-safe.
func (l *SkillLibrary) List() []*Skill {
	if l == nil {
		return nil
	}
	out := make([]*Skill, 0, len(l.names))
	for _, n := range l.names {
		out = append(out, l.byName[n])
	}
	return out
}

// Names returns the registered skill names in registration order. Nil-safe.
func (l *SkillLibrary) Names() []string {
	if l == nil {
		return nil
	}
	return append([]string(nil), l.names...)
}

// Len reports the number of registered skills. Nil-safe.
func (l *SkillLibrary) Len() int {
	if l == nil {
		return 0
	}
	return len(l.names)
}

// LoadSkills discovers and loads every skill under root in fsys. A
// directory that contains a SKILL.md is one skill; its subdirectories are
// bundled files, not further skills. Discovery walks in lexical order,
// skips hidden directories and node_modules, follows a symlinked skill
// directory one level, and looks at most four levels below root. root may
// itself be a skill directory. Point it at a shared location such as
// .agents/skills to use the skills other clients install.
//
// Loading is lenient where the specification's client guide says it
// should be. A name that breaks the naming rules or does not match its
// directory, an over-long field or a field the specification does not
// define is recorded in Skill.Diagnostics, logged, and the skill still
// loads. Problems that leave a skill unusable fail the whole load, so a
// server cannot start with a skill silently missing: frontmatter that does
// not parse, a missing description, or two skills with the same name. To
// skip broken skills instead, call LoadSkill per directory and pass the
// ones that load to NewSkillLibrary. ValidateSkills applies the strict
// rules.
func LoadSkills(fsys fs.FS, root string) (*SkillLibrary, error) {
	dirs, err := discoverSkillDirs(fsys, root)
	if err != nil {
		return nil, err
	}
	lib := &SkillLibrary{byName: make(map[string]*Skill, len(dirs))}
	for _, dir := range dirs {
		s, err := LoadSkill(fsys, dir)
		if err != nil {
			return nil, err
		}
		if err := lib.add(s); err != nil {
			return nil, err
		}
		for _, d := range s.Diagnostics {
			slog.Warn("skill loaded despite a problem", "skill", s.Name, "dir", s.Dir, "problem", d)
		}
	}
	return lib, nil
}

// ValidateSkills applies the specification's strict rules, the checks the
// reference validator (`skills-ref validate`) performs, to every skill under
// root. It returns one joined error listing every problem, or nil when all
// skills are spec-clean. Finding no skills is an error, since it usually
// means the wrong root. Run it in a test so the skills you ship stay
// portable to other clients:
//
//	func TestSkillsAreSpecClean(t *testing.T) {
//		if err := agent.ValidateSkills(os.DirFS("."), "skills"); err != nil {
//			t.Fatal(err)
//		}
//	}
func ValidateSkills(fsys fs.FS, root string) error {
	dirs, err := discoverSkillDirs(fsys, root)
	if err != nil {
		return err
	}
	if len(dirs) == 0 {
		return fmt.Errorf("no skills found under %q", root)
	}
	var errs []error
	seen := map[string]string{}
	for _, dir := range dirs {
		s, err := LoadSkill(fsys, dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, d := range s.Diagnostics {
			errs = append(errs, fmt.Errorf("skill %q: %s", dir, d))
		}
		if prev, dup := seen[s.Name]; dup {
			errs = append(errs, fmt.Errorf("duplicate skill name %q (in %q and %q)", s.Name, prev, dir))
		}
		seen[s.Name] = dir
	}
	return errors.Join(errs...)
}

// LoadSkill loads the one skill in dir: its SKILL.md and the list of its
// bundled files. See LoadSkills for what is lenient and what is fatal.
func LoadSkill(fsys fs.FS, dir string) (*Skill, error) {
	dir = path.Clean(dir)
	file, ok := findSkillFile(fsys, dir)
	if !ok {
		return nil, fmt.Errorf("skill %q: no %s", dir, skillFileName)
	}
	src, err := fs.ReadFile(fsys, path.Join(dir, file))
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	s, err := parseSkill(src, skillDirName(dir))
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	if file != skillFileName {
		s.Diagnostics = append([]string{fmt.Sprintf("skill file is named %q; the specification names it %q", file, skillFileName)}, s.Diagnostics...)
	}
	s.Dir, s.fsys = dir, fsys
	res, truncated, err := listSkillResources(fsys, dir, file)
	if err != nil {
		return nil, fmt.Errorf("skill %q: list bundled files: %w", dir, err)
	}
	s.Resources = res
	if truncated {
		s.diag(fmt.Sprintf("more than %d bundled files; only the first %d are readable", maxSkillResources, maxSkillResources))
	}
	return s, nil
}

// ParseSkillMarkdown parses one SKILL.md source with no directory around
// it: the name cannot fall back to a directory name and is required, there
// is no directory to match, and the skill has no bundled files. The other
// rules are those of LoadSkills.
func ParseSkillMarkdown(source []byte) (*Skill, error) {
	return parseSkill(source, "")
}

func parseSkill(source []byte, dirName string) (*Skill, error) {
	front, body, err := splitFrontmatter(source)
	if err != nil {
		return nil, err
	}
	fields, recovered, err := parseFrontmatter(front)
	if err != nil {
		return nil, err
	}
	s := &Skill{Body: body}
	if recovered {
		s.diag(`frontmatter is not valid YAML: an unquoted value contains ": "; read it as a quoted string`)
	}
	if err := s.applyFields(fields); err != nil {
		return nil, err
	}
	if s.Name == "" {
		if dirName == "" {
			return nil, errors.New(`frontmatter field "name" is missing or empty`)
		}
		s.Name = dirName
		s.diag(fmt.Sprintf(`frontmatter field "name" is missing; using the directory name %q`, dirName))
	}
	if s.Description == "" {
		return nil, errors.New(`frontmatter field "description" is missing or empty`)
	}
	s.Diagnostics = append(s.Diagnostics, skillNameProblems(s.Name, dirName)...)
	if n := utf8.RuneCountInString(s.Description); n > maxSkillDescriptionLength {
		s.diag(fmt.Sprintf("description exceeds the %d-character limit (%d characters)", maxSkillDescriptionLength, n))
	}
	if n := utf8.RuneCountInString(s.Compatibility); n > maxSkillCompatibilityLength {
		s.diag(fmt.Sprintf("compatibility exceeds the %d-character limit (%d characters)", maxSkillCompatibilityLength, n))
	}
	return s, nil
}

// splitFrontmatter separates the YAML between the leading `---` line and
// the next `---` line from the markdown body after it. A UTF-8 BOM and CRLF
// line endings are tolerated.
func splitFrontmatter(source []byte) (front, body string, err error) {
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	text = strings.TrimPrefix(text, "\xEF\xBB\xBF")
	lines := strings.Split(text, "\n")
	if strings.TrimRight(lines[0], " \t") != "---" {
		return "", "", fmt.Errorf("missing frontmatter: the first line must be %q", "---")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			front = strings.Join(lines[1:i], "\n")
			body = strings.Join(lines[i+1:], "\n")
			body = strings.TrimLeft(body, "\n")
			body = strings.TrimRight(body, " \t\n")
			return front, body, nil
		}
	}
	return "", "", fmt.Errorf("unterminated frontmatter: no closing %q", "---")
}

type frontField struct {
	key string
	val *yaml.Node
}

// parseFrontmatter reads the frontmatter as a YAML mapping. Frontmatter
// written for clients with forgiving parsers often carries an unquoted
// value with a colon in it (`description: Use when: …`), which is invalid
// YAML; as the client guide suggests, such values are quoted and the parse
// retried, and recovered reports that this happened.
func parseFrontmatter(front string) (fields []frontField, recovered bool, err error) {
	m, err := yamlMapping(front)
	if err != nil {
		fixed, changed := quoteColonValues(front)
		if !changed {
			return nil, false, err
		}
		if m, err = yamlMapping(fixed); err != nil {
			return nil, false, err
		}
		recovered = true
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := resolveAlias(m.Content[i])
		if k.Kind != yaml.ScalarNode {
			return nil, false, errors.New("frontmatter keys must be strings")
		}
		if seen[k.Value] {
			return nil, false, fmt.Errorf("frontmatter field %q appears twice", k.Value)
		}
		seen[k.Value] = true
		fields = append(fields, frontField{key: k.Value, val: resolveAlias(m.Content[i+1])})
	}
	return fields, recovered, nil
}

func yamlMapping(src string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, fmt.Errorf("invalid YAML in frontmatter: %w", err)
	}
	if doc.Kind == 0 || (doc.Kind == yaml.DocumentNode && len(doc.Content) == 0) {
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	}
	if doc.Kind != yaml.DocumentNode || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("frontmatter must be a YAML mapping")
	}
	return doc.Content[0], nil
}

var plainKeyLine = regexp.MustCompile(`^([A-Za-z0-9_-]+):[ \t]+(\S.*)$`)

// quoteColonValues rewrites top-level `key: value` lines whose plain value
// contains ": " or ends in ":" as double-quoted strings.
func quoteColonValues(front string) (string, bool) {
	lines := strings.Split(front, "\n")
	changed := false
	for i, line := range lines {
		m := plainKeyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := strings.TrimRight(m[2], " \t")
		if strings.ContainsAny(val[:1], "\"'|>[{&*!%@`#") {
			continue
		}
		if !strings.Contains(val, ": ") && !strings.HasSuffix(val, ":") {
			continue
		}
		lines[i] = m[1] + ": " + strconv.Quote(val)
		changed = true
	}
	return strings.Join(lines, "\n"), changed
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		return n.Alias
	}
	return n
}

// scalarText returns a scalar's text. Every scalar counts as a string, as in
// the reference parser: `version: 1.0` reads as "1.0". YAML null reads as
// the empty string. ok is false for a mapping or a sequence.
func scalarText(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	if n.ShortTag() == "!!null" {
		return "", true
	}
	return n.Value, true
}

func (s *Skill) applyFields(fields []frontField) error {
	var unknown []string
	for _, f := range fields {
		switch f.key {
		case "name", "description":
			text, ok := scalarText(f.val)
			if !ok {
				return fmt.Errorf("frontmatter field %q must be a string", f.key)
			}
			if f.key == "name" {
				s.Name = strings.TrimSpace(text)
			} else {
				s.Description = strings.TrimSpace(text)
			}
		case "license", "compatibility":
			text, ok := scalarText(f.val)
			if !ok {
				s.diag(fmt.Sprintf("frontmatter field %q must be a string; ignored", f.key))
				continue
			}
			if f.key == "license" {
				s.License = strings.TrimSpace(text)
			} else {
				s.Compatibility = strings.TrimSpace(text)
			}
		case "metadata":
			s.Metadata = s.parseMetadata(f.val)
		case "allowed-tools":
			s.AllowedTools = s.parseAllowedTools(f.val)
		default:
			unknown = append(unknown, f.key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		s.diag(fmt.Sprintf("frontmatter fields not in the specification, ignored: %s (the specification defines %s)",
			strings.Join(unknown, ", "), strings.Join(specSkillFields, ", ")))
	}
	return nil
}

// parseMetadata reads the metadata mapping. Scalar values become strings;
// other values are dropped with a diagnostic.
func (s *Skill) parseMetadata(n *yaml.Node) map[string]string {
	if text, ok := scalarText(n); ok && text == "" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		s.diag(`frontmatter field "metadata" must be a mapping from strings to strings; ignored`)
		return nil
	}
	out := map[string]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, ok := scalarText(resolveAlias(n.Content[i]))
		if !ok {
			s.diag("a metadata key is not a string; ignored")
			continue
		}
		val, ok := scalarText(resolveAlias(n.Content[i+1]))
		if !ok {
			s.diag(fmt.Sprintf("metadata %q is not a string; ignored", key))
			continue
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseAllowedTools reads `allowed-tools`. The specification makes it a
// space-separated string; skills written for other clients also use commas
// or a YAML list, so all three are accepted. Separators inside parentheses
// are part of the tool pattern, as in `Bash(git add:*)`.
func (s *Skill) parseAllowedTools(n *yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		text, _ := scalarText(n)
		return splitToolList(text)
	case yaml.SequenceNode:
		var out []string
		for _, item := range n.Content {
			text, ok := scalarText(resolveAlias(item))
			if !ok {
				s.diag(`an "allowed-tools" entry is not a string; ignored`)
				continue
			}
			out = append(out, splitToolList(text)...)
		}
		return out
	default:
		s.diag(`frontmatter field "allowed-tools" must be a space-separated string; ignored`)
		return nil
	}
}

func splitToolList(text string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case r == '(':
			depth++
			cur.WriteRune(r)
		case r == ')':
			if depth > 0 {
				depth--
			}
			cur.WriteRune(r)
		case depth == 0 && (unicode.IsSpace(r) || r == ','):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// skillNameProblems applies the specification's naming rules, in the
// reference validator's order and with its wording. Letters and digits from
// any script are allowed, as the reference validator allows them; names are
// compared to the directory without Unicode normalisation.
func skillNameProblems(name, dirName string) []string {
	var out []string
	if n := utf8.RuneCountInString(name); n > maxSkillNameLength {
		out = append(out, fmt.Sprintf("skill name %q exceeds the %d-character limit (%d characters)", name, maxSkillNameLength, n))
	}
	if name != strings.ToLower(name) {
		out = append(out, fmt.Sprintf("skill name %q must be lowercase", name))
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		out = append(out, fmt.Sprintf("skill name %q cannot start or end with a hyphen", name))
	}
	if strings.Contains(name, "--") {
		out = append(out, fmt.Sprintf("skill name %q cannot contain consecutive hyphens", name))
	}
	for _, r := range name {
		if r != '-' && !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			out = append(out, fmt.Sprintf("skill name %q contains invalid characters; only letters, digits and hyphens are allowed", name))
			break
		}
	}
	if dirName != "" && dirName != name {
		out = append(out, fmt.Sprintf("directory name %q must match skill name %q", dirName, name))
	}
	return out
}

func skillDirName(dir string) string {
	b := path.Base(dir)
	if b == "." || b == "/" {
		return ""
	}
	return b
}

func skipWhileScanning(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__"
}

func findSkillFile(fsys fs.FS, dir string) (string, bool) {
	for _, name := range []string{skillFileName, skillFileNameLower} {
		if info, err := fs.Stat(fsys, path.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return name, true
		}
	}
	return "", false
}

func depthBelow(root, p string) int {
	if p == root {
		return 0
	}
	rel := p
	if root != "." {
		rel = strings.TrimPrefix(p, root+"/")
	}
	return strings.Count(rel, "/") + 1
}

func discoverSkillDirs(fsys fs.FS, root string) ([]string, error) {
	root = path.Clean(root)
	if _, err := fs.Stat(fsys, root); err != nil {
		return nil, fmt.Errorf("read skills root %q: %w", root, err)
	}
	var dirs []string
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("scan skills root %q: %w", root, err)
		}
		if p != root && skipWhileScanning(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			// A symlinked skill directory, which some installers create, is
			// followed one level: it is a skill when it holds a SKILL.md.
			if d.Type()&fs.ModeSymlink != 0 {
				if info, err := fs.Stat(fsys, p); err == nil && info.IsDir() {
					if _, ok := findSkillFile(fsys, p); ok {
						dirs = append(dirs, p)
					}
				}
			}
			return nil
		}
		if _, ok := findSkillFile(fsys, p); ok {
			dirs = append(dirs, p)
			return fs.SkipDir
		}
		if depthBelow(root, p) >= maxSkillDepth {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dirs, nil
}

// listSkillResources lists the regular files under dir, relative to it, for
// the model to read on demand. Hidden entries, node_modules, __pycache__,
// the skill file itself and anything that is not a regular file (symlinks
// in particular, so a link cannot reach outside the skill) are left out.
func listSkillResources(fsys fs.FS, dir, skillFile string) ([]string, bool, error) {
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	var out []string
	truncated := false
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if skipWhileScanning(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel := strings.TrimPrefix(p, prefix)
		if rel == skillFile {
			return nil
		}
		if len(out) == maxSkillResources {
			truncated = true
			return fs.SkipAll
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	sort.Strings(out)
	return out, truncated, nil
}

// xmlText escapes element text. Quotes need escaping only inside attributes,
// so apostrophes in a description reach the model as written.
var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// SkillsSummary renders the skill catalog appended to a stable system
// prompt: a short instruction and an <available_skills> block with one
// name and description per skill, the format the specification's
// reference library generates. Empty when the library is nil or empty, so
// no catalog appears when there is nothing to load.
func SkillsSummary(lib *SkillLibrary) string {
	if lib.Len() == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available skills\n\n")
	b.WriteString("The following skills provide specialized instructions for specific tasks. ")
	b.WriteString("When a task matches a skill's description, call the `load_skill` tool with the skill's name ")
	b.WriteString("to load its full instructions before you proceed. Do not load a skill the task does not need.\n\n")
	b.WriteString("<available_skills>\n")
	for _, s := range lib.List() {
		b.WriteString("  <skill>\n    <name>")
		b.WriteString(xmlText.Replace(s.Name))
		b.WriteString("</name>\n    <description>")
		b.WriteString(xmlText.Replace(s.Description))
		b.WriteString("</description>\n  </skill>\n")
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}
