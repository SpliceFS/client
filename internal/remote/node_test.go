package remote

import (
	"net/http"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		url     string
		client  *http.Client
		wantErr bool
		wantURL string
	}{
		{name: "https with path", url: "https://east.example.com/api", client: http.DefaultClient, wantURL: "https://east.example.com/api"},
		{name: "http with port", url: "http://192.0.2.1:8080/api", client: http.DefaultClient, wantURL: "http://192.0.2.1:8080/api"},
		{name: "trailing slash trimmed", url: "https://east.example.com/api/", client: http.DefaultClient, wantURL: "https://east.example.com/api"},
		{name: "no path", url: "https://east.example.com", client: http.DefaultClient, wantURL: "https://east.example.com"},
		{name: "unparsable", url: "http://[::1", client: http.DefaultClient, wantErr: true},
		{name: "relative", url: "/api", client: http.DefaultClient, wantErr: true},
		{name: "wrong scheme", url: "ftp://east.example.com/api", client: http.DefaultClient, wantErr: true},
		{name: "no host", url: "https:///api", client: http.DefaultClient, wantErr: true},
		{name: "query", url: "https://east.example.com/api?x=1", client: http.DefaultClient, wantErr: true},
		{name: "fragment", url: "https://east.example.com/api#x", client: http.DefaultClient, wantErr: true},
		{name: "nil client", url: "https://east.example.com/api", client: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, err := New("east", tt.url, tt.client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if n.base != tt.wantURL {
				t.Errorf("base = %q, want %q", n.base, tt.wantURL)
			}
			if n.Name() != "east" {
				t.Errorf("Name() = %q, want %q", n.Name(), "east")
			}
		})
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
		{name: "special characters", in: "my share/a#b/100%/caf\u00e9?.txt", want: "my%20share/a%23b/100%25/caf%C3%A9%3F.txt"},
		{name: "empty", in: "", wantErr: true},
		{name: "leading slash", in: "/share", wantErr: true},
		{name: "trailing slash", in: "share/", wantErr: true},
		{name: "double slash", in: "share//a", wantErr: true},
		{name: "dot", in: "share/./a", wantErr: true},
		{name: "dot dot", in: "share/../a", wantErr: true},
		{name: "only dot dot", in: "..", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := escapePath(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("escapePath(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("escapePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
