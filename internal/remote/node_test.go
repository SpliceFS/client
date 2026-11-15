package remote

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func TestNewKeepsBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		// wantBase is the base URL the node must keep, url itself when empty.
		wantBase string
	}{
		{name: "https with path", url: "https://east.example.com/api"},
		{name: "http with port", url: "http://192.0.2.1:8080/api"},
		{name: "trailing slash trimmed", url: "https://east.example.com/api/", wantBase: "https://east.example.com/api"},
		{name: "no path", url: "https://east.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, err := New("east", tt.url, http.DefaultClient)
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			wantBase := cmp.Or(tt.wantBase, tt.url)
			if n.base != wantBase {
				t.Errorf("base = %q, want %q", n.base, wantBase)
			}
			if n.Name() != "east" {
				t.Errorf("Name() = %q, want %q", n.Name(), "east")
			}
		})
	}
}

func TestNewRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		// noClient passes a nil client instead of the default one.
		noClient bool
		wantErr  error
	}{
		{name: "relative", url: "/api", wantErr: errNotAbsoluteURL},
		{name: "wrong scheme", url: "ftp://east.example.com/api", wantErr: errNotAbsoluteURL},
		{name: "no host", url: "https:///api", wantErr: errNotAbsoluteURL},
		{name: "query", url: "https://east.example.com/api?x=1", wantErr: errQueryOrFragURL},
		{name: "fragment", url: "https://east.example.com/api#x", wantErr: errQueryOrFragURL},
		{name: "nil client", url: "https://east.example.com/api", noClient: true, wantErr: ErrNilClient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := http.DefaultClient
			if tt.noClient {
				client = nil
			}
			n, err := New("east", tt.url, client)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
			}
			if n != nil {
				t.Errorf("New() node = %v, want nil", n)
			}
		})
	}
}

func TestNewUnparsableURL(t *testing.T) {
	t.Parallel()
	_, err := New("east", "http://[::1", http.DefaultClient)
	if _, ok := errors.AsType[*url.Error](err); !ok {
		t.Fatalf("New() error = %v, want a *url.Error", err)
	}
}

func TestEscapePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain", in: "share/a/b", want: "share/a/b"},
		{
			name: "special characters",
			in:   "my share/a#b/100%/caf\u00e9?.txt",
			want: "my%20share/a%23b/100%25/caf%C3%A9%3F.txt",
		},
		{name: "empty", in: "", wantErr: true},
		{name: "leading slash", in: "/share", wantErr: true},
		{name: "trailing slash", in: "share/", wantErr: true},
		{name: "double slash", in: "share//a", wantErr: true},
		{name: "current directory segment", in: "share/./a", wantErr: true},
		{name: "parent directory segment", in: "share/../a", wantErr: true},
		{name: "only parent directory segment", in: "..", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := escapePath(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("escapePath(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidPath) {
				t.Errorf("escapePath(%q) error = %v, want ErrInvalidPath", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("escapePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
