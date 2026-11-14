---
description: "Review release publication, tag verification, and the intentionally suspended immutable-tag prerequisite."
applyTo: ".github/workflows/release.yml,.github/release-tag-protection.json,tools/release/**"
---

# Release Policy Review

- The owner intentionally suspends the immutable-tag prerequisite while this private repository's GitHub plan does not support tag rulesets. The Release workflow reads repository variable `REQUIRE_TAG_POLICY`, defaulting to `false` when unset. Treat this specific disabled state and its remaining tag-check-to-publication race as an accepted configuration decision, not a new defect to report on each review. This is not a claim that tags are immutable.
- Suspension applies only to ruleset verification. Always review that the remote lightweight or annotated tag resolves to the exact tested revision immediately before publication, that `target_commitish` is pinned to that revision, and that missing, moved, unreadable, or malformed tags block publication. Report regressions in these checks.
- Keep the ruleset template, enforcement implementation, and both enabled/disabled regression tests committed. The CLI defaults to requiring policy; only the workflow's explicit repository-variable setting suspends that prerequisite. When `REQUIRE_TAG_POLICY` is `true`, missing, incomplete, inactive, or unreadable policy must block publication, with no exclusions or bypass actors permitted.
- Future activation is manual in GitHub's UI: the owner changes visibility, imports `.github/release-tag-protection.json` under Settings > Rules > Rulesets, then sets repository Actions variable `REQUIRE_TAG_POLICY` to `true`. Do not automatically change visibility, billing, rulesets, or the variable. Do not broaden this exception to other release, security, checksum, or CI issues.