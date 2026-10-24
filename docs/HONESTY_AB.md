# Measuring an honesty guard

A server-side guard that redacts figures the model states without data is
easy to write and easy to believe in. This document is about not believing
in it: how `eval.HonestyRunner` measures a guard, and the failure modes a
guard has to be designed against.

## Background

Prompt hardening against fabrication under pressure (a user pushing three
times for "just a ballpark") tends to displace the fabrication rather than
remove it: the model states the figure in whichever phrasing the prompt did
not enumerate. A turn-level guard is the engineering answer: on a turn with
no successful grounding tool call, a reply that states a regulated figure is
rewritten to an honest placeholder.

Such a guard is easy to ship and hard to see working. Whether it fires on
the turns that actually fabricate, and whether it damages grounded replies,
only shows up in a paired measurement against an independent judge, which
reports the guard-vs-judge confusion matrix (TP, FN, FP, TN) alongside the
hallucination rate before and after the guard.

## Design of the measurement

**Paired.** One upstream call per turn. The raw provider reply and the
reply after the guard are both audited, so before/after is compared on
identical model output rather than two independent samples. The model is
fed its own raw reply as history, so every turn stays paired (the
second-order effect of the model seeing redacted text is out of scope).

**Judged independently.** A different, stronger model lists every
unsupported claim in the reply against the tool results it saw, by kind
(money, percent, duration, quantity, entity_fact, regulation, other), and
flags which are likely false. The guard's own regexes are deliberately not
the metric — measuring a regex redactor with the same regex trivially reads
0%.

**Traps and controls.** Trap cases push the model to fabricate (a soft-failed
lookup plus pressure). Control cases have the data available and assert the
guard did not damage a grounded reply (`no_redaction`, `must_keep_figures`).

**Recorded.** With `RecordDir` set, every case writes its full block-IR
transcript, raw output, guarded reply and both verdicts. `ReplayRecord`
re-applies the guard to the recorded raw output and must reproduce the
guarded reply byte for byte — the captured fabricated strings become the
guard's regression corpus, replayed in CI for free.

**Production envelope.** `FixtureRegistry` builds the case registry from the
real tool contracts (names, descriptions, schemas) with bodies swapped for
fixtures, so the model sees exactly what production shows it.

## Failure modes to design against

1. **A soft-failed grounding tool counted as grounding.** A guard that
   checks tool *names* rather than *outcomes* treats a turn as grounded when
   the model called the right tool, got `error_code` back, and stated the
   figure anyway. That is the exact failure the guard exists for.
2. **Phrasing displacement past the signatures.** On no-tool turns the model
   states the figure without the context word a regex keys on, or in a form
   the signatures do not cover (counts instead of durations, bare
   arithmetic). Enumerating contexts keeps losing to displacement.
3. **Out of scope by design.** Rule-like facts stated from memory. A regex
   cannot tell a right rule from a wrong one; those need a verified
   knowledge base the prompt cites, not a redactor.
4. **False positives on restatements.** An exemption that covers only some
   restated numbers (say, three-digit amounts) redacts an honest restatement
   of another tool-produced number, such as a minute count.

## What is built into `agent.Guard`

- **Grounding means a successful result, not a call.** `ResultIsSuccess`
  rejects any result carrying `error_code` / `soft_failure`, directly or in
  an envelope's `model_observation`. A same-turn call whose outcome is not
  yet known still counts (the text streamed alongside it cannot be held
  until the tool returns).
- **Restating any number a tool produced is honest.** The exemption applies
  to every number of at least `MinRestatementDigits` digits (default 2) found
  in earlier tool results or inputs.
- **Signatures are configuration.** Default detectors cover currency amounts
  and percentages; applications add their own kinds and can narrow which
  tools ground which kind (`KindGrounding`).
- **Fire rate is observable** through the redactions `GuardReply` returns
  and the warnings it logs, instead of a silent rewrite.
- **Text the user reads in a pause is checked like the reply.** The input of
  a pause tool (the built-in human-in-the-loop tools, plus any listed in
  `Guard.PauseTools`) is written by the model for the user: a question, the
  options, a proposal card. It is redacted with the reply, in the persisted
  `tool_use` block and in the `waiting_for_user` event alike, and its
  numbers never count as grounded, so a figure cannot be slipped into a
  pause and then restated as if a tool had produced it.

What the guard cannot do is point 3 above: a qualitative claim ("a
non-mature estate") is not a figure, and no signature sees it. Where such a
claim would land on something the user acts on, take the field away from
the model. `examples/chatserver`'s `propose_shortlist` takes only a project
name, and the card the user sees is built from the server's project record.

## Running it

```
RUN_LLM_EVALS=1 \
LLM_PROVIDER=anthropic ANTHROPIC_API_KEY=… ANTHROPIC_MODEL=… \
EVAL_JUDGE_BASE_URL=… EVAL_JUDGE_API_KEY=… EVAL_JUDGE_MODEL=… \
HONESTY_RECORD=1 HONESTY_REPORT_PATH=./honesty-report.json \
  go test ./eval/ -run TestNightly_HonestyGuardAB -v -timeout 45m
```

The test asserts only what is deterministic: no control expectation may
fail, and the guarded trap set may not be worse than raw. The rates
themselves are the measurement and land in the report (JSON plus a Markdown
twin). `HONESTY_CASE=<id>` runs one case; `HONESTY_CONCURRENCY` bounds cases
in flight.

Writing cases: keep traps multi-turn (the pressure pattern is what leaks),
read every fixture figure from a real data source so a judge's
"unsupported" can never mean "implausible fixture", and include a few
controls that a too-narrow restatement exemption would fail.
