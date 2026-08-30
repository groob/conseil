package main

import (
	"errors"
	"testing"

	"github.com/peterbourgon/ff/v4"
)

func TestCommandHelpAndUnknownCommand(t *testing.T) {
	if err := command().ParseAndRun(t.Context(), nil); !errors.Is(err, ff.ErrHelp) {
		t.Fatalf("no arguments error = %v, want help", err)
	}
	if err := command().ParseAndRun(t.Context(), []string{"unknown"}); err == nil || err.Error() != `unknown command "unknown"` {
		t.Errorf("unknown command error = %v, want exact unknown-command error", err)
	}
}

func TestCreateRejectsInvalidSpecBeforeBuildingWorker(t *testing.T) {
	err := command().ParseAndRun(t.Context(), []string{"create", "--project", "INVALID", "--task", "test", "--ref", "0123456789012345678901234567890123456789"})
	got := ""
	if err != nil {
		got = err.Error()
	}
	if want := `invalid lowercase project repository name "INVALID"`; got != want {
		t.Errorf("create error = %q, want %q", got, want)
	}
}
