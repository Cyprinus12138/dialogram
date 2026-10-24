package main

import (
	"testing"

	"github.com/Cyprinus12138/dialogram/agent"
)

// TestBundledSkillsAreSpecClean keeps the example's skills portable: the
// strict checks are the ones the Agent Skills reference validator applies.
func TestBundledSkillsAreSpecClean(t *testing.T) {
	if err := agent.ValidateSkills(skillFiles, "skills"); err != nil {
		t.Fatal(err)
	}
	lib, err := agent.LoadSkills(skillFiles, "skills")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := lib.Get("trip-planner")
	if !ok || len(s.Resources) != 1 || s.Resources[0] != "references/packing.md" {
		t.Fatalf("trip-planner and its packing reference should load: %+v", s)
	}
}
