package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
)

// writeFiles creates the files, given by their path under dir, with their folders.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a test directory
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// realConfigBackend loads the configuration from the files, as the program does, and
// fails the test when anything else is asked of it.
func realConfigBackend() backend {
	b, _ := fakeBackend(nil, nil, nil, nil, nil, app.Report{})
	b.loadConfig = config.LoadDir
	return b
}

const sampleCache = "cache:\n  dir: /var/cache/example\n"

func TestConfigCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"conf.d/cache.yaml":     sampleCache,
		"clusters.d/east.yaml":  "url: https://east.example.com/api\nuser: alice\npassword_file: pw\n",
		"clusters.d/west.yaml":  "url: https://west.example.com/api\nuser: alice\npassword_file: pw\n",
		"mounts.d/media.yaml":   "cluster: east\npath: /media\ntarget: /srv/media\n",
		"mounts.d/ignored.txt":  "not yaml",
		"mounts.d/.hidden.yaml": "bad: [",
		"clusters.d/sub/x.yaml": "bad: [",
		"mounts.d/archive.yaml": "cluster: west\npath: /\ntarget: /srv/archive\n",
		"conf.d/ignored/a.yaml": "bad: [",
	})
	empty := t.TempDir()
	writeFiles(t, empty, map[string]string{"conf.d/cache.yaml": sampleCache})
	tests := []struct {
		name string
		dir  string
		want string
	}{
		{"clusters and mounts", dir, "ok: 2 clusters, 2 mounts\n"},
		{"nothing configured", empty, "ok: 0 clusters, 0 mounts\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, stdout, stderr := executeWith(realConfigBackend(), "--config", tt.dir, "config", "check")
			if status != exitOK || stdout != tt.want || stderr != "" {
				t.Errorf("status %d stdout %q stderr %q, want 0 %q", status, stdout, stderr, tt.want)
			}
		})
	}
}

func TestConfigCheckPrintsEveryProblem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"conf.d/cache.yaml":    "cache:\n  dir: /var/cache/example\n  colour: red\n",
		"clusters.d/east.yaml": "url: ftp://east.example.com\nuser: alice\npassword_file: pw\n",
		// A file where a folder belongs: the loader cannot read the mounts folder.
		"mounts.d": "not a folder",
	})
	status, stdout, stderr := executeWith(realConfigBackend(), "--config", dir, "config", "check")
	if status != exitError || stdout != "" {
		t.Errorf("status %d stdout %q", status, stdout)
	}
	for _, want := range []string{"conf.d/cache.yaml", "colour", "mounts.d"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
}

func TestConfigShow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"conf.d/10-cache.yaml": "cache:\n  dir: /var/cache/example\n  max_size: 10GiB\n",
		"conf.d/20-cache.yaml": "cache:\n  max_size: 20GiB\n",
		"clusters.d/east.yaml": "url: https://east.example.com/api\nuser: alice\n" +
			"password_file: /etc/pw\n",
		"mounts.d/media.yaml": "cluster: east\npath: /media\ntarget: /srv/media\n" +
			"cache:\n  max_age: 7d\n",
		"mounts.d/docs.yaml": "cluster: east\npath: /docs\ntarget: /srv/docs\noffline: true\n",
	})
	status, stdout, stderr := executeWith(realConfigBackend(), "--config", dir, "config", "show")
	if status != exitOK || stderr != "" {
		t.Fatalf("status %d stderr %q", status, stderr)
	}
	want := `cache:
  dir: /var/cache/example # conf.d/10-cache.yaml
  max_size: 20GiB # conf.d/20-cache.yaml
  max_age: unlimited
  min_free: 1GiB
  min_file_size: "0"
  revalidate: 1h
  listing_ttl: 30s
clusters:
  east:
    url: https://east.example.com/api # clusters.d/east.yaml
    user: alice # clusters.d/east.yaml
    password_file: /etc/pw # clusters.d/east.yaml
    block_size: 5MiB
    streams_per_node: 4
    read_ahead: 64MiB
mounts:
  - name: docs
    cluster: east # mounts.d/docs.yaml
    path: /docs # mounts.d/docs.yaml
    target: /srv/docs # mounts.d/docs.yaml
    allow_other: false
    offline: true # mounts.d/docs.yaml
    cache:
      dir: /var/cache/example
      max_size: 20GiB
      max_age: unlimited
      min_free: 1GiB
      min_file_size: "0"
      revalidate: 1h
      listing_ttl: 30s
  - name: media
    cluster: east # mounts.d/media.yaml
    path: /media # mounts.d/media.yaml
    target: /srv/media # mounts.d/media.yaml
    allow_other: false
    offline: true
    cache:
      dir: /var/cache/example
      max_size: 20GiB
      max_age: 7d # mounts.d/media.yaml
      min_free: 1GiB
      min_file_size: "0"
      revalidate: 1h
      listing_ttl: 30s
`
	if stdout != want {
		t.Errorf("output:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestConfigShowWithoutClustersAndMounts(t *testing.T) {
	t.Parallel()
	b, _ := fakeBackend(&config.Config{}, nil, nil, nil, nil, app.Report{})
	status, stdout, stderr := executeWith(b, "config", "show")
	if !strings.HasSuffix(stdout, "clusters: {}\nmounts: []\n") || status != exitOK || stderr != "" {
		t.Errorf("status %d stdout %q stderr %q", status, stdout, stderr)
	}
}

func TestConfigFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("invalid client config /x: no way")
	for _, command := range []string{"check", "show"} {
		t.Run("load "+command, func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(nil, boom, nil, nil, nil, app.Report{})
			status, stdout, stderr := executeWith(b, "config", command)
			if status != exitError || stdout != "" || stderr != "splicefs: "+boom.Error()+"\n" {
				t.Errorf("status %d stdout %q stderr %q", status, stdout, stderr)
			}
		})
		t.Run("write "+command, func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(&config.Config{}, nil, nil, nil, nil, app.Report{})
			var errOut bytes.Buffer
			status := execute(context.Background(), []string{"config", command}, failingWriter{}, &errOut, b)
			if status != exitError || !strings.Contains(errOut.String(), "disk full") {
				t.Errorf("status %d stderr %q", status, errOut.String())
			}
		})
	}
}

func TestUnknownDefaultConfigDir(t *testing.T) {
	t.Parallel()
	boom := errors.New("cannot find the home directory")
	tests := []struct {
		name string
		args []string
		// looksUp says whether the command needs the default configuration directory.
		looksUp bool
	}{
		{"mount", []string{"mount"}, true},
		{"umount", []string{"umount"}, true},
		{"status", []string{"status"}, true},
		{"config check", []string{"config", "check"}, true},
		{"config show", []string{"config", "show"}, true},
		{"directory given", []string{"--config", "/etc/example", "status"}, false},
		{"version", []string{"--version"}, false},
		{"help", []string{"--help"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, c := fakeBackend(&config.Config{}, nil, nil, nil, nil, app.Report{})
			b.defaultConfigDir = func() (string, error) { return "", boom }
			status, stdout, stderr := executeWith(b, tt.args...)
			if !tt.looksUp {
				if status != exitOK {
					t.Errorf("status %d stdout %q stderr %q, want a command that ran", status, stdout, stderr)
				}
				return
			}
			if status != exitError || stdout != "" || stderr != "splicefs: "+boom.Error()+"\n" {
				t.Errorf("status %d stdout %q stderr %q", status, stdout, stderr)
			}
			if c.loadDir != "" || c.runCfg != nil || c.socketPath != "" || c.statusCalls != 0 {
				t.Errorf("a command ran without a configuration directory: %+v", c)
			}
		})
	}
}
