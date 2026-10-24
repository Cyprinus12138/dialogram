# Skills

dialogram reads [Agent Skills](https://agentskills.io/specification). A skill
is a directory holding a `SKILL.md`, which is YAML frontmatter followed by
markdown instructions, plus any files the instructions refer to. The same
directory works in the other clients that read the format.

```
trip-planner/
├── SKILL.md          # required: frontmatter and instructions
├── references/       # optional: documents the model reads on demand
│   └── packing.md
├── scripts/          # optional: listed and readable as text, never run
└── assets/           # optional: templates, data, images
```

## SKILL.md

```markdown
---
name: trip-planner
description: Plan a short trip that combines the forecast with a flight. Use when the user asks for a trip plan or which day to travel.
license: Apache-2.0
compatibility: Needs the get_weather and search_flights tools
metadata:
  author: example-org
  version: "1.0"
allowed-tools: get_weather search_flights
---

# Trip planner

1. Confirm the dates and the cities with request_user_form.
2. Check the forecast with get_weather. For what to pack, follow
   references/packing.md.
```

| Field | Specification | What dialogram does |
|---|---|---|
| `name` | Required. 1 to 64 characters: lowercase letters, digits and hyphens, with no leading, trailing or doubled hyphen. Matches the directory name. | Identifies the skill in the catalog and in `load_skill`. Falls back to the directory name when missing. |
| `description` | Required. 1 to 1024 characters saying what the skill does and when to use it. | The only text the model reads before it decides to load the skill. |
| `license` | Optional. | Kept in `Skill.License`. |
| `compatibility` | Optional. Up to 500 characters of environment requirements. | Kept in `Skill.Compatibility` and returned to the model with the instructions. |
| `metadata` | Optional. A map from strings to strings. | Kept in `Skill.Metadata`. Scalars are read as their text, so `version: 1.0` is `"1.0"`. |
| `allowed-tools` | Optional and experimental. Space-separated pre-approved tools. | Kept in `Skill.AllowedTools` and not enforced (see below). Commas and YAML lists are accepted as well. |

## Progressive disclosure

The specification loads a skill in three tiers. In dialogram they are:

1. **Catalog.** `loop.ComposeStableSystem` appends every skill's name and
   description to the profile's system prompt, inside the cached stable
   prefix, in the `<available_skills>` format the specification's reference
   library generates:

   ```
   ## Available skills

   The following skills provide specialized instructions for specific tasks. When a task matches a skill's description, call the `load_skill` tool with the skill's name to load its full instructions before you proceed. Do not load a skill the task does not need.

   <available_skills>
     <skill>
       <name>trip-planner</name>
       <description>Plan a short trip that combines the forecast with a flight. …</description>
     </skill>
   </available_skills>
   ```

2. **Instructions.** `load_skill {"name": "trip-planner"}` returns the body,
   the compatibility note when there is one, and the list of bundled files.
   The files are listed, not read.
3. **Resources.** `load_skill {"name": "trip-planner", "path": "references/packing.md"}`
   returns one bundled file as text.

Text longer than 32 KiB, instructions or a file, comes a page at a time. A
page ends at a line break near its end, or at a character boundary when there
is none, and carries `next_offset`; the model calls again with `offset` set to
it. A page sits below the executor's default 64 KiB observation cap, so a long
skill is never cut down to an opaque preview. A 100 KB `SKILL.md` arrives in
four pages.

The model has no file system of its own, so `load_skill` is the only way in.
Its `name` parameter is an enum of the loaded skills. A path is served only
when it is in the skill's own file list, so it cannot leave the skill
directory. Hidden files, `node_modules`, `__pycache__` and symlinks are never
listed, and a binary file comes back as a `BINARY_RESOURCE` soft failure
instead of bytes. Up to 100 files are listed in the activation result; files
past that stay readable by path.

## Loading

```go
lib, err := agent.LoadSkills(os.DirFS("."), "skills") // or an embed.FS
agent.RegisterLoadSkill(reg, lib)                       // no-op when lib is empty
deps.Skills = lib                                       // puts the catalog in the stable prompt
```

Every directory under the root that holds a `SKILL.md` is one skill, and its
subdirectories are its files rather than further skills. The walk is in
lexical order, skips hidden directories and `node_modules`, follows a
symlinked skill directory one level, and stops four levels below the root.
The root may itself be a skill directory. To share skills with other
clients, point the root at `.agents/skills`.

Loading is lenient where the specification's
[client guide](https://agentskills.io/client-implementation/adding-skills-support)
recommends it, and strict where a skill would otherwise go missing unnoticed:

| Problem | `LoadSkills` |
|---|---|
| The name breaks the naming rules or does not match the directory. | Loads and records a diagnostic. |
| The name is missing. | Loads under the directory name and records a diagnostic. |
| The description or compatibility is over its limit. | Loads and records a diagnostic. |
| A frontmatter field the specification does not define, such as Claude Code's `model`. | Ignores the field and records a diagnostic. |
| An unquoted value containing `: `, invalid YAML that other clients accept. | Quotes the value, parses again and records a diagnostic. |
| The file is `skill.md` rather than `SKILL.md`. | Loads and records a diagnostic. |
| The frontmatter does not parse or is not a mapping. | Fails the load. |
| The description is missing or empty. | Fails the load. |
| Two skills share a name. | Fails the load. |

Diagnostics are logged at WARN when the skill loads and kept in
`Skill.Diagnostics`. The client guide suggests skipping a skill that cannot
load. dialogram fails instead, so that a server does not start with a skill
silently missing. To skip broken skills, call `agent.LoadSkill` for each
directory and pass the ones that load to `agent.NewSkillLibrary`.

`agent.ValidateSkills` applies the strict rules, the ones the reference
validator `skills-ref validate` checks, and fails on any diagnostic. Run it in
a test to keep the skills you ship portable:

```go
func TestSkillsAreSpecClean(t *testing.T) {
	if err := agent.ValidateSkills(os.DirFS("."), "skills"); err != nil {
		t.Fatal(err)
	}
}
```

`examples/chatserver` ships a `trip-planner` skill and runs this test.

## What dialogram does not do

- **Run scripts.** Files under `scripts/` are listed and readable as text.
  dialogram has no code-execution tool and does not add one. A deployment
  that wants scripts to run registers its own tool for it, with its own
  isolation (see [the trust boundary](BLOCK_IR.md#the-trust-boundary)).
- **Enforce `allowed-tools`.** The specification marks the field
  experimental and leaves support to each client. Approval policy lives in
  interceptors, and an application can write one that consults
  `Skill.AllowedTools`.
- **User-invoked activation.** There is no `/skill-name` syntax. An
  application that wants one can look the skill up with `lib.Get` and put its
  body into the turn itself.
- **Deduplicate activations or protect them from compaction.** dialogram does
  not compact history. A repeated `load_skill` call returns the same
  instructions again.
- **Unicode normalisation of names.** Names are compared to directory names
  byte for byte. The reference validator normalises both to NFKC first, which
  only matters for non-ASCII names.

## Writing a good skill

- The description is all the model reads before deciding to load. Say what
  the skill does and name the requests that should trigger it.
- Keep `SKILL.md` under 500 lines, about 5000 tokens. Move detail into
  `references/` files, one level deep.
- Refer to bundled files by their path relative to the skill directory, the
  way the activation result lists them.
