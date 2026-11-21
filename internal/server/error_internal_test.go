package server

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorChainLogsFullCause(t *testing.T) {
	err := fmt.Errorf("upload failed: %w", fmt.Errorf("commit object: %w", errors.New("connection refused")))
	want := "upload failed: commit object: connection refused -> commit object: connection refused -> connection refused"
	if got := errorChain(err); got != want {
		t.Fatalf("errorChain = %q, want %q", got, want)
	}
}
