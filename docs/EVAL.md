# Evaluation harness

Three layers, cheapest first, so the free tiers gate every pull request and
only the paid tier runs on a schedule.

## Layer 1 — deterministic loop evals (CI, no model, free)

These live as tests next to the providers. They drive the **real** loop
against a scripted upstream (`httptest`) and a stubbed tool registry,
asserting the machinery:

- step cap → one tool-free synthesis call,
- pause detection stops the loop with no dispatch and no synthesis,
- history assembly (tool results threaded back into the next request),
- mixed-turn deferral (real tools run, the pause is paired with a correction),
- `ChatInput.Tools` narrows the on-the-wire tool envelope,
- Anthropic `cache_control` placement: stable system block + last tool +
  last message carry the breakpoint; the volatile block does not.

`agent.PromptFingerprint` / `Profile.Fingerprint()` are the
**prompt-regression gate**: pin them in a golden test, and a prompt edit
fails CI until the golden is re-baselined — a conscious act that flags the
scenario scores for a fresh look.

## Layer 2 — grounding checker (CI, no judge, free)

`eval.CheckGrounding(reply, toolResults)`. When the prompt mandates that
every figure (currency amounts, comma-grouped numbers, sample sizes) and
every uppercase entity token in a reply appear verbatim in a tool result of
the same turn, hallucination is deterministically checkable. Both sides are
normalised the same way, only `model_observation` counts, and ranges are
checked at both endpoints. `CheckTranscript` runs it over recorded
block-IR conversations in `testdata/transcripts/`.

It is a screen, not a proof — see the caveat in the code about adjacent
digit runs — and `GroundingOptions` lets you add stopwords or disable the
entity class.

## Layer 3 — scenarios and the LLM-as-judge (scheduled, costs money)

- `scenario.go` — golden scenarios as block-IR fixtures
  (`testdata/scenarios/`) with deterministic expectations (grounding,
  required/banned substrings, language match) that run free in CI against a
  recorded reply, plus rubric floors judged on the paid path.
- `judge.go` — the rubric (faithfulness, citation, conciseness, tool
  selection, honesty on empty, language match), a pure prompt builder, a
  robust score parser, and `LLMJudge` over a `Completer` seam.
- `completer.go` — `HTTPCompleter`, an isolated OpenAI-compatible
  single-shot client with exponential backoff on 429/5xx/network.

```
RUN_LLM_EVALS=1 OPENAI_API_KEY=… OPENAI_BASE_URL=… EVAL_JUDGE_MODEL=… \
  EVAL_REPORT_PATH=./agent-eval-report.json \
  go test ./eval/ -run TestNightly_JudgeScenarios -v
```

Only genuine rubric-floor failures turn the run red. Two classes of "could
not obtain a judgment" are kept off the red path so an infrastructure hiccup
never masquerades as an agent regression: missing judge secrets self-skip,
and a transient judge failure after retries is recorded as `errored` (a
separate `JudgeReport` bucket) and skipped. A permanent config error (401,
400) surfaces immediately.

## Layer 3b — honesty-guard A/B

See [HONESTY_AB.md](HONESTY_AB.md).

## Cross-cutting

- `budget.go` — per-profile cost/latency envelopes (steps, output tokens,
  follow-up cache-read ratio) asserted over a completed `ChatOutput`, using
  the same helpers the production metrics use so an eval breach and an SLO
  breach are measured on identical numbers.
- `manifest.go` — pins each tool's contract by a content hash of its wire
  spec plus the profile's prompt fingerprint. A golden test over the
  built-in tools (`testdata/manifest_builtin_tools.golden.json`) fails CI
  with the exact diff when a contract moves; re-pin a reviewed change with
  `UPDATE_GOLDEN=1`.
- `agent.Metrics` exports the same budget signals to Prometheus
  (`dialogram_agent_turns_total`, `…_turn_tool_steps`,
  `…_turn_output_tokens`, `…_turn_cache_read_ratio`).
