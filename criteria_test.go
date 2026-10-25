package main

import (
	"os"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// TestCriteria runs the scripts that prove the guarantees of the
// foundation with failpoints and many iterations, in testdata/criteria;
// make criteria runs them in a build with -tags failpoints.
func TestCriteria(t *testing.T) {
	if os.Getenv("OPSTRUSS_CRITERIA") == "" {
		t.Skip("set OPSTRUSS_CRITERIA=1, as make criteria does")
	}
	workRoot(t)
	testscript.Run(t, params("testdata/criteria"))
}

// TestLoad runs the load criteria in testdata/load after TestCriteria, as
// a test of its own, so no other script competes for the machine while
// it measures.
func TestLoad(t *testing.T) {
	if os.Getenv("OPSTRUSS_CRITERIA") == "" {
		t.Skip("set OPSTRUSS_CRITERIA=1, as make criteria does")
	}
	workRoot(t)
	testscript.Run(t, params("testdata/load"))
}
