# Contributing

Thanks for your interest. Please read this whole page before opening a pull
request.

## Outside code contributions are not accepted yet

This project is licensed under the AGPL-3.0, and its copyright holder also
ships it inside another service. Before any outside contribution can be
merged, a contributor licence grant has to be in place: a Contributor
Licence Agreement that lets the copyright holder license your contribution
under other terms as well as the AGPL. A Developer Certificate of Origin
sign-off on its own is not enough, because it carries no relicensing grant.

Until that agreement is published and enforced on this repository, **pull
requests from outside contributors will be closed without review.** Issues,
bug reports and discussion are welcome now. This section will be replaced
with the agreement and how to sign it once it exists.

## Ground rules for every change

These apply to the maintainer as much as to future contributors.

- **Synthetic data only.** Fixtures, seeds, test names, examples, comments
  and screenshots must use invented people, companies and case numbers. Never
  copy a real name, court file number, amount or excerpt from a real matter,
  even a public one. Use reserved domains (`example.org`, `*.example`,
  `*.test`, `*.invalid`) for addresses and URLs.
- **The legal gates are not configuration.** The naming rule
  (`readmodel/naming.go` and its SQL mirrors), suppression screening and
  auto-quarantine, the ALLEGED labelling of pre-verdict matters, the report
  minimum cell size, export being off by default, and the ban on automated
  access to judgment databases are deliberate. A change that weakens any of
  them needs an explicit rationale in the pull request and review by the
  code owner.
- **Sources must be compliant.** A new connector must respect the site's
  robots.txt on every run (disable itself, don't argue), record the licence
  basis on every claim it creates, and route everything through the review
  queue. No connector may fetch from Judicial Decisions Online or NZLII.
- **Licence headers.** Every source file carries an
  `SPDX-License-Identifier` line and an `SPDX-FileCopyrightText` line in its
  native comment syntax.

## Development

See the README for building and running the tests. Run `go vet ./...` and
`go test ./...` (with `DATABASE_URL_TEST` set) before sending a change. A
test run that skips the database tests doesn't count. CI fails any run in
which a test skipped (`scripts/ci-no-skips.sh`).
