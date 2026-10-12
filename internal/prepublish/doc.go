// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package prepublish holds the repository's publication guards. It has no
// production code; the checks live in its tests and run with `go test ./...`:
//
//   - TestNoDeniedContent scans every text file in the module for things
//     that must never be published: credential shapes, private network
//     addresses, and email addresses outside reserved domains. Fixtures must
//     use reserved names (example.org, *.test, *.invalid, *.example,
//     TEST-NET IPs). Operator branding and infrastructure names are checked
//     by the embedding host against its own, unpublished list.
//   - TestDependencyLicences fails when a dependency that isn't in the
//     vetted, AGPL-compatible licence list enters the build.
//
// Neither check can spot real personal data. Fixtures and examples must use
// invented names; that is a review rule (CONTRIBUTING.md), not something a
// pattern list can enforce without itself publishing the names.
package prepublish
