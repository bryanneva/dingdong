---
name: Feature
about: New feature or enhancement
labels: enhancement
---

<!--
ISSUE BODY DOES NOT DUPLICATE STRUCTURED METADATA.

Source of truth (do NOT inline these as body text):
- Priority / phase / sequencing → PRD doc (linked under "Source of truth" below)
  + GitHub milestone (sets via `gh api repos/.../issues/N -F milestone=M`)
- Cross-issue dependencies → GitHub native sub-issues / tracked-by relationships
  (set via the GH issue UI "Development → Tracked by", or GraphQL
  `addSubIssue` mutation), AND the PRD's parallelism matrix
- Project membership → project-queue (`mcp__project-queue__update_work_item`)

If you find yourself writing "Blocked by: #N" as body text, STOP. Either:
  (a) The dependency belongs in the PRD's parallelism matrix (cross-cutting), OR
  (b) The dependency belongs as a GitHub native sub-issue/tracked-by link.
Body text duplicating structured fields goes stale on every priority shift —
this is what we're explicitly preventing. See PRD §11 for the IoC rationale.
-->

## Source of truth

<!-- One link, one line. The PRD/ADR is authoritative. If sequencing or
     dependencies change later, the PRD changes — this issue body does not. -->

Tracks: <!-- e.g., docs/prd/<name>.md (§<section>) — or "no PRD; standalone" -->

## Context

<!-- Why this work exists. What problem it solves. -->

## What needs to happen

<!-- Concrete deliverables. -->

-

## Exit criteria

<!-- Testable conditions. Closing this issue requires all checked. -->

- [ ]
- [ ]

## Notes

<!-- Open questions, risks, related context. NOT a place to inline blocker
     numbers — those live in GitHub native dependencies + the PRD. -->
