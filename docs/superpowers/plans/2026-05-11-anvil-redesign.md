---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:writing-plans
lifecycle_status: draft
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# Implementation Plan Draft: anvil

This draft is not approved until `superpowers:writing-plans` and `plan-eng-review` complete.

## Phase 0 - Complete Design Brief

- Turn the generated spec into an approved information architecture and domain boundary map.
- Resolve grill-me questions and record the accepted answers in project-local artifacts.

## Candidate Tasks

- Reconcile existing architecture docs with current code and ADRs.
- Update lifecycle artifacts after domain-architecture and grill-me decisions.
- Add focused verification for any changed documentation, workflow, or runtime behavior.

## Expected Gates

- `plan-design-review`
- `plan-eng-review`
- `code-review`

## Verification Candidates

- Run lifecycle lint for this run.
- Run project-local doc/link checks when available.
- Run code tests only for approved runtime-affecting changes.

## Lifecycle Gate Evidence

- Stage: `superpowers:writing-plans`
- Status: `draft`
- Approved by: `not-approved`
- Evidence: Generated draft artifact. This gate is not passed yet.
