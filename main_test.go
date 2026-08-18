package main

import (
	"strings"
	"testing"

	"agents-session-manager/internal/model"
)

func TestFindSession(t *testing.T) {
	ss := []model.Session{
		{Kind: model.Claude, ID: "aaaa-1111", Title: "Fix the widget"},
		{Kind: model.Claude, ID: "bbbb-2222", Title: "Other"},
		{Kind: model.Grok, ID: "cccc-3333", Title: "Fix the widget"},
	}

	got, err := findSession(ss, "aaaa-1111")
	if err != nil || got.ID != "aaaa-1111" {
		t.Fatalf("by id: %+v %v", got, err)
	}

	got, err = findSession(ss, "other")
	if err != nil || got.ID != "bbbb-2222" {
		t.Fatalf("by name: %+v %v", got, err)
	}

	if _, err := findSession(ss, "Fix the widget"); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ambiguous name: %v", err)
	}
	if _, err := findSession(ss, "nope"); err == nil {
		t.Fatal("expected not found")
	}
}
