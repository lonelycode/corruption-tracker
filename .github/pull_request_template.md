<!--
Outside contributions can't be merged yet: see CONTRIBUTING.md. Pull
requests from outside contributors are closed without review until the
contributor licence agreement is in place.
-->

## What and why

## Checks

- [ ] `go vet ./...` and `go test -p 1 ./...` pass with `DATABASE_URL_TEST` set (no skipped database tests)
- [ ] New source files carry SPDX headers
- [ ] Synthetic data only, in code, fixtures, tests and screenshots
- [ ] If this touches a legal gate (naming, suppression, quarantine, report cell size, export defaults), the rationale is above
