<!-- Thank you for the contribution. Please fill in the sections below. -->

## Summary

<!-- 1–2 sentences: what changes and why. -->

## Type

<!-- Tick one (Conventional Commits prefix used in the squash-merge title). -->

- [ ] `feat` — new user-visible functionality
- [ ] `fix` — bug fix
- [ ] `docs` — documentation only
- [ ] `refactor` — internal change without behaviour change
- [ ] `test` — tests only
- [ ] `chore` — build, CI, dependencies

## Related issue

<!-- Use "Closes #N" to auto-close the linked issue on merge. -->

Closes #

## Checklist

- [ ] `go build ./...` succeeds.
- [ ] `go test -race ./...` passes.
- [ ] `gofmt -l .` returns no files.
- [ ] `go vet ./...` is clean.
- [ ] `golangci-lint run` returns no new issues.
- [ ] Tests added or updated for new behaviour.
- [ ] `CHANGELOG.md` updated under `[Unreleased]` if user-visible.
- [ ] Documentation in `docs/` updated if architecture, API, or SRS requirements change.
- [ ] No secrets, API keys, or personal data in the diff.

## Notes for reviewers

<!-- Optional: anything reviewers should focus on, edge-cases, follow-ups. -->
