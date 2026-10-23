# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `agent`: provider-neutral block IR (`Block`, `Turn`), `Provider` contract,
  `Agent` wrapper, `Profile`/`Config` with per-profile system prompt,
  tool allow-list and step cap, interceptor chain with
  `ApprovalListInterceptor`, built-in and custom pause detection
  (`DetectPause`, `Config.PauseTools`), `SKILL.md` loader and `load_skill`
  tool, opt-in Prometheus `Metrics`, `PromptFingerprint`.
- `agent.Guard`: configurable honesty guard with `Signature` detectors,
  per-kind grounding sets, restatement exemption, and `WrapEmit` /
  `GuardReply` for the streaming and non-streaming paths.
- `tool`: `Tool`, `Registry`, three-channel `Envelope`, `ArtifactStore`,
  schema helpers, built-in `request_user_input` / `request_user_choice` /
  `request_user_form`, `present_data`, `render_from_ref`, `now`.
- `provider/anthropic`, `provider/openai`: wire adapters over the shared
  loop, with prompt-cache breakpoints (Anthropic), parallel tool dispatch,
  mixed-turn pause deferral, step-cap synthesis and transient-error retries.
- `streambuf`: `Store` contract, `MemStore`, `RedisStore` with configurable
  key prefix and TTL.
- `sse`: resumable Server-Sent Events handler with detached producer, idle
  watchdog, cancel endpoint, programmatic `Launch`, gen_ui conversion and an
  `OnFinish` persistence hook.
- `eval`: grounding checker, LLM-as-judge scenarios with floors and an
  errored-vs-failed report, manifest/prompt pins, budgets, and the
  honesty-guard A/B runner with recorded-corpus replay.
- `examples/chatserver`: runnable reference deployment with a vanilla-JS
  client that resumes streams after a reload.

### Changed (relative to the production code this was extracted from)
- Grounding is outcome-aware: a tool call whose result carries
  `error_code` / `soft_failure` no longer vouches for a figure
  (`agent.ResultIsSuccess`). This closes the largest leak class the
  production A/B measured.
- The restatement exemption applies to any number a tool produced with at
  least `Guard.MinRestatementDigits` digits (default 2), not only to
  three-digit amounts — the one observed false positive.
- Metrics are opt-in and registered on a caller-supplied registry under the
  `dialogram_agent_*` names; nothing auto-registers.
- Tool visibility is a per-profile allow-list plus a per-turn override,
  instead of per-tool mode restrictions on the registry.
- Neutral tool-use ids carry the `dg_` prefix.
