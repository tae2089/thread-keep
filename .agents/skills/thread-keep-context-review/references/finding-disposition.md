# Finding Disposition Contract

Classify a candidate finding after comparing implementation evidence with active Thread Keep context.

| Disposition | Use when | Review action |
| --- | --- | --- |
| Confirmed finding | Code or tests violate an active constraint, explicit contract, or independently demonstrated correctness or safety requirement. | Report the defect with evidence. |
| Context-supported exclusion | The alleged defect is exactly an intentional behavior or non-goal, the active note has traceable rationale, and no stronger contract is violated. | Exclude it from actionable findings; optionally record the note ID in the exclusion summary. |
| Design or context risk | Active context conflicts with tests, public contracts, security, data integrity, or verified current requirements. | Report the contradiction and ask which contract should change; do not disguise it as a simple implementation bug. |
| Insufficient evidence | Context is missing, stale, historical, unverified, or too vague to resolve the claim. | Continue investigating or state the evidence gap; do not assert a finding or exclusion from context alone. |

Context can explain why behavior exists. It cannot prove that the behavior is safe, correctly implemented, or still desired.
