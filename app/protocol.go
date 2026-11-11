package app

// The control socket speaks one JSON request and one JSON reply per connection.
//
//	{"op":"status"}                                  -> {"report":{...}}
//	{"op":"unmount","targets":["/mnt/a"],"force":false} -> {"results":[{"target":"/mnt/a","result":"unmounted"}]}
//	any request that cannot be served                -> {"error":"..."}

// Operations a Request can ask for.
const (
	// OpStatus asks for a Report.
	OpStatus = "status"
	// OpUnmount asks for mounts to be unmounted.
	OpUnmount = "unmount"
)

// Outcomes of unmounting one target.
const (
	// ResultUnmounted means the mount is gone.
	ResultUnmounted = "unmounted"
	// ResultBusy means a program is using the mount and force was not set; the mount
	// is still in place.
	ResultBusy = "busy"
	// ResultFailed means the unmount failed for another reason, given in the error.
	ResultFailed = "failed"
	// ResultUnknown means the client has no mount at that target.
	ResultUnknown = "unknown"
)

// Mount states in a Report.
const (
	// StateMounted means the mount is in place.
	StateMounted = "mounted"
	// StateEnded means the mount is gone, whether unmounted by the client or from
	// outside it.
	StateEnded = "ended"
)

// Request is what a control client sends.
type Request struct {
	// Op is OpStatus or OpUnmount.
	Op string `json:"op"`
	// Targets are the absolute mount targets to unmount. Empty means every mount.
	Targets []string `json:"targets,omitempty"`
	// Force unmounts a mount that is in use.
	Force bool `json:"force,omitempty"`
}

// Reply is what the client answers. Exactly one of its fields is set.
type Reply struct {
	// Error is why the request could not be served.
	Error string `json:"error,omitempty"`
	// Report answers OpStatus.
	Report *Report `json:"report,omitempty"`
	// Results answers OpUnmount, one entry per target in request order.
	Results []UnmountResult `json:"results,omitempty"`
}

// UnmountResult is the outcome for one target of an unmount request.
type UnmountResult struct {
	// Target is the mount target as requested, cleaned.
	Target string `json:"target"`
	// Result is one of the Result constants.
	Result string `json:"result"`
	// Error describes the failure when Result is ResultFailed.
	Error string `json:"error,omitempty"`
}

// Report is the state of a running client. It never contains passwords or tokens.
type Report struct {
	// Mounts lists the mounts in configuration order.
	Mounts []MountReport `json:"mounts"`
	// Clusters lists every configured cluster with the nodes it has found.
	Clusters []ClusterReport `json:"clusters"`
	// Caches lists every cache directory in use.
	Caches []CacheReport `json:"caches"`
}

// MountReport describes one mount.
type MountReport struct {
	// Target is the local directory.
	Target string `json:"target"`
	// Source is the name shown in the mount table, for example "splicefs:example/media".
	Source string `json:"source"`
	// State is StateMounted or StateEnded.
	State string `json:"state"`
}

// ClusterReport describes one cluster.
type ClusterReport struct {
	// Name is the cluster's name in the configuration.
	Name string `json:"name"`
	// Nodes is empty until the client has connected to the cluster.
	Nodes []NodeReport `json:"nodes"`
}

// NodeReport describes one node of a cluster.
type NodeReport struct {
	// Name identifies the node.
	Name string `json:"name"`
	// BytesPerSecond is the measured read throughput, 0 while Measured is false.
	BytesPerSecond float64 `json:"bytes_per_second"`
	// Measured is false until a block has been read from the node.
	Measured bool `json:"measured"`
}

// CacheReport describes one cache directory.
type CacheReport struct {
	// Dir is the cache directory.
	Dir string `json:"dir"`
	// Entries is the number of files with cached blocks.
	Entries int `json:"entries"`
	// Bytes is the size of the cached blocks.
	Bytes int64 `json:"bytes"`
}
