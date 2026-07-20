package main

import "testing"

func TestParseAnswers(t *testing.T) {
	md := `# Krakoa needs answers — task-lifecycle-abc

refine needs answers: stuff

## Q1: which direction?

ANSWER: inbound only

## Q2: pin the revision?

ANSWER: yes, pin latest
spanning two lines

## Q3: unanswered

ANSWER:
`
	qs := []string{"which direction?", "pin the revision?", "unanswered"}
	got := parseAnswers(md, qs)
	if len(got) != 2 {
		t.Fatalf("want 2 answers, got %d: %v", len(got), got)
	}
	if got["which direction?"] != "inbound only" {
		t.Errorf("q1 = %q", got["which direction?"])
	}
	if got["pin the revision?"] != "yes, pin latest\nspanning two lines" {
		t.Errorf("q2 = %q", got["pin the revision?"])
	}
	if _, ok := got["unanswered"]; ok {
		t.Error("empty ANSWER block must not produce an answer")
	}
}
