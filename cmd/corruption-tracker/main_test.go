// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package main

import (
	"bytes"
	"log/slog"
	"regexp"
	"testing"
	"time"
)

// Log lines keep UTC timestamps when time.Local is the site time zone.
func TestLogTimesStayUTC(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatal(err)
	}
	time.Local = auckland
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: utcLogTime})).Info("hello", "at", time.Now())
	if !regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d+Z level=INFO msg=hello `).Match(buf.Bytes()) {
		t.Errorf("log line = %q, want a UTC (…Z) time", buf.String())
	}
}
