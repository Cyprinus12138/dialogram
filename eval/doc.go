// Package eval is the evaluation harness for a dialogram agent, organised in
// three cheapest-first layers so the free tiers can gate every pull request
// and only the paid tier runs on a schedule:
//
//   - Layer 1 — deterministic loop evals. These live as tests next to the
//     providers: they drive the real loop against a scripted upstream and a
//     stubbed registry, asserting the machinery (step cap, pause detection,
//     history assembly, cache_control placement) at zero model cost.
//
//   - Layer 2 — the grounding / no-fabrication checker (grounding.go). When
//     the system prompt mandates that every figure or named entity in a
//     reply appear in a tool result of the same turn, hallucination becomes
//     deterministically checkable without a judge. Runs over recorded
//     transcripts in CI.
//
//   - Layer 3 — the scenario suite and the LLM-as-judge (scenario.go,
//     judge.go). Golden scenarios are block-IR fixtures; a rubric judge
//     scores faithfulness, citation, conciseness, tool selection, honesty on
//     empty results and language match. The judge costs money, so it is
//     nightly-only and gated behind RUN_LLM_EVALS.
//
//   - Layer 3b — the honesty-guard A/B (honesty.go). A paired before/after
//     measurement of agent.Guard: one upstream call per turn, the raw reply
//     and the guarded reply both audited by an independent judge, with the
//     full transcript recorded so the guard can be replayed over real model
//     output in CI for free.
//
// Cross-cutting: budget.go asserts per-profile cost/latency budgets over a
// completed ChatOutput, and manifest.go pins each tool's contract hash plus
// the prompt fingerprint so a tool or prompt change is a visible,
// re-baseline event rather than a silent regression.
package eval
