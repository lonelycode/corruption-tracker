// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package storetest_test

import "github.com/lonelycode/corruption-tracker/store"

func storeActor(email string) store.Actor {
	return store.Actor{ExternalRef: "test:" + email, Label: email, Email: email}
}
