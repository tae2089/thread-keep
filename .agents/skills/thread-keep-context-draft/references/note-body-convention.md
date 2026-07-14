# Note Body Convention

Choose one `NoteKind` before writing. Keep one primary durable claim per note. If a section contains an independently useful claim of another kind, split that claim into a separate note.

Use `Summary` for the selected kind's primary claim. Add `References` only when verified evidence exists. Then use only the optional sections for the selected kind.

## Intent

Record why the entity exists or which outcome it serves.

```text
Summary:
<purpose or desired outcome>

Rationale:
<why the purpose matters>

Non-goals:
<behavior outside the purpose, without a mandatory prohibition>

References:
<verified references>
```

A mandatory or forbidden behavior is a `constraint`, not an intent non-goal.

## Decision

Record a selected approach when alternatives or tradeoffs matter.

```text
Summary:
<selected approach>

Alternatives:
<options considered but not selected>

Tradeoffs:
<accepted costs and benefits of the selection>

References:
<verified references>
```

Split an independently actionable hazard from a tradeoff into a `warning` note.

## Constraint

Record mandatory, forbidden, bounded, or invariant behavior.

```text
Summary:
<constraint in concise terms>

Rule:
<the exact must, must-not, limit, or invariant>

Scope:
<where and when the rule applies>

References:
<verified references>
```

Do not hide a hard requirement inside an `intent`, `decision`, or `example` note.

## Example

Record an illustrative scenario and its observed or expected outcome.

```text
Summary:
<what the example demonstrates>

Scenario:
<input and relevant conditions>

Expected behavior:
<observable result>

References:
<verified references>
```

An example illustrates a contract but does not create one. Split normative behavior into a `constraint` note when it must hold beyond the scenario.

## Warning

Record a risk, hazardous condition, or easy-to-miss operational consequence.

```text
Summary:
<risk or hazard>

Trigger:
<condition that exposes the risk>

Mitigation:
<verified way to avoid or reduce the risk>

References:
<verified references>
```

Split a mandatory safety rule from its risk explanation into a `constraint` note.

## Reference rules

- Cite only references inspected in the diff, tests, repository, issue text, or explicit user statement.
- Prefer stable test names, symbols, headings, issue or ADR IDs, and commit SHAs over line numbers.
- Use repository-relative paths for local files.
- Preserve an exact verified URL when one is supplied; never invent or guess a URL.
- Do not fetch an external reference unless the user requested it and an authorized tool is available.
- Keep a short note as plain prose when optional sections would add only empty boilerplate.
