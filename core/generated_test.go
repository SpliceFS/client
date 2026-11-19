package core

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"path"
	"slices"
	"time"
)

// generatedPlan is a seeded mix of paths: each path has one to three versions that differ
// in size and time, and each node holds one of them or nothing.
type generatedPlan struct {
	holders map[string]map[int]fakeFile // by path, then by node
}

const generatedNodes = 3

func generatePlan(seed uint64, paths int) generatedPlan {
	random := rand.New(rand.NewPCG(seed, seed))
	plan := generatedPlan{holders: map[string]map[int]fakeFile{}}
	for i := 1; i <= paths; i++ {
		name := generatedPath(random, i)
		versions := generatedVersions(random, i)
		holds := map[int]fakeFile{}
		for node := range generatedNodes {
			if random.Float64() < 0.7 {
				holds[node] = versions[random.IntN(len(versions))]
			}
		}
		if len(holds) == 0 {
			holds[random.IntN(generatedNodes)] = versions[0]
		}
		plan.holders[name] = holds
	}
	return plan
}

func generatedPath(random *rand.Rand, i int) string {
	levels := [][]string{{"alpha", "beta", "gamma", "delta"}, {"one", "two", "three"}, {"x", "y"}}
	dir := ""
	for _, level := range levels[:random.IntN(len(levels)+1)] {
		dir = path.Join(dir, level[random.IntN(len(level))])
	}
	exts := []string{"txt", "bin", "dat", "log"}
	return path.Join(dir, fmt.Sprintf("f%02d.%s", i, exts[random.IntN(len(exts))]))
}

// generatedVersions makes one to three versions with distinct sizes and distinct times.
func generatedVersions(random *rand.Rand, i int) []fakeFile {
	count := 1 + random.IntN(3)
	sizes := random.Perm(1 << 16)[:count]
	days := random.Perm(300)[:count]
	versions := make([]fakeFile, count)
	for v := range count {
		mtime := fileTime().Add(time.Duration(1+days[v]) * 24 * time.Hour)
		versions[v] = copyOf(byte(i*3+v), sizes[v], mtime)
	}
	return versions
}

func (p generatedPlan) placed() map[int]map[string]fakeFile {
	placed := map[int]map[string]fakeFile{}
	for name, holds := range p.holders {
		for node, stored := range holds {
			if placed[node] == nil {
				placed[node] = map[string]fakeFile{}
			}
			placed[node][name] = stored
		}
	}
	return placed
}

// want is the version of each path that the running nodes vote for.
func (p generatedPlan) want(running ...int) map[string][]fakeFile {
	want := map[string][]fakeFile{}
	for name, holds := range p.holders {
		if best, ok := vote(holds, running); ok {
			want[name] = one(best)
		}
	}
	return want
}

func vote(holds map[int]fakeFile, running []int) (fakeFile, bool) {
	votes := map[string]int{}
	byTag := map[string]fakeFile{}
	for _, node := range running {
		if stored, ok := holds[node]; ok {
			votes[stored.etag()]++
			byTag[stored.etag()] = stored
		}
	}
	var best fakeFile
	bestVotes := 0
	for _, tag := range slices.Sorted(maps.Keys(votes)) {
		stored := byTag[tag]
		if votes[tag] > bestVotes || (votes[tag] == bestVotes && stored.mtime.After(best.mtime)) {
			best, bestVotes = stored, votes[tag]
		}
	}
	return best, bestVotes > 0
}

// dirs is every folder above a path that the running nodes hold.
func (p generatedPlan) dirs(running ...int) []string {
	seen := map[string]bool{}
	for name := range p.want(running...) {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
