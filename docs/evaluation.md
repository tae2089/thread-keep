# Agent Integration Evaluation Plan

The deterministic guarantees of Thread Keep — conflict resolution, stale-context
blocking, replication, authorization — are covered by unit tests and the Docker
E2E suite. This document defines the **probabilistic** questions that tests
cannot answer, how each will be measured, and its current status. None of these
metrics are collected yet; this plan is the contract for the evaluation harness.

## Method common to all metrics

- **Fixture repositories**: frozen mid-size repositories with committed context
  (curated notes across kinds and entities), plus a task set of realistic
  changes (bug fix, refactor, feature) with known-correct outcomes.
- **Paired A/B runs**: the same agent, model version, and task prompt executed
  with and without the Thread Keep MCP server attached. Multiple seeds per
  task; report medians with dispersion.
- **No self-grading**: rubric scoring uses a separate judge configuration from
  the executing agent, with the rubric checked into the fixture.

## Metrics

### 1. Draft appropriateness (does the agent write the right notes?)
- Sample: agent sessions over the task set with drafting enabled.
- Measures: (a) entity precision — drafted note bound to the entity a curator
  would pick; (b) kind accuracy against curator labels; (c) evidence rate —
  fraction of note bodies citing a diff, test, issue, or user statement;
  (d) duplication rate — notes that should have been `note_revise` of an
  existing note.
- Acceptance: evidence rate ≥ 0.9, duplication ≤ 0.1 before enabling drafting
  by default in any workflow.

### 2. Stale-context protection in practice
- The mechanism is deterministic (covered by E2E); the open question is
  whether agents *respect* it: after a `needs_review` transition, does the
  agent act on the hidden stale note anyway (from prior conversation memory)?
- Measure: contamination rate — tasks where the agent asserts a stale
  contract that `context get` no longer serves.

### 3. Exploration cost (tool calls and tokens)
- Compare A/B: number of file-reading/search tool calls and total tokens
  until first correct edit, per task.
- Hypothesis: `search`/`context_get` replaces part of the exploratory reads.
- Report: median reduction; flag tasks where MCP output *added* tokens
  without reducing exploration (net-negative cases matter as much).

### 4. Outcome quality (review quality, task success)
- Task success: pass/fail against fixture-defined acceptance (tests pass,
  behavior matches) per task, A/B.
- Review quality: for review-type tasks, recall of seeded defects A/B —
  specifically defects whose detection requires historical context
  (a `constraint`/`warning` note) rather than local code reading.

### 5. Human acceptance of agent drafts
- Field metric, not a lab metric. Source of truth already exists in the data
  model: every committed note revision carries `origin`; drafts that humans
  discard never enter a commit.
- Measure: acceptance rate = agent-origin revisions in committed snapshots ÷
  agent-origin pending notes drafted (drafted count requires a small local
  event log or periodic `diff` sampling — the one instrumentation gap).
- Interpretation: a low rate signals noisy drafting (tighten the evidence
  gate); a very high rate with high volume signals rubber-stamping (sample
  audits needed).

## Status

| Metric | Status |
| --- | --- |
| Deterministic integrity (conflict, staleness, replication, auth) | Covered by E2E |
| 1–4 | Planned; harness not built |
| 5 | Derivable from history except drafted-count instrumentation |

## Deferred related work

- Remote read-only MCP endpoint (requires a server-side derived projection;
  transfer objects stay verbatim).
- `thread-keep stats` for acceptance-rate reporting from local history.
