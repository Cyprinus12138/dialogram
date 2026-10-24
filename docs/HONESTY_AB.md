# Measuring an honesty guard

A server-side guard that redacts figures the model states without data is
easy to write and easy to believe in. This document is about not believing
in it: how `eval.HonestyRunner` measures a guard, and what the first such
measurement taught.

## Background

The runtime this package was extracted from first tried prompt hardening
against fabrication under pressure (a user pushing three times for "just a
ballpark"). A live re-run showed the model displacing the fabrication into
whichever phrasing the prompt had not enumerated. A turn-level guard was
then added: on a turn with no grounding tool call, a reply that states a
regulated figure is rewritten to an honest placeholder. It was never
re-measured, and it emitted no counter.

The measurement that followed changed the hallucination rate by **zero**:
over 49 judged turns (41 trap, 8 control) the guard rewrote one turn, and
that turn was a *grounded* restatement — a false positive. The guard-vs-judge
confusion matrix read TP 0, FN 23, FP 1, TN 25.

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

## What the first measurement found

1. **A soft-failed grounding tool still counted as grounding** — 9 of 18
   leaked turns. The guard checked tool *names*, not *outcomes*. The model
   called the right tool, got `error_code` back, and stated the figure
   anyway; the guard treated the turn as grounded and never looked at the
   text. This was the exact failure class the guard was built for.
2. **Phrasing displacement past the signatures** — 5 of 18. On no-tool turns
   the model stated the figure without the context word the regex keyed on,
   or in a form the signatures did not cover (counts instead of durations,
   bare arithmetic). Enumerating contexts keeps losing to displacement.
3. **Out of scope by design** — 4 of 18 turns but the largest share of
   claims: rule-like facts stated from memory. A regex cannot tell a right
   rule from a wrong one; those need a verified knowledge base the prompt
   cites, not a redactor.
4. **The one redaction was a false positive**: a restated tool-produced
   minute count, outside the three-digit restatement exemption.

## What is built into `agent.Guard` as a result

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
controls that are *meant* to be red until the restatement exemption is
right.
