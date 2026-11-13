package version

import "testing"

// TestString replaces the package variable that the linker sets, so it cannot run in
// parallel with other tests that read it.
//
//nolint:paralleltest // Replaces the package variable version.
func TestString(t *testing.T) {
	tests := []struct {
		name string
		set  string
		want string
	}{
		{name: "default build", set: "dev", want: "dev"},
		{name: "release build", set: "v1.2.3", want: "v1.2.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := version
			t.Cleanup(func() { version = old })
			version = tt.set
			if got := String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDefaultIsDev only reads the variable. A parallel test resumes after every sequential
// test has finished and restored it, so it sees the value the build gave it.
func TestDefaultIsDev(t *testing.T) {
	t.Parallel()
	if version != "dev" {
		t.Errorf("version = %q in a test build, want %q", version, "dev")
	}
}
