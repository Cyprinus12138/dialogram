package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Cyprinus12138/dialogram/tool"
)

// LoadSkillToolName is the reserved tool through which the model loads a
// skill's instructions and reads the files bundled with it.
const LoadSkillToolName = "load_skill"

const (
	// skillPageBytes is how much text one load_skill call returns, for the
	// instructions and for a bundled file alike. Longer text is paged: the
	// result carries next_offset and the model calls again. The page sits
	// well below the executor's default observation cap, so a long skill
	// arrives in whole pages rather than as one opaque truncated preview.
	skillPageBytes = 32 << 10
	// maxListedSkillResources caps the bundled-file list in an activation
	// result. Files past the cap stay readable by path.
	maxListedSkillResources = 100

	errCodeUnknownSkill    = "UNKNOWN_SKILL"
	errCodeUnknownResource = "UNKNOWN_RESOURCE"
	errCodeBinaryResource  = "BINARY_RESOURCE"
)

// resourcesNote tells the model how to follow a relative path in a
// skill's instructions, since it has no file system of its own.
const resourcesNote = "Paths in these instructions are relative to the skill directory. " +
	"To read one of the listed files, call load_skill again with this skill's name and the file's path."

// pageNote tells the model how to read past the end of a page.
const pageNote = "This is one part of the text. Call load_skill again with the same arguments and offset %d to read on."

// skillActivation is load_skill's result for a name alone: tier 2 of the
// specification's progressive disclosure, with the bundled files listed
// but not read. The list and the compatibility note come with the first
// page only.
type skillActivation struct {
	Name             string   `json:"name"`
	Compatibility    string   `json:"compatibility,omitempty"`
	Instructions     string   `json:"instructions"`
	Offset           int      `json:"offset,omitempty"`
	NextOffset       int      `json:"next_offset,omitempty"`
	Resources        []string `json:"resources,omitempty"`
	ResourcesOmitted int      `json:"resources_omitted,omitempty"`
	Note             string   `json:"note,omitempty"`
}

// skillResource is load_skill's result for a name and a path: tier 3, one
// bundled file as text.
type skillResource struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	Bytes      int    `json:"bytes"`
	Offset     int    `json:"offset,omitempty"`
	NextOffset int    `json:"next_offset,omitempty"`
	Note       string `json:"note,omitempty"`
}

// RegisterLoadSkill wires the load_skill tool into reg, backed by lib. It is
// a no-op when lib is nil or empty, so no load_skill appears on the wire
// when there is nothing to load. The name parameter is an enum of the
// library's skill names, so the model cannot ask for a skill that does not
// exist.
//
// Tool contract:
//
//	input:  {"name": "<skill>", "offset"?: n}                    → instructions
//	        {"name": "<skill>", "path": "<file>", "offset"?: n}  → one bundled file
//	output: {"name", "compatibility"?, "instructions", "offset"?, "next_offset"?, "resources"?, "resources_omitted"?, "note"?}
//	        {"name", "path", "content", "bytes", "offset"?, "next_offset"?, "note"?}
//	misses: soft failures UNKNOWN_SKILL, UNKNOWN_RESOURCE, BINARY_RESOURCE,
//	        INVALID_INPUT, NOT_FOUND
//
// Text longer than one page (32 KiB) is returned a page at a time, cut at a
// line break where one is near: next_offset is where the following page
// starts, and the note tells the model to call again with it.
//
// Bundled files are served from the skill's own listing only, so a path
// cannot leave the skill directory. Scripts are returned as text and never
// run: executing them needs a tool of the application's own, with its own
// isolation.
func RegisterLoadSkill(reg *tool.Registry, lib *SkillLibrary) {
	if reg == nil || lib.Len() == 0 {
		return
	}
	reg.Register(&tool.Tool{
		Name:  LoadSkillToolName,
		Label: "Load a skill",
		Description: "Load a skill listed under <available_skills> in the system prompt. " +
			"Pass only the name to get the skill's full instructions and the list of files bundled with it. " +
			"Pass the name and a path from that list to read one bundled file, such as references/REFERENCE.md. " +
			"Long text comes in parts: when a result has next_offset, call again with that offset. " +
			"Load a skill only when the task matches its description.",
		Schema: tool.Obj(map[string]any{
			"name":   tool.Enum("The skill's name, as listed under <available_skills>.", lib.Names()...),
			"path":   tool.Str("Optional. A bundled file to read, relative to the skill directory, as listed in the skill's resources."),
			"offset": tool.Int("Optional. Where to continue reading, from a previous result's next_offset."),
		}, "name"),
		Fn: func(_ context.Context, input json.RawMessage) (any, error) {
			var args struct {
				Name   string `json:"name"`
				Path   string `json:"path"`
				Offset int    `json:"offset"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.Soft(tool.ErrorCodeInvalidInput, fmt.Sprintf("decode input: %v", err)), nil
			}
			if args.Name == "" {
				return tool.Soft(tool.ErrorCodeInvalidInput, "name is required"), nil
			}
			if args.Offset < 0 {
				return tool.Soft(tool.ErrorCodeInvalidInput, "offset cannot be negative"), nil
			}
			s, ok := lib.Get(args.Name)
			if !ok {
				return tool.Soft(errCodeUnknownSkill, fmt.Sprintf("no skill named %q; available skills: %s",
					args.Name, strings.Join(lib.Names(), ", "))), nil
			}
			if strings.TrimSpace(args.Path) == "" {
				return activateSkill(s, args.Offset), nil
			}
			return readSkillResource(s, args.Path, args.Offset), nil
		},
		Annotations: tool.Annotations{ReadOnly: true, Idempotent: true},
	})
}

func activateSkill(s *Skill, offset int) any {
	page, start, next, ok := textPage(s.Body, offset)
	if !ok {
		return tool.Soft(tool.ErrorCodeInvalidInput, fmt.Sprintf("offset %d is past the end of the instructions (%d bytes)", offset, len(s.Body)))
	}
	out := skillActivation{Name: s.Name, Instructions: page, Offset: start, NextOffset: next}
	var notes []string
	if offset == 0 {
		out.Compatibility = s.Compatibility
		if len(s.Resources) > 0 {
			out.Resources = s.Resources
			if len(out.Resources) > maxListedSkillResources {
				out.Resources = out.Resources[:maxListedSkillResources]
				out.ResourcesOmitted = len(s.Resources) - maxListedSkillResources
			}
			notes = append(notes, resourcesNote)
		}
	}
	if next > 0 {
		notes = append(notes, fmt.Sprintf(pageNote, next))
	}
	out.Note = strings.Join(notes, " ")
	return out
}

// textPage returns the page of text starting at offset, where it actually
// starts, and the offset of the next page, 0 when this page reaches the end.
// A page ends at the last line break in its final 2 KiB when there is one,
// else at a character boundary. An offset inside a multi-byte character
// moves forward to the next one. ok is false for an offset past the end.
func textPage(text string, offset int) (page string, start, next int, ok bool) {
	if offset > len(text) || (offset == len(text) && offset > 0) {
		return "", 0, 0, false
	}
	for offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset++
	}
	end := offset + skillPageBytes
	if end >= len(text) {
		return text[offset:], offset, 0, true
	}
	for end > offset && !utf8.RuneStart(text[end]) {
		end--
	}
	if nl := strings.LastIndexByte(text[offset:end], '\n'); nl >= 0 && offset+nl+1 >= end-2048 {
		end = offset + nl + 1
	}
	return text[offset:end], offset, end, true
}

func readSkillResource(s *Skill, p string, offset int) any {
	rel, ok := s.lookupResource(p)
	if !ok {
		if len(s.Resources) == 0 {
			return tool.Soft(errCodeUnknownResource, fmt.Sprintf("skill %q has no bundled files", s.Name))
		}
		listed := s.Resources
		more := ""
		if len(listed) > 20 {
			listed, more = listed[:20], ", …"
		}
		return tool.Soft(errCodeUnknownResource, fmt.Sprintf("skill %q has no bundled file %q; bundled files: %s%s",
			s.Name, p, strings.Join(listed, ", "), more))
	}
	data, err := fs.ReadFile(s.fsys, path.Join(s.Dir, rel))
	if err != nil {
		return tool.Soft(tool.ErrorCodeNotFound, fmt.Sprintf("read %q: %v", rel, err))
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return tool.Soft(errCodeBinaryResource, fmt.Sprintf("%q is a binary file (%d bytes) and cannot be returned as text", rel, len(data)))
	}
	page, start, next, ok := textPage(string(data), offset)
	if !ok {
		return tool.Soft(tool.ErrorCodeInvalidInput, fmt.Sprintf("offset %d is past the end of %q (%d bytes)", offset, rel, len(data)))
	}
	out := skillResource{Name: s.Name, Path: rel, Content: page, Bytes: len(data), Offset: start, NextOffset: next}
	if next > 0 {
		out.Note = fmt.Sprintf(pageNote, next)
	}
	return out
}

// lookupResource maps a path the model passed to an entry of s.Resources.
// It tolerates a "./" prefix and a leading skill name or skill directory,
// which models add when they echo a path from the instructions.
func (s *Skill) lookupResource(p string) (string, bool) {
	if s.fsys == nil || len(s.Resources) == 0 {
		return "", false
	}
	clean := path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "./"))
	candidates := []string{clean}
	for _, prefix := range []string{s.Name + "/", s.Dir + "/"} {
		if prefix != "/" && strings.HasPrefix(clean, prefix) {
			candidates = append(candidates, strings.TrimPrefix(clean, prefix))
		}
	}
	for _, c := range candidates {
		if i := sort.SearchStrings(s.Resources, c); i < len(s.Resources) && s.Resources[i] == c {
			return c, true
		}
	}
	return "", false
}
