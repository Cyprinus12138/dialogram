package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/Cyprinus12138/dialogram/tool"
)

func mf(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

const pdfSkill = `---
name: pdf-processing
description: Extract PDF text, fill forms, merge files. Use when handling PDFs.
license: Apache-2.0
compatibility: Requires poppler-utils
metadata:
  author: example-org
  version: "1.0"
allowed-tools: Bash(git:*) Bash(jq:*) Read
---

# PDF processing

See [the reference guide](references/REFERENCE.md).
`

const referenceMD = "# Reference\n\nForm fields are listed here.\n"

func pdfFS() fstest.MapFS {
	return fstest.MapFS{
		"skills/pdf-processing/SKILL.md":                                    mf(pdfSkill),
		"skills/pdf-processing/references/REFERENCE.md":                     mf(referenceMD),
		"skills/pdf-processing/scripts/extract.py":                          mf("print('extract')\n"),
		"skills/pdf-processing/assets/logo.png":                             {Data: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")},
		"skills/pdf-processing/.DS_Store":                                   mf("junk"),
		"skills/pdf-processing/scripts/__pycache__/extract.cpython-312.pyc": mf("junk"),
	}
}

func mustLoad(t *testing.T, fsys fstest.MapFS, root string) *SkillLibrary {
	t.Helper()
	lib, err := LoadSkills(fsys, root)
	if err != nil {
		t.Fatalf("LoadSkills(%q): %v", root, err)
	}
	return lib
}

func TestLoadSkill_SpecSkill(t *testing.T) {
	lib := mustLoad(t, pdfFS(), "skills")
	s, ok := lib.Get("pdf-processing")
	if !ok {
		t.Fatalf("skill not found; names = %v", lib.Names())
	}
	if s.Description != "Extract PDF text, fill forms, merge files. Use when handling PDFs." ||
		s.License != "Apache-2.0" || s.Compatibility != "Requires poppler-utils" || s.Dir != "skills/pdf-processing" {
		t.Errorf("fields off: %+v", s)
	}
	if want := map[string]string{"author": "example-org", "version": "1.0"}; !reflect.DeepEqual(s.Metadata, want) {
		t.Errorf("metadata = %v, want %v", s.Metadata, want)
	}
	if want := []string{"Bash(git:*)", "Bash(jq:*)", "Read"}; !reflect.DeepEqual(s.AllowedTools, want) {
		t.Errorf("allowed tools = %q, want %q", s.AllowedTools, want)
	}
	if !strings.HasPrefix(s.Body, "# PDF processing\n\nSee [the reference guide]") || strings.HasSuffix(s.Body, "\n") {
		t.Errorf("body = %q", s.Body)
	}
	if want := []string{"assets/logo.png", "references/REFERENCE.md", "scripts/extract.py"}; !reflect.DeepEqual(s.Resources, want) {
		t.Errorf("resources = %q, want %q (hidden files, __pycache__ and SKILL.md excluded)", s.Resources, want)
	}
	if len(s.Diagnostics) != 0 {
		t.Errorf("a spec-clean skill must load without diagnostics, got %q", s.Diagnostics)
	}
}

func TestLoadSkills_Discovery(t *testing.T) {
	skill := func(name string) *fstest.MapFile {
		return mf("---\nname: " + name + "\ndescription: Use for " + name + ".\n---\nbody\n")
	}
	fsys := fstest.MapFS{
		"skills/README.md":                     mf("not a skill"),
		"skills/notes/todo.txt":                mf("a directory without SKILL.md is not a skill"),
		"skills/alpha/SKILL.md":                skill("alpha"),
		"skills/alpha/nested/SKILL.md":         skill("nested"),
		"skills/document/pdf/SKILL.md":         skill("pdf"),
		"skills/lower/skill.md":                skill("lower"),
		"skills/a/b/c/four/SKILL.md":           skill("four"),
		"skills/a/b/c/d/five/SKILL.md":         skill("five"),
		"skills/.git/hidden/SKILL.md":          skill("hidden"),
		"skills/node_modules/pkg/SKILL.md":     skill("pkg"),
		"elsewhere/outside-the-root/SKILL.md":  skill("outside-the-root"),
		"skills/document/pdf/references/x.txt": mf("x"),
	}
	lib := mustLoad(t, fsys, "skills")
	if want := []string{"four", "alpha", "pdf", "lower"}; !reflect.DeepEqual(lib.Names(), want) {
		t.Fatalf("names = %q, want %q (lexical walk, depth <= 4, hidden and node_modules skipped)", lib.Names(), want)
	}
	alpha, _ := lib.Get("alpha")
	if want := []string{"nested/SKILL.md"}; !reflect.DeepEqual(alpha.Resources, want) {
		t.Errorf("a SKILL.md inside a skill is a bundled file, not a skill: resources = %q", alpha.Resources)
	}
	lower, _ := lib.Get("lower")
	if len(lower.Diagnostics) != 1 || !strings.Contains(lower.Diagnostics[0], `named "skill.md"`) {
		t.Errorf("lowercase skill.md loads with a diagnostic, got %q", lower.Diagnostics)
	}

	if one := mustLoad(t, fsys, "skills/alpha"); !reflect.DeepEqual(one.Names(), []string{"alpha"}) {
		t.Errorf("a root that is itself a skill directory loads that skill, got %q", one.Names())
	}
	if trailing := mustLoad(t, fsys, "skills/document/"); !reflect.DeepEqual(trailing.Names(), []string{"pdf"}) {
		t.Errorf("a trailing slash on root is cleaned, got %q", trailing.Names())
	}
	if empty := mustLoad(t, fsys, "skills/notes"); empty.Len() != 0 {
		t.Errorf("a root with no skills gives an empty library, got %q", empty.Names())
	}
	if _, err := LoadSkills(fsys, "missing"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a missing root must fail naming it, got %v", err)
	}
}

func TestLoadSkills_LenientDiagnostics(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		dir, frontmatter, wantName, wantDiag string
	}{
		{"trip", "name: trip-planner\ndescription: d", "trip-planner", `directory name "trip" must match skill name "trip-planner"`},
		{"Trip-Planner", "name: Trip-Planner\ndescription: d", "Trip-Planner", "must be lowercase"},
		{"-pdf", "name: -pdf\ndescription: d", "-pdf", "cannot start or end with a hyphen"},
		{"pdf--processing", "name: pdf--processing\ndescription: d", "pdf--processing", "cannot contain consecutive hyphens"},
		{"pdf_processing", "name: pdf_processing\ndescription: d", "pdf_processing", "contains invalid characters"},
		{long, "name: " + long + "\ndescription: d", long, "exceeds the 64-character limit (65 characters)"},
		{"from-dir", "description: d", "from-dir", `"name" is missing; using the directory name "from-dir"`},
		{"cc", "name: cc\ndescription: d\nmodel: sonnet\ndisable-model-invocation: true", "cc",
			"not in the specification, ignored: disable-model-invocation, model"},
		{"desc", "name: desc\ndescription: " + strings.Repeat("x", 1025), "desc", "description exceeds the 1024-character limit (1025 characters)"},
		{"compat", "name: compat\ndescription: d\ncompatibility: " + strings.Repeat("x", 501), "compat", "compatibility exceeds the 500-character limit"},
		{"lic", "name: lic\ndescription: d\nlicense: [MIT]", "lic", `"license" must be a string; ignored`},
		{"meta", "name: meta\ndescription: d\nmetadata: oops", "meta", `"metadata" must be a mapping`},
		{"meta-nested", "name: meta-nested\ndescription: d\nmetadata:\n  ok: yes\n  nested:\n    deep: x", "meta-nested", `metadata "nested" is not a string`},
		{"tools", "name: tools\ndescription: d\nallowed-tools:\n  read: true", "tools", `"allowed-tools" must be a space-separated string`},
		{"colon", "name: colon\ndescription: Use this skill when: the user asks about PDFs", "colon", "not valid YAML"},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			fsys := fstest.MapFS{"skills/" + c.dir + "/SKILL.md": mf("---\n" + c.frontmatter + "\n---\nbody\n")}
			s, err := LoadSkill(fsys, "skills/"+c.dir)
			if err != nil {
				t.Fatalf("lenient problems must not stop loading: %v", err)
			}
			if s.Name != c.wantName {
				t.Errorf("name = %q, want %q", s.Name, c.wantName)
			}
			if !strings.Contains(strings.Join(s.Diagnostics, "\n"), c.wantDiag) {
				t.Errorf("diagnostics %q do not mention %q", s.Diagnostics, c.wantDiag)
			}
		})
	}
}

func TestParseSkill_FieldForms(t *testing.T) {
	parse := func(t *testing.T, frontmatter string) *Skill {
		t.Helper()
		s, err := ParseSkillMarkdown([]byte("---\nname: x\ndescription: d\n" + frontmatter + "\n---\nbody"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, c := range []struct{ frontmatter, want string }{
		{"allowed-tools: Read Grep  Glob", "Read|Grep|Glob"},
		{"allowed-tools: Read, Grep, Glob", "Read|Grep|Glob"},
		{"allowed-tools:\n  - Read\n  - Bash(git:*)", "Read|Bash(git:*)"},
		{"allowed-tools: Bash(git add:*) Bash(npm run test, lint) Read", "Bash(git add:*)|Bash(npm run test, lint)|Read"},
		{"allowed-tools:", ""},
	} {
		if got := strings.Join(parse(t, c.frontmatter).AllowedTools, "|"); got != c.want {
			t.Errorf("%q: allowed tools = %q, want %q", c.frontmatter, got, c.want)
		}
	}
	s := parse(t, "metadata:\n  version: 1.0\n  beta: true\n  quoted: \"null\"\n  empty:")
	if want := map[string]string{"version": "1.0", "beta": "true", "quoted": "null", "empty": ""}; !reflect.DeepEqual(s.Metadata, want) {
		t.Errorf("scalars read as their text: metadata = %q, want %q", s.Metadata, want)
	}
	if s := parse(t, "metadata:"); s.Metadata != nil || len(s.Diagnostics) != 0 {
		t.Errorf("an empty metadata field is no metadata, got %v / %q", s.Metadata, s.Diagnostics)
	}
	folded, err := ParseSkillMarkdown([]byte("---\nname: x\ndescription: >\n  Folded across\n  two lines.\n---\n"))
	if err != nil || folded.Description != "Folded across two lines." || folded.Body != "" {
		t.Errorf("folded description: %+v, %v", folded, err)
	}
	colon, err := ParseSkillMarkdown([]byte("---\nname: x\ndescription: Use this skill when: the user asks \"why\"\n---\n"))
	if err != nil || colon.Description != `Use this skill when: the user asks "why"` {
		t.Errorf("an unquoted colon value is recovered verbatim: %+v, %v", colon, err)
	}
}

func TestParseSkillMarkdown_FrameAndBody(t *testing.T) {
	s, err := ParseSkillMarkdown([]byte("\xEF\xBB\xBF---\r\nname: body\r\ndescription: check body\r\n---\r\n\r\n    indented first line\r\n\r\nparagraph two.\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "    indented first line\n\nparagraph two."; s.Body != want {
		t.Errorf("body = %q, want %q", s.Body, want)
	}
	if s.Dir != "" || s.Resources != nil || len(s.Diagnostics) != 0 {
		t.Errorf("a parsed file has no directory, resources or diagnostics: %+v", s)
	}
	if _, err := ParseSkillMarkdown([]byte("---\ndescription: no name and no directory\n---\n")); err == nil || !strings.Contains(err.Error(), `"name" is missing`) {
		t.Errorf("without a directory the name is required, got %v", err)
	}
}

func TestLoadSkills_FatalErrors(t *testing.T) {
	cases := map[string]string{
		"invalid YAML":        "---\nname: [unclosed\ndescription: d\n---\n",
		"missing description": "---\nname: nodesc\n---\n",
		"empty description":   "---\nname: nodesc\ndescription: \"  \"\n---\n",
		"description list":    "---\nname: x\ndescription: [a, b]\n---\n",
		"name mapping":        "---\nname: {a: b}\ndescription: d\n---\n",
		"not a mapping":       "---\n- a\n- b\n---\n",
		"no frontmatter":      "# just markdown\n",
		"unterminated":        "---\nname: x\ndescription: d\n",
		"duplicate field":     "---\nname: x\nname: y\ndescription: d\n---\n",
		"empty frontmatter":   "---\n---\nbody\n",
		"comment-only":        "---\n# nothing here\n---\nbody\n",
		"non-string key":      "---\n[a]: b\nname: x\ndescription: d\n---\n",
	}
	for label, src := range cases {
		t.Run(label, func(t *testing.T) {
			fsys := fstest.MapFS{"skills/broken/SKILL.md": mf(src), "skills/fine/SKILL.md": mf("---\nname: fine\ndescription: d\n---\n")}
			if _, err := LoadSkills(fsys, "skills"); err == nil || !strings.Contains(err.Error(), `"skills/broken"`) {
				t.Errorf("must fail naming the skill directory, got %v", err)
			}
		})
	}
	dup := fstest.MapFS{
		"skills/one/SKILL.md": mf("---\nname: same\ndescription: first\n---\n"),
		"skills/two/SKILL.md": mf("---\nname: same\ndescription: second\n---\n"),
	}
	if _, err := LoadSkills(dup, "skills"); err == nil || !strings.Contains(err.Error(), `duplicate skill name "same"`) {
		t.Errorf("duplicate names must fail, got %v", err)
	}
}

func TestNewSkillLibrary(t *testing.T) {
	lib, err := NewSkillLibrary(
		&Skill{Name: "beta", Description: "second"},
		nil,
		&Skill{Name: "alpha", Description: "first"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lib.Names(), []string{"beta", "alpha"}) || lib.Len() != 2 || lib.List()[1].Name != "alpha" {
		t.Errorf("registration order lost: %q", lib.Names())
	}
	if _, err := NewSkillLibrary(&Skill{Name: "x"}); err == nil {
		t.Error("a skill without a description must fail")
	}
	if _, err := NewSkillLibrary(&Skill{Description: "d"}); err == nil {
		t.Error("a skill without a name must fail")
	}
	if _, err := NewSkillLibrary(&Skill{Name: "x", Description: "a"}, &Skill{Name: "x", Description: "b"}); err == nil ||
		!strings.Contains(err.Error(), "built in code") {
		t.Errorf("duplicate names must fail and say where each came from, got %v", err)
	}
	var none *SkillLibrary
	if _, ok := none.Get("x"); ok || none.Len() != 0 || none.List() != nil || none.Names() != nil {
		t.Error("a nil library must behave as empty")
	}
}

func TestSkillsSummary(t *testing.T) {
	if SkillsSummary(nil) != "" || SkillsSummary(&SkillLibrary{}) != "" {
		t.Fatal("nil/empty lib must render an empty summary")
	}
	lib, _ := NewSkillLibrary(
		&Skill{Name: "alpha", Description: "Use for the user's <b> & friends."},
		&Skill{Name: "beta", Description: "Use when asked about Y."},
	)
	got := SkillsSummary(lib)
	want := "## Available skills\n\n" +
		"The following skills provide specialized instructions for specific tasks. " +
		"When a task matches a skill's description, call the `load_skill` tool with the skill's name " +
		"to load its full instructions before you proceed. Do not load a skill the task does not need.\n\n" +
		"<available_skills>\n" +
		"  <skill>\n    <name>alpha</name>\n    <description>Use for the user's &lt;b&gt; &amp; friends.</description>\n  </skill>\n" +
		"  <skill>\n    <name>beta</name>\n    <description>Use when asked about Y.</description>\n  </skill>\n" +
		"</available_skills>\n"
	if got != want {
		t.Errorf("catalog drift:\n got %q\nwant %q", got, want)
	}
}

// callLoadSkill invokes load_skill and decodes its JSON result.
func callLoadSkill(t *testing.T, reg *tool.Registry, input string) map[string]any {
	t.Helper()
	out, err := reg.Call(context.Background(), LoadSkillToolName, input)
	if err != nil {
		t.Fatalf("Call(%s): %v", input, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("result is not a JSON object: %v: %s", err, out)
	}
	return parsed
}

func TestRegisterLoadSkill_Activation(t *testing.T) {
	lib := mustLoad(t, pdfFS(), "skills")
	coded, _ := NewSkillLibrary(&Skill{Name: "inline", Description: "built in code", Body: "Do the thing."})
	reg := tool.NewRegistry()
	RegisterLoadSkill(reg, lib)

	tl, ok := reg.Lookup(LoadSkillToolName)
	if !ok {
		t.Fatal("load_skill not registered")
	}
	props := tl.Schema["properties"].(map[string]any)
	if enum := props["name"].(map[string]any)["enum"]; !reflect.DeepEqual(enum, []any{"pdf-processing"}) {
		t.Errorf("name must be an enum of the library's names, got %v", enum)
	}
	if typ := props["offset"].(map[string]any)["type"]; typ != "integer" {
		t.Errorf("offset is an optional integer, got %v", typ)
	}
	if req := tl.Schema["required"]; !reflect.DeepEqual(req, []string{"name"}) {
		t.Errorf("only name is required, got %v", req)
	}

	got := callLoadSkill(t, reg, `{"name":"pdf-processing"}`)
	if got["name"] != "pdf-processing" || got["compatibility"] != "Requires poppler-utils" ||
		!strings.HasPrefix(got["instructions"].(string), "# PDF processing") || got["note"] != resourcesNote {
		t.Errorf("activation payload: %v", got)
	}
	if res := got["resources"]; !reflect.DeepEqual(res, []any{"assets/logo.png", "references/REFERENCE.md", "scripts/extract.py"}) {
		t.Errorf("resources are listed, not read: %v", res)
	}

	codedReg := tool.NewRegistry()
	RegisterLoadSkill(codedReg, coded)
	got = callLoadSkill(t, codedReg, `{"name":"inline"}`)
	if got["instructions"] != "Do the thing." || got["resources"] != nil || got["note"] != nil || got["compatibility"] != nil {
		t.Errorf("a skill built in code activates with its body only: %v", got)
	}
	if got = callLoadSkill(t, codedReg, `{"name":"inline","path":"references/x.md"}`); got["error_code"] != errCodeUnknownResource {
		t.Errorf("a skill built in code has no bundled files: %v", got)
	}

	for input, code := range map[string]string{
		`{"name":"bogus"}`: errCodeUnknownSkill,
		`{"name":""}`:      tool.ErrorCodeInvalidInput,
		`{"id":"pdf"}`:     tool.ErrorCodeInvalidInput,
		`not json`:         tool.ErrorCodeInvalidInput,
	} {
		if got := callLoadSkill(t, reg, input); got["error_code"] != code || got["retryable"] != false {
			t.Errorf("%s: want soft failure %s, got %v", input, code, got)
		}
	}

	empty := tool.NewRegistry()
	RegisterLoadSkill(empty, nil)
	RegisterLoadSkill(empty, &SkillLibrary{})
	RegisterLoadSkill(nil, lib)
	if empty.Len() != 0 {
		t.Error("nil/empty lib must not register a tool")
	}
}

func TestRegisterLoadSkill_Resources(t *testing.T) {
	lib := mustLoad(t, pdfFS(), "skills")
	reg := tool.NewRegistry()
	RegisterLoadSkill(reg, lib)

	for _, p := range []string{
		"references/REFERENCE.md",
		"./references/REFERENCE.md",
		" references/REFERENCE.md ",
		"pdf-processing/references/REFERENCE.md",
		"skills/pdf-processing/references/REFERENCE.md",
		"references//REFERENCE.md",
	} {
		got := callLoadSkill(t, reg, fmt.Sprintf(`{"name":"pdf-processing","path":%q}`, p))
		if got["content"] != referenceMD || got["path"] != "references/REFERENCE.md" ||
			got["bytes"] != float64(len(referenceMD)) || got["next_offset"] != nil || got["note"] != nil {
			t.Errorf("path %q: %v", p, got)
		}
	}
	if got := callLoadSkill(t, reg, `{"name":"pdf-processing","path":"scripts/extract.py"}`); got["content"] != "print('extract')\n" {
		t.Errorf("scripts are returned as text: %v", got)
	}

	for p, code := range map[string]string{
		"../other/SKILL.md":       errCodeUnknownResource,
		"/etc/passwd":             errCodeUnknownResource,
		"SKILL.md":                errCodeUnknownResource,
		".DS_Store":               errCodeUnknownResource,
		"references":              errCodeUnknownResource,
		"assets/logo.png":         errCodeBinaryResource,
		"references/NOT-THERE.md": errCodeUnknownResource,
	} {
		got := callLoadSkill(t, reg, fmt.Sprintf(`{"name":"pdf-processing","path":%q}`, p))
		if got["error_code"] != code {
			t.Errorf("path %q: want %s, got %v", p, code, got)
		}
	}
	miss := callLoadSkill(t, reg, `{"name":"pdf-processing","path":"references/NOT-THERE.md"}`)
	if msg, _ := miss["error_message"].(string); !strings.Contains(msg, "references/REFERENCE.md") {
		t.Errorf("a miss lists the bundled files: %v", miss)
	}

}

// readAllPages follows next_offset until the end and returns the pages.
func readAllPages(t *testing.T, reg *tool.Registry, args map[string]any, field string) []string {
	t.Helper()
	var pages []string
	for i := 0; i < 20; i++ {
		in, _ := json.Marshal(args)
		got := callLoadSkill(t, reg, string(in))
		text, ok := got[field].(string)
		if !ok {
			t.Fatalf("page %d has no %s: %v", i, field, got)
		}
		if len(text) > skillPageBytes || !utf8.ValidString(text) {
			t.Fatalf("page %d: %d bytes, valid UTF-8 %v", i, len(text), utf8.ValidString(text))
		}
		pages = append(pages, text)
		next, more := got["next_offset"].(float64)
		if !more {
			if got["note"] != nil && strings.Contains(got["note"].(string), "offset") {
				t.Errorf("the last page must not point further: %v", got["note"])
			}
			return pages
		}
		if note, _ := got["note"].(string); !strings.Contains(note, fmt.Sprintf("offset %d", int(next))) {
			t.Errorf("page %d: the note must name the next offset %d: %q", i, int(next), note)
		}
		args["offset"] = int(next)
	}
	t.Fatal("paging did not terminate")
	return nil
}

func TestRegisterLoadSkill_Paging(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Long skill\n\n")
	for i := 0; body.Len() < 3*skillPageBytes; i++ {
		fmt.Fprintf(&body, "%04d. Step with an accent: é, and a quote: \"q\".\n", i)
	}
	longBody := strings.TrimRight(body.String(), "\n")
	bigFile := strings.Repeat("ü", skillPageBytes) // two bytes per rune, no line breaks
	fsys := fstest.MapFS{
		"skills/long/SKILL.md":           mf("---\nname: long\ndescription: d\ncompatibility: c\n---\n" + longBody + "\n"),
		"skills/long/references/big.txt": mf(bigFile),
	}
	reg := tool.NewRegistry()
	RegisterLoadSkill(reg, mustLoad(t, fsys, "skills"))

	first := callLoadSkill(t, reg, `{"name":"long"}`)
	if first["resources"] == nil || first["compatibility"] != "c" || !strings.Contains(first["note"].(string), resourcesNote) {
		t.Errorf("the first page carries the resources, compatibility and resources note: %v", first)
	}
	if text := first["instructions"].(string); !strings.HasSuffix(text, "\n") {
		t.Errorf("a page ends at a line break when one is near: ...%q", text[len(text)-20:])
	}
	pages := readAllPages(t, reg, map[string]any{"name": "long"}, "instructions")
	if len(pages) < 3 || strings.Join(pages, "") != longBody {
		t.Fatalf("%d pages do not reassemble the instructions", len(pages))
	}
	second := callLoadSkill(t, reg, fmt.Sprintf(`{"name":"long","offset":%d}`, len(pages[0])))
	if second["resources"] != nil || second["compatibility"] != nil || second["offset"] != float64(len(pages[0])) {
		t.Errorf("later pages carry only the text and their offset: %v", second)
	}

	filePages := readAllPages(t, reg, map[string]any{"name": "long", "path": "references/big.txt"}, "content")
	if len(filePages) != 2 || strings.Join(filePages, "") != bigFile {
		t.Errorf("a file without line breaks pages at character boundaries: %d pages", len(filePages))
	}
	mid := callLoadSkill(t, reg, `{"name":"long","path":"references/big.txt","offset":1}`)
	if c, _ := mid["content"].(string); !strings.HasPrefix(c, "ü") || !utf8.ValidString(c) || mid["offset"] != float64(2) {
		t.Errorf("an offset inside a character moves to the next one and reports where it started: offset=%v %q", mid["offset"], c[:4])
	}

	for _, input := range []string{
		`{"name":"long","offset":-1}`,
		fmt.Sprintf(`{"name":"long","offset":%d}`, len(longBody)),
		fmt.Sprintf(`{"name":"long","path":"references/big.txt","offset":%d}`, len(bigFile)+1),
	} {
		if got := callLoadSkill(t, reg, input); got["error_code"] != tool.ErrorCodeInvalidInput {
			t.Errorf("%s: want INVALID_INPUT, got %v", input, got)
		}
	}
}

func TestRegisterLoadSkill_ListCap(t *testing.T) {
	fsys := fstest.MapFS{"skills/many/SKILL.md": mf("---\nname: many\ndescription: d\n---\nbody")}
	for i := 0; i < maxListedSkillResources+5; i++ {
		fsys[fmt.Sprintf("skills/many/references/r%03d.md", i)] = mf("x")
	}
	reg := tool.NewRegistry()
	RegisterLoadSkill(reg, mustLoad(t, fsys, "skills"))
	got := callLoadSkill(t, reg, `{"name":"many"}`)
	if res, _ := got["resources"].([]any); len(res) != maxListedSkillResources || got["resources_omitted"] != float64(5) {
		t.Errorf("listing capped at %d with the remainder counted: len=%d omitted=%v", maxListedSkillResources, len(res), got["resources_omitted"])
	}
	if got := callLoadSkill(t, reg, `{"name":"many","path":"references/r104.md"}`); got["content"] != "x" {
		t.Errorf("files past the listing cap stay readable: %v", got)
	}
}

func TestLoadSkill_ResourceLimit(t *testing.T) {
	fsys := fstest.MapFS{"skills/huge/SKILL.md": mf("---\nname: huge\ndescription: d\n---\nbody")}
	for i := 0; i < maxSkillResources+1; i++ {
		fsys[fmt.Sprintf("skills/huge/f%04d.txt", i)] = mf("x")
	}
	s, err := LoadSkill(fsys, "skills/huge")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Resources) != maxSkillResources || len(s.Diagnostics) != 1 || !strings.Contains(s.Diagnostics[0], "more than 1000 bundled files") {
		t.Errorf("resources = %d, diagnostics = %q", len(s.Resources), s.Diagnostics)
	}
}

func TestValidateSkills(t *testing.T) {
	if err := ValidateSkills(pdfFS(), "skills"); err != nil {
		t.Errorf("a spec-clean tree validates: %v", err)
	}
	fsys := fstest.MapFS{
		"skills/clean/SKILL.md":  mf("---\nname: clean\ndescription: d\n---\n"),
		"skills/trip/SKILL.md":   mf("---\nname: trip-planner\ndescription: d\nmodel: x\n---\n"),
		"skills/broken/SKILL.md": mf("---\nname: broken\n---\n"),
		"skills/again/SKILL.md":  mf("---\nname: clean\ndescription: d\n---\n"),
	}
	err := ValidateSkills(fsys, "skills")
	if err == nil {
		t.Fatal("problems must fail validation")
	}
	for _, want := range []string{
		`skill "skills/broken": frontmatter field "description" is missing or empty`,
		`skill "skills/trip": directory name "trip" must match skill name "trip-planner"`,
		`skill "skills/trip": frontmatter fields not in the specification, ignored: model`,
		`duplicate skill name "clean"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error does not report %q:\n%v", want, err)
		}
	}
	if err := ValidateSkills(fstest.MapFS{"skills/README.md": mf("x")}, "skills"); err == nil || !strings.Contains(err.Error(), "no skills found") {
		t.Errorf("finding no skills is an error, got %v", err)
	}
}

func TestLoadSkills_Symlinks(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, data string) {
		t.Helper()
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("store/linked/SKILL.md", "---\nname: linked\ndescription: installed by symlink\n---\nbody")
	write("store/linked/references/r.md", "ref")
	write("skills/plain/SKILL.md", "---\nname: plain\ndescription: d\n---\nbody")
	write("outside.txt", "secret")
	if err := os.Symlink(filepath.Join(tmp, "store", "linked"), filepath.Join(tmp, "skills", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(tmp, "outside.txt"), filepath.Join(tmp, "skills", "plain", "leak.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	lib, err := LoadSkills(os.DirFS(tmp), "skills")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lib.Names(), []string{"linked", "plain"}) {
		t.Fatalf("a symlinked skill directory is discovered: %q", lib.Names())
	}
	linked, _ := lib.Get("linked")
	if !reflect.DeepEqual(linked.Resources, []string{"references/r.md"}) {
		t.Errorf("files inside a symlinked skill are listed: %q", linked.Resources)
	}
	plain, _ := lib.Get("plain")
	if len(plain.Resources) != 0 {
		t.Errorf("a symlinked file is never served: %q", plain.Resources)
	}
}
