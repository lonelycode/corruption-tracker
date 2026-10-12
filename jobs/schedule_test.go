// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package jobs

import (
	"regexp"
	"slices"
	"testing"
)

// scheduleGrammar is the jobs.tsv schedule column grammar.
var scheduleGrammar = regexp.MustCompile(
	`^(interval:[1-9][0-9]*|calendar:([01][0-9]|2[0-3]):[0-5][0-9](/dow=[0-6]|/dom=([1-9]|[12][0-9]|3[01])(/months=([1-9]|1[0-2])(,([1-9]|1[0-2]))*)?)?)$`)

func TestScheduleIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, j := range Schedule {
		if seen[j.Name] {
			t.Errorf("duplicate job name %q", j.Name)
		}
		seen[j.Name] = true
		if !regexp.MustCompile(`^fraud-[a-z-]+$`).MatchString(j.Name) {
			t.Errorf("%s: name must be fraud-<kebab>", j.Name)
		}
		if !scheduleGrammar.MatchString(j.Schedule) {
			t.Errorf("%s: schedule %q does not match the jobs.tsv grammar", j.Name, j.Schedule)
		}
		if j.Lock != LockNone && j.Lock != LockLLM {
			t.Errorf("%s: lock %q is not none|llm", j.Name, j.Lock)
		}
		// Every LLM job takes the llm lock, and only LLM jobs do.
		if (j.Lock == LockLLM) != slices.Contains(j.Needs, NeedLLM) {
			t.Errorf("%s: lock %q disagrees with needs %v", j.Name, j.Lock, j.Needs)
		}
		for _, n := range j.Needs {
			if n != NeedLLM && n != NeedIMAP {
				t.Errorf("%s: unknown need %q", j.Name, n)
			}
		}
		if len(j.Args) == 0 {
			t.Errorf("%s: no args", j.Name)
		}
	}
}

func TestScheduleGrammarRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"", "calendar:7:15", "calendar:24:00", "calendar:07:15/dow=7",
		"calendar:07:15/dom=0", "calendar:07:15/months=1", "interval:0", "interval:abc",
		"calendar:07:15/dom=1/months=13",
	} {
		if scheduleGrammar.MatchString(s) {
			t.Errorf("grammar accepted malformed schedule %q", s)
		}
	}
}
