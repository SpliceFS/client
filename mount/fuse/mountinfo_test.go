package fuse

import "testing"

func TestFuseConnectionID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		table  string
		target string
		want   uint32
		found  bool
	}{
		{"empty content", "", "/mnt/data", 0, false},
		{"found", "36 25 0:44 / /mnt/data rw - fuse.splicefs splicefs rw\n", "/mnt/data", 44, true},
		{"plain fuse type", "36 25 0:44 / /mnt/data rw - fuse x rw\n", "/mnt/data", 44, true},
		{"block fuse type", "36 25 0:44 / /mnt/data rw - fuseblk x rw\n", "/mnt/data", 44, true},
		{"not found", "36 25 0:44 / /mnt/data rw - fuse x rw\n", "/mnt/other", 0, false},
		{
			"a longer path is another mount",
			"36 25 0:44 / /mnt/data2 rw - fuse x rw\n",
			"/mnt/data", 0, false,
		},
		{"target is cleaned", "36 25 0:44 / /mnt/data rw - fuse x rw\n", "/mnt//data/", 44, true},
		{
			"shadowed by a later mount",
			"36 25 0:44 / /mnt/data rw - fuse x rw\n37 25 0:45 / /mnt/data rw - fuse x rw\n",
			"/mnt/data", 45, true,
		},
		{
			"a later mount that is not FUSE does not count",
			"36 25 0:44 / /mnt/data rw - fuse x rw\n37 25 8:1 / /mnt/data rw - ext4 x rw\n",
			"/mnt/data", 44, true,
		},
		{
			"only a mount that is not FUSE",
			"37 25 8:1 / /mnt/data rw - ext4 x rw\n",
			"/mnt/data", 0, false,
		},
		{
			"escaped space",
			"36 25 0:44 / /mnt/my\\040data rw - fuse x rw\n",
			"/mnt/my data", 44, true,
		},
		{
			"escaped tab, newline and backslash",
			"36 25 0:44 / /mnt/a\\011b\\012c\\134d rw - fuse x rw\n",
			"/mnt/a\tb\nc\\d", 44, true,
		},
		{
			"optional fields are skipped",
			"36 25 0:44 / /mnt/data rw shared:1 master:2 - fuse x rw\n",
			"/mnt/data", 44, true,
		},
		{
			"a mount point named like the separator",
			"36 25 0:44 / - rw - fuse x rw\n",
			"-", 44, true,
		},
		{
			"malformed lines are ignored",
			"short line\n" +
				"36 25 0:44 / /mnt/data rw\n" + // no separator
				"36 25 0:44 / /mnt/data rw - \n" + // no type
				"36 25 044 / /mnt/data rw - fuse x rw\n" + // no colon in the device
				"36 25 0:x / /mnt/data rw - fuse x rw\n" + // minor is not a number
				"36 25 0:99999999999 / /mnt/data rw - fuse x rw\n" + // minor out of range
				"36 25 0:46 / /mnt/data rw - fuse x rw\n",
			"/mnt/data", 46, true,
		},
		{"no final newline", "36 25 0:44 / /mnt/data rw - fuse x rw", "/mnt/data", 44, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, found := fuseConnectionID(tt.table, tt.target)
			if id != tt.want || found != tt.found {
				t.Fatalf("fuseConnectionID = %d, %v; want %d, %v", id, found, tt.want, tt.found)
			}
		})
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"/plain", "/plain"},
		{`/a\040b`, "/a b"},
		{`\134`, `\`},
		{`/a\04`, `/a\04`},     // too short to be an escape
		{`/a\9zzb`, `/a\9zzb`}, // not octal
		{`/a\777b`, `/a\777b`}, // does not fit a byte
		{`/a\`, `/a\`},
		{"", ""},
	}
	for _, tt := range tests {
		if got := unescapeMountinfo(tt.in); got != tt.want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
