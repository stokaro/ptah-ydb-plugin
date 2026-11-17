# AGENTS.md

This repository provides the executable YDB provider for Ptah. Build, test,
and release procedures live in [CONTRIBUTING.md](CONTRIBUTING.md).

## Pull request descriptions

### Purpose and scope

- Read and follow `.github/pull_request_template.md`, including when
  creating or updating a PR through a CLI or API. Keep its headings and
  remove drafting comments and unused optional content.
- Write in plain American English and follow the repository's title
  convention. The title and first paragraph must make the purpose clear
  to a reviewer who knows the project but not the investigation. Define
  uncommon terms before using them. Do not add tool signatures or
  generated-with footers.
- Establish intent from the request, issue, discussion, and supporting
  evidence. Do not invent user impact, urgency, performance gains, or
  security benefits. Ask for clarification when missing information
  materially changes the goal, scope, acceptable risk, or completion
  criteria.
- Explain the problem or need and the resulting behavior. For a bug fix,
  explain the established cause. Distinguish observations, conclusions
  drawn from code, and hypotheses.
- Describe meaningful limitations and non-obvious decisions. Include
  implementation details when they help assess correctness, risk, or use.
  Avoid file inventories, commit histories, and repeated summaries.
- Keep the essential explanation in the PR. Link issues that define its
  scope, required companion changes, and relevant remaining work.
  Use closing keywords only when the PR fully resolves an issue.

### Evidence and maintenance

- Report observed validation results and important coverage gaps.
  Explain what each significant check establishes. Do not present lint,
  compilation, skipped checks, or planned work as proof of behavior they
  do not verify.
- State the engines, versions, platforms, or component combinations
  actually exercised when they affect the claim. Provide manual steps
  only where they add useful verification.
- Before publishing, inspect the complete diff from the actual base
  branch to the current head. When updating an existing PR, read its
  current title and description first.
- After substantive changes, revise the description for the complete
  current PR. Update behavior, scope, risks, dependencies, and validation
  claims; replace stale text instead of appending a development diary.
- Keep the description proportional to the change. Put essential results,
  material risks, and remaining uncertainty in the main text. Link or
  collapse lengthy supporting evidence.
- Follow all repository-specific testing, documentation, safety, and
  compatibility requirements alongside this description standard.

The shared wording of this section and the PR template is maintained in
[`stokaro/ptah`](https://github.com/stokaro/ptah). Keep copies aligned across
Ptah repositories; put repository-specific additions in a separate section.

## Provider-specific PR details

- For a protocol or capability change, identify the affected contract or
  negotiated feature and the minimum host it needs. Keep the SDK dependency,
  startup contract, and feature requirements distinct.
- State which provider and Ptah host builds, YDB release lines, and relevant
  server feature settings exercised the behavior. Distinguish offline
  conformance, live provider tests, and tests that drive the Ptah binary.
  Disclose skipped tests and the environment or host prerequisite they lacked.
- For changes that affect stored data or schema execution, describe relevant
  partial-application, transaction, locking, or recovery implications and what
  was read back from the database. Identify any required companion host change.
- Use the existing [build and test](CONTRIBUTING.md#build-and-test),
  [host-driven integration](CONTRIBUTING.md#host-driven-integration-checks),
  and [release](CONTRIBUTING.md#releases) procedures for applicable checks.
  A run against an unmerged host ref establishes behavior for that ref;
  it does not establish support in a released host.
