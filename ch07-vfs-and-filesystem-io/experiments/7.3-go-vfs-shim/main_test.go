package main

import (
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
)

func rolledBackOutcome() outcome {
	return outcome{stepErr: "io", stepCode: 10, userVersion: faultStep - 1, integrity: "ok", match: recipe.Match{Step: faultStep - 1, Exact: true}}
}

func committedOutcome() outcome {
	return outcome{userVersion: faultStep, rowPresent: true, integrity: "ok", match: recipe.Match{Step: faultStep, Exact: true}}
}

// TestTrialHeld verifies that trialHeld rejects unfired in-range faults,
// masked-only hash matches, close errors, wrong error codes, and fired controls.
func TestTrialHeld(t *testing.T) {
	if !trialHeld(1, 3, true, rolledBackOutcome()) {
		t.Fatal("fired rollback rejected")
	}
	if trialHeld(1, 3, false, committedOutcome()) {
		t.Fatal("unfired fault inside the step accepted")
	}
	masked := rolledBackOutcome()
	masked.match.Exact = false
	if trialHeld(1, 3, true, masked) {
		t.Fatal("masked-only match accepted")
	}
	closeFailed := rolledBackOutcome()
	closeFailed.closeErr = "boom"
	if trialHeld(1, 3, true, closeFailed) {
		t.Fatal("close error accepted")
	}
	wrongCode := rolledBackOutcome()
	wrongCode.stepCode = 1
	if trialHeld(1, 3, true, wrongCode) {
		t.Fatal("non-IOERR accepted")
	}
	if !trialHeld(4, 3, false, committedOutcome()) {
		t.Fatal("unreached fault with commit rejected")
	}
	if trialHeld(4, 3, true, committedOutcome()) {
		t.Fatal("fault past the end fired")
	}
	maskedCommit := committedOutcome()
	maskedCommit.match.Exact = false
	if trialHeld(4, 3, false, maskedCommit) {
		t.Fatal("masked commit accepted")
	}
}
