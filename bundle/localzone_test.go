// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lonelycode/corruption-tracker/store/storetest"
)

// The standalone binary moves time.Local to the site time zone. An export
// must not change with it: Postgres renders the rows, and every Go-side
// time in the bundle is UTC. The manifest's snapshot (pg_current_snapshot,
// WAL position) is left out of the comparison: any write elsewhere on the
// cluster between the two exports moves it.
func TestExportIndependentOfLocalZone(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	st := storetest.Open(t)
	seed(t, st)

	time.Local = time.UTC
	utc, mUTC := exportBytes(t, st, pinned(quietLog()))
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatal(err)
	}
	time.Local = auckland
	// Another writer on the cluster between the two exports, as in a
	// full test run: it moves the snapshot but must not matter.
	if _, err := st.Pool.Exec(context.Background(), `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	nz, mNZ := exportBytes(t, st, pinned(quietLog()))

	mUTC.Snapshot, mNZ.Snapshot = Snapshot{}, Snapshot{}
	a, _ := json.Marshal(mUTC)
	b, _ := json.Marshal(mNZ)
	if !bytes.Equal(a, b) {
		t.Errorf("manifest differs with time.Local in Pacific/Auckland:\n%s\n%s", a, b)
	}
	eu, en := unpack(t, utc), unpack(t, nz)
	if len(eu) != len(en) {
		t.Fatalf("entries: %d vs %d", len(eu), len(en))
	}
	for i := range eu {
		if eu[i].hdr.Name == ManifestFile {
			continue
		}
		if eu[i].hdr.Name != en[i].hdr.Name || !eu[i].hdr.ModTime.Equal(en[i].hdr.ModTime) || !bytes.Equal(eu[i].body, en[i].body) {
			t.Errorf("%s differs with time.Local in Pacific/Auckland", eu[i].hdr.Name)
		}
	}
}
