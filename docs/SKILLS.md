# Skills

Skills are packaged workflow bundles that extend what the agent can do
without growing the stable system prompt. Each skill is a directory holding
a `SKILL.md`: YAML-subset frontmatter plus a markdown body.

Skills are advertised in the system prompt as one-line summaries; the full
body is loaded on demand by the model through the `load_skill` tool. The
stable prompt stays small (and cache-friendly) while bodies can be long.

## SKILL.md

```markdown
---
id: trip_planner
name: Trip planner
description: When the user wants a multi-day itinerary rather than a single lookup
version: 1
tool_allowlist: get_weather, search_flights
---

# Trip planner

1. Confirm dates and cities with request_user_form.
2. Check the forecast for each day with get_weather.
3. …
```

Required keys: `id`, `name`, `description`. Optional: `version` (integer,
default 1), `tool_allowlist` (comma-separated). Unknown keys are ignored so
the schema can evolve. Values may be quoted. Duplicate keys are an error.

## Loading

```go
lib, err := agent.LoadSkills(os.DirFS("."), "skills")   // one subdirectory per skill
agent.RegisterLoadSkill(reg, lib)                        // no-op when lib is nil/empty
```

Or build a library in code with `agent.NewSkillLibrary(&agent.Skill{…})`.
Load errors are fatal by design: a missing `SKILL.md`, malformed
frontmatter or a duplicate id fails startup rather than silently shipping a
partial library.

Pass the library to the provider (`Options.Skills`); `loop.ComposeStableSystem`
appends an "Available skills" block to the profile's system prompt, inside
the cached stable prefix:

```
## Available skills

You can load a skill's full instructions on demand by calling the
`load_skill` tool with its id. Load only when the user's request matches
one of these workflows — don't load preemptively.

- id: `trip_planner`
  name: Trip planner
  When to load: When the user wants a multi-day itinerary rather than a single lookup
```

## Tool allow-lists

`tool_allowlist` is metadata: the library parses and stores it, and does
not enforce it. Enforcement belongs to an `agent.Interceptor` that knows
which skill is active and denies tools outside its list. No allow-list means
the skill may use any tool.

## Good practice

- Deterministic ids: lowercase, underscore-separated.
- The description is the only thing the model reads before deciding to
  load; be specific about the user request that triggers the skill.
- Keep a body to one clear workflow; split beyond ~2000 words.
- Increment `version` on a breaking revision.
