// Package wire defines the JSON bodies and header names of the SpliceFS HTTP API,
// version 1. The server and the client both use these types, so a change here is a
// change to the protocol.
package wire

import "time"

// HeaderHeadDigest carries the head digest of a file on HEAD responses: the
// hex-encoded SHA-256 of its first HeadDigestBytes bytes.
const HeaderHeadDigest = "X-SpliceFS-Head-Digest"

// HeadDigestBytes is how much of the start of a file the head digest covers.
const HeadDigestBytes = 64 << 10

// Health is the body of GET /v1/health.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Node    string `json:"node,omitempty"`
}

// LoginRequest is the body of POST /v1/login.
type LoginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// LoginResponse is the answer to a successful login.
type LoginResponse struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Node is one member of a cluster.
type Node struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
}

// Nodes is the body of GET /v1/nodes and the content of a node list file.
type Nodes struct {
	Cluster string `json:"cluster" yaml:"cluster"`
	Nodes   []Node `json:"nodes" yaml:"nodes"`
}

// Entry is one name in a directory listing. ETag is empty for directories.
type Entry struct {
	Name  string    `json:"name"`
	Dir   bool      `json:"dir"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	ETag  string    `json:"etag,omitempty"`
}

// Listing is the body of GET /v1/tree/...
type Listing struct {
	Entries []Entry `json:"entries"`
}

// Space is the body of GET /v1/space/<share>, in bytes.
type Space struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// Error is the body of every error response.
type Error struct {
	Error string `json:"error"`
}
