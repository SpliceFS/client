package nfs

import "testing"

func TestLRU(t *testing.T) {
	t.Parallel()
	c := newLRU[string, int](2)
	c.add("a", 1)
	c.add("b", 2)
	if v, ok := c.get("a"); !ok || v != 1 { // a becomes the most recent
		t.Fatalf("get a = %d, %v", v, ok)
	}
	c.add("c", 3) // evicts b, the least recently used
	if _, ok := c.get("b"); ok {
		t.Error("b should have been evicted")
	}
	for key, want := range map[string]int{"a": 1, "c": 3} {
		if v, ok := c.get(key); !ok || v != want {
			t.Errorf("get %s = %d, %v; want %d", key, v, ok, want)
		}
	}
}

func TestLRUAddReplacesAndRefreshes(t *testing.T) {
	t.Parallel()
	c := newLRU[string, int](2)
	c.add("a", 1)
	c.add("b", 2)
	c.add("a", 10) // replaces the value and makes a the most recent
	c.add("c", 3)  // evicts b
	if v, _ := c.get("a"); v != 10 {
		t.Errorf("a = %d, want 10", v)
	}
	if _, ok := c.get("b"); ok {
		t.Error("b should have been evicted")
	}
}

func TestLRURemove(t *testing.T) {
	t.Parallel()
	c := newLRU[string, int](2)
	c.add("a", 1)
	c.remove("a")
	c.remove("missing")
	if _, ok := c.get("a"); ok {
		t.Error("a should be gone")
	}
}
