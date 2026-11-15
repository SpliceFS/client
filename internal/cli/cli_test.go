package cli_test

import (
	"bytes"
	"errors"
	"flag"
	"slices"
	"testing"

	"github.com/SpliceFS/client/internal/cli"
	"github.com/SpliceFS/client/internal/version"
)

func tool() cli.Program {
	return cli.Program{Name: "tool", Usage: "usage: tool [--config DIR]\n"}
}

func TestHandled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		showVersion bool
		wantDone    bool
		wantStatus  int
		wantOut     string
		wantErr     string
	}{
		{"nothing to answer", nil, false, false, 0, "", ""},
		{"help", flag.ErrHelp, false, true, cli.ExitOK, tool().Usage, ""},
		{
			"help wrapped", errors.Join(errors.New("context"), flag.ErrHelp), false, true, cli.ExitOK, tool().Usage, "",
		},
		{
			"bad command line", errors.New("flag provided but not defined: -x"), false, true, cli.ExitUsage, "",
			"tool: flag provided but not defined: -x\n" + tool().Usage,
		},
		{"version", nil, true, true, cli.ExitOK, "tool " + version.String() + "\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			status, done := tool().Handled(tt.err, tt.showVersion, &out, &errOut)
			if done != tt.wantDone || status != tt.wantStatus || out.String() != tt.wantOut || errOut.String() != tt.wantErr {
				t.Errorf("Handled = %d, %v, stdout %q, stderr %q; want %d, %v, %q, %q",
					status, done, out.String(), errOut.String(), tt.wantStatus, tt.wantDone, tt.wantOut, tt.wantErr)
			}
		})
	}
}

func TestProgramNameAndArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		argv     []string
		wantName string
		wantArgs []string
	}{
		{"name only", []string{"tool"}, "tool", nil},
		{"with a path", []string{"/usr/bin/tool", "a", "b"}, "tool", []string{"a", "b"}},
		{"no argv", nil, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, args := cli.ProgramName(tt.argv), cli.Arguments(tt.argv)
			if name != tt.wantName || !slices.Equal(args, tt.wantArgs) {
				t.Errorf("ProgramName, Arguments of %q = %q, %q; want %q, %q", tt.argv, name, args, tt.wantName, tt.wantArgs)
			}
		})
	}
}

func TestFail(t *testing.T) {
	t.Parallel()
	var errOut bytes.Buffer
	if status := tool().Fail(&errOut, errors.New("boom")); status != cli.ExitError || errOut.String() != "tool: boom\n" {
		t.Errorf("Fail = %d, stderr %q", status, errOut.String())
	}
}

func TestFlags(t *testing.T) {
	t.Parallel()
	var flags cli.Flags
	fs := cli.NewFlagSet("tool")
	flags.Register(fs)
	if err := cli.ParseFlags(fs, []string{"--config", "/x", "--version", "rest"}); err != nil {
		t.Fatal(err)
	}
	if flags.ConfigDir != "/x" || !flags.ShowVersion || fs.NArg() != 1 {
		t.Errorf("flags = %+v, %d arguments left", flags, fs.NArg())
	}
}

func TestParseFlagsKeepsHelpRecognizable(t *testing.T) {
	t.Parallel()
	fs := cli.NewFlagSet("tool")
	if err := cli.ParseFlags(fs, []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("error = %v, want flag.ErrHelp", err)
	}
	err := cli.ParseFlags(fs, []string{"--bogus"})
	if err == nil || err.Error() != "flag provided but not defined: -bogus" {
		t.Errorf("error = %v", err)
	}
}

func TestResolveConfigDir(t *testing.T) {
	t.Parallel()
	boom := errors.New("no home")
	tests := []struct {
		name    string
		given   string
		lookup  func() (string, error)
		want    string
		wantErr error
	}{
		{"given", "/given", func() (string, error) { return "", errors.New("must not be asked") }, "/given", nil},
		{"default", "", func() (string, error) { return "/default", nil }, "/default", nil},
		{"no default", "", func() (string, error) { return "", boom }, "", boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			flags := cli.Flags{ConfigDir: tt.given}
			err := flags.ResolveConfigDir(tt.lookup)
			if !errors.Is(err, tt.wantErr) || flags.ConfigDir != tt.want {
				t.Errorf("ConfigDir = %q, error %v; want %q, %v", flags.ConfigDir, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestSayfAndAnnouncef(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cli.Sayf(&out, "%s-%d", "a", 1)
	if status := cli.Announcef(&out, "%s", "b"); status != cli.ExitOK || out.String() != "a-1b" {
		t.Errorf("status %d output %q", status, out.String())
	}
	// A write that fails changes nothing: there is nowhere left to report it.
	if status := cli.Announcef(failingWriter{}, "x"); status != cli.ExitOK {
		t.Errorf("status %d after a failed write", status)
	}
	cli.Sayf(failingWriter{}, "x")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }
