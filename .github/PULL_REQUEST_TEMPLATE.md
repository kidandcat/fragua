<!--
Thanks for the PR. Couple of quick checks:

- One change per PR. Bundle a refactor with the bug fix it enables.
- `go test ./...` is green.
- If you added or changed a script verb, `internal/script/usage.go`
  and the dispatcher stay in sync.

Drop the comment markers and fill in the sections below.
-->

## Summary

What changed and why.

## How to verify

- [ ] Tests added or existing tests still pass.
- [ ] End-to-end check on a real board (`route` / `pack` / etc.).

## Notes for reviewers

Anything tricky, anything you want a second opinion on, anything you
deferred.
