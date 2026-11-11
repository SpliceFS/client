package version

import "testing"

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

func TestDefaultIsDev(t *testing.T) {
	if version != "dev" {
		t.Errorf("version = %q in a test build, want %q", version, "dev")
	}
}
