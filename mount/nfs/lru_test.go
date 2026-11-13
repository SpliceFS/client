package nfs

import "testing"

func TestLRU(t *testing.T) {
	t.Parallel()
	cache := newLRU[string, int](2)
	cache.add("a", 1)
	cache.add("b", 2)
	if v, ok := cache.get("a"); !ok || v != 1 { // a becomes the most recent
		t.Fatalf("get a = %d, %v", v, ok)
	}
	cache.add("c", 3) // evicts b, the least recently used
	if _, ok := cache.get("b"); ok {
		t.Error("b should have been evicted")
	}
	for key, want := range map[string]int{"a": 1, "c": 3} {
		if v, ok := cache.get(key); !ok || v != want {
			t.Errorf("get %s = %d, %v; want %d", key, v, ok, want)
		}
	}
}

func TestLRUAddReplacesAndRefreshes(t *testing.T) {
	t.Parallel()
	cache := newLRU[string, int](2)
	cache.add("a", 1)
	cache.add("b", 2)
	cache.add("a", 10) // replaces the value and makes a the most recent
	cache.add("c", 3)  // evicts b
	if v, _ := cache.get("a"); v != 10 {
		t.Errorf("a = %d, want 10", v)
	}
	if _, ok := cache.get("b"); ok {
		t.Error("b should have been evicted")
	}
}

func TestLRURemove(t *testing.T) {
	t.Parallel()
	cache := newLRU[string, int](2)
	cache.add("a", 1)
	cache.remove("a")
	cache.remove("missing")
	if _, ok := cache.get("a"); ok {
		t.Error("a should be gone")
	}
}
