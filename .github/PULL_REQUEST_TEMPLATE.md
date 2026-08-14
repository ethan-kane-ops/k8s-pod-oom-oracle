## Description

What changed and why. Link the issue it addresses.

Fixes # (issue reference)

## Type of Change

- [ ] **Bug fix**, non-breaking change which fixes an issue
- [ ] **New feature**, non-breaking change which adds functionality
- [ ] **Breaking change**, alters behavior such that existing users would need to update
- [ ] **Documentation**, additions or changes to docs, examples, or manifests
- [ ] **Refactor**, restructuring or performance work with no behavior change

## Quality Checklist

- [ ] `just check` passes locally (tidy + lint + race tests).
- [ ] Tests added or updated to cover the change.
- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (e.g. `feat(detector): ...`).
- [ ] Documentation updated where behavior, flags, or the HTTP API changed.

## If you touched these, say so

- [ ] **`internal/detector/bpf/*.bpf.c`**: `just bpf-generate` was run and the regenerated `.o` and `.go` files are committed. CI's `bpf-verify` fails otherwise.
- [ ] **Anything the daemon does on a node**: `just e2e` was run, or the `run-e2e` label is applied to this PR so CI runs it.
- [ ] **`deploy/`**: the change was applied to a real cluster, not just parsed. RBAC and security context defects pass every unit test.
- [ ] **Build-tagged code** (`//go:build`): the tag is covered by a lint invocation. A tag no linter passes is a file no linter reads.
