// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package fraudagency

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"
)

func TestCheckIMAP(t *testing.T) {
	srv := server.New(memory.New())
	srv.AllowInsecureAuth = true
	var wire lockedBuffer
	srv.Debug = &wire
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { _ = srv.Serve(l) }()

	ok := IMAPConfig{Addr: l.Addr().String(), Username: "username", Password: "password", Insecure: true}
	n, err := CheckIMAP(ok)
	if err != nil || n != 1 { // the memory backend's INBOX holds one sample message
		t.Errorf("CheckIMAP = %d, %v; want 1, nil", n, err)
	}
	// A connection test must not touch flags: EXAMINE, never SELECT.
	if s := wire.String(); !strings.Contains(s, " EXAMINE INBOX") || strings.Contains(s, " SELECT ") {
		t.Errorf("CheckIMAP did not open the folder read-only; wire:\n%s", s)
	}
	bad := ok
	bad.Password = "wrong"
	if _, err := CheckIMAP(bad); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("bad password: err = %v", err)
	}
	missing := ok
	missing.Folder = "NoSuchFolder"
	if _, err := CheckIMAP(missing); err == nil || !strings.Contains(err.Error(), "select") {
		t.Errorf("missing folder: err = %v", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
