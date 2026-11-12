package core

import (
	"maps"
	"slices"
)

// Status is a snapshot of the client for display, as "splicefs status" prints it.
type Status struct {
	// Clusters is every current cluster, sorted by name.
	Clusters []ClusterStatus
	// Caches is every current cache directory, sorted by directory.
	Caches []CacheStatus
}

// ClusterStatus describes one cluster.
type ClusterStatus struct {
	// Name is the cluster's name in the configuration.
	Name string
	// Nodes is the cluster's nodes in node-list order. It is empty until the client
	// has connected to the cluster.
	Nodes []NodeStatus
}

// NodeStatus describes one node of a cluster.
type NodeStatus struct {
	// Name is the node's name in the node list.
	Name string
	// BytesPerSecond is the measured read throughput of the node.
	BytesPerSecond float64
	// Measured is false while no block has been read from the node, and then
	// BytesPerSecond is 0.
	Measured bool
}

// CacheStatus describes one cache directory.
type CacheStatus struct {
	// Dir is the cache directory.
	Dir string
	// Entries is the number of files with cached blocks.
	Entries int
	// Bytes is the size of the cached blocks.
	Bytes int64
}

// Status reports the current clusters with their known nodes and measured speeds, and the
// use of every current cache. Clusters and caches that a reload replaced are left out. It
// does not use the network.
func (c *Client) Status() Status {
	c.mu.Lock()
	clusters, caches := c.clusters, c.caches
	c.mu.Unlock()
	var s Status
	for _, name := range slices.Sorted(maps.Keys(clusters)) {
		cl := clusters[name]
		cs := ClusterStatus{Name: name}
		for _, node := range cl.knownNodes() {
			rate, known := cl.meter.Rate(node)
			cs.Nodes = append(cs.Nodes, NodeStatus{Name: node, BytesPerSecond: rate, Measured: known})
		}
		s.Clusters = append(s.Clusters, cs)
	}
	for _, dir := range slices.Sorted(maps.Keys(caches)) {
		u := caches[dir].Usage()
		s.Caches = append(s.Caches, CacheStatus{Dir: dir, Entries: u.Entries, Bytes: u.Bytes})
	}
	return s
}
