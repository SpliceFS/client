package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStatus int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"--version"}, wantStatus: ExitOK, wantStdout: "prog dev\n"},
		{name: "unknown flag", args: []string{"--nope"}, wantStatus: ExitUsage, wantStderr: "flag provided but not defined"},
		{name: "no arguments", args: nil, wantStatus: ExitError, wantStderr: "prog: not implemented yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := Run("prog", tt.args, &stdout, &stderr); got != tt.wantStatus {
				t.Errorf("status = %d, want %d", got, tt.wantStatus)
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunVersionWriteError(t *testing.T) {
	var stderr bytes.Buffer
	if got := Run("prog", []string{"--version"}, failingWriter{}, &stderr); got != ExitError {
		t.Errorf("status = %d, want %d", got, ExitError)
	}
}
