// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

// Package licensecheck holds the test that every module source file carries
// SPDX licence and copyright headers (design D1.2). It has no production
// code.
//
// Third-party code vendored into the module (the vendored map) keeps its
// upstream licence, is exempt from AGPL headers, and must ship its upstream
// LICENSE file.
//
// The test is enforcing: a module source file without both headers fails
// it. Data files and other non-source files are covered by REUSE.toml at
// the module root instead.
package licensecheck
