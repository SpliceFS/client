package main

import (
	"os"
	"testing"

	"github.com/BlueRival/splicefs/internal/cli"
)

func TestMainPrintsVersion(t *testing.T) {
	oldExit, oldArgs := exit, os.Args
	t.Cleanup(func() { exit, os.Args = oldExit, oldArgs })

	status := -1
	exit = func(code int) { status = code }
	os.Args = []string{"splicefs", "--version"}

	main()

	if status != cli.ExitOK {
		t.Errorf("exit status = %d, want %d", status, cli.ExitOK)
	}
}
