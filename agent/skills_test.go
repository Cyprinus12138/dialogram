package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Cyprinus12138/dialogram/tool"
)

func TestLoadSkills_TwoValidSkills(t *testing.T) {
	fsys := fstest.MapFS{
		"skills/alpha/SKILL.md": &fstest.MapFile{Data: []byte("---\nid: alpha\nname: Alpha\ndescription: First skill\n---\nAlpha body.\n")},
		"skills/beta/SKILL.md":  &fstest.MapFile{Data: []byte("---\nid: beta\nname: Beta\ndescription: Second skill\nversion: 2\n---\nBeta body.\n")},
	}
	lib, err := LoadSkills(fsys, "skills")
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if lib.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", lib.Len())
	}
	a, ok := lib.Get("alpha")
	if !ok || a.Name != "Alpha" || a.Description != "First skill" || a.Version != 1 || a.Body != "Alpha body." {
		t.Errorf("alpha skill fields off: %+v", a)
	}
	if b, _ := lib.Get("beta"); b.Version != 2 {
		t.Errorf("beta version = %d, want 2", b.Version)
	}
	if list := lib.List(); len(list) != 2 || list[0].ID != "alpha" || list[1].ID != "beta" {
		t.Errorf("List() order wrong")
	}
}

func TestLoadSkills_Errors(t *testing.T) {
	_, err := LoadSkills(fstest.MapFS{
		"skills/valid/SKILL.md":   &fstest.MapFile{Data: []byte("---\nid: valid\nname: Valid\ndescription: ok\n---\nbody\n")},
		"skills/broken/README.md": &fstest.MapFile{Data: []byte("no SKILL.md here")},
	}, "skills")
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("missing SKILL.md must fail naming the subdir, got %v", err)
	}
	_, err = LoadSkills(fstest.MapFS{
		"skills/one/SKILL.md": &fstest.MapFile{Data: []byte("---\nid: same\nname: One\ndescription: first\n---\nbody one\n")},
		"skills/two/SKILL.md": &fstest.MapFile{Data: []byte("---\nid: same\nname: Two\ndescription: second\n---\nbody two\n")},
	}, "skills")
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate ids must fail, got %v", err)
	}
	if _, err := ParseSkillMarkdown([]byte("---\nname: No ID\ndescription: missing id\n---\nbody\n")); err == nil || !strings.Contains(err.Error(), "id") {
		t.Errorf("missing id must fail, got %v", err)
	}
	if _, err := ParseSkillMarkdown([]byte("---\nid: x\nid: y\nname: n\ndescription: d\n---\n")); err == nil {
		t.Error("duplicate key must fail")
	}
	if _, err := ParseSkillMarkdown([]byte("---\nid: x\nname: n\ndescription: d\n")); err == nil {
		t.Error("unterminated frontmatter must fail")
	}
}

func TestParseSkill_BodyAllowlistUnknownKeysBOM(t *testing.T) {
	s, err := ParseSkillMarkdown([]byte("\xEF\xBB\xBF---\nid: b\nname: Body\ndescription: check body\ntool_allowlist: foo, bar,baz\nfuture_field: whatever\n---\n\n# Heading\n\nparagraph one.\n\nparagraph two.\n"))
	if err != nil {
		t.Fatalf("ParseSkillMarkdown: %v", err)
	}
	if want := "# Heading\n\nparagraph one.\n\nparagraph two."; s.Body != want {
		t.Errorf("body = %q, want %q", s.Body, want)
	}
	if len(s.ToolAllowlist) != 3 || s.ToolAllowlist[2] != "baz" {
		t.Errorf("tool_allowlist = %v", s.ToolAllowlist)
	}
}

func twoSkillLib(t *testing.T) *SkillLibrary {
	t.Helper()
	lib, err := NewSkillLibrary(
		&Skill{ID: "alpha", Name: "Alpha", Description: "use when asked about X", Body: "# Alpha\n\nfull body."},
		&Skill{ID: "beta", Name: "Beta", Description: "use when asked about Y", Body: "# Beta\n\nanother body."},
	)
	if err != nil {
		t.Fatal(err)
	}
	return lib
}

func TestRegisterLoadSkill(t *testing.T) {
	reg := tool.NewRegistry()
	RegisterLoadSkill(reg, twoSkillLib(t))
	out, err := reg.Call(context.Background(), LoadSkillToolName, `{"id":"alpha"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(out), &parsed)
	if parsed["id"] != "alpha" || parsed["name"] != "Alpha" || parsed["body"] != "# Alpha\n\nfull body." {
		t.Errorf("unexpected hit payload: %v", parsed)
	}
	out, _ = reg.Call(context.Background(), LoadSkillToolName, `{"id":"bogus"}`)
	_ = json.Unmarshal([]byte(out), &parsed)
	if parsed["error_code"] != "UNKNOWN_SKILL" || parsed["retryable"] != false {
		t.Errorf("miss payload wrong: %v", parsed)
	}
	out, _ = reg.Call(context.Background(), LoadSkillToolName, `{"id":""}`)
	_ = json.Unmarshal([]byte(out), &parsed)
	if parsed["error_code"] != tool.ErrorCodeInvalidInput {
		t.Errorf("empty id payload wrong: %v", parsed)
	}

	empty := tool.NewRegistry()
	RegisterLoadSkill(empty, nil)
	RegisterLoadSkill(empty, &SkillLibrary{})
	if empty.Len() != 0 {
		t.Error("nil/empty lib must not register a tool")
	}
}

func TestSkillsSummary(t *testing.T) {
	if SkillsSummary(nil) != "" || SkillsSummary(&SkillLibrary{}) != "" {
		t.Fatal("nil/empty lib must render an empty summary")
	}
	got := SkillsSummary(twoSkillLib(t))
	for _, want := range []string{"## Available skills", "alpha", "use when asked about X", "beta", "load_skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q", want)
		}
	}
	if strings.Index(got, "alpha") > strings.Index(got, "beta") {
		t.Error("summaries not in registration order")
	}
	if _, err := NewSkillLibrary(&Skill{ID: "x"}); err == nil {
		t.Error("missing name/description must fail")
	}
}
