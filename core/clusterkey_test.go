package core

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/version"
	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/common/apitest"
	"github.com/SpliceFS/common/remote"
	"github.com/SpliceFS/common/wire"
)

// The names of the nodes of these tests are not in their certificates on purpose: a node is
// trusted for the cluster key that signed its certificate, not for its name.
const (
	certificateName = "node.example.test"
	sharedFile      = "a.txt"
)

func newTestClusterKey(t *testing.T) apitest.ClusterKey {
	t.Helper()
	key, err := apitest.NewClusterKey()
	if err != nil {
		t.Fatalf("NewClusterKey: %v", err)
	}
	return key
}

// keyText is the cluster_key setting for key.
func keyText(key apitest.ClusterKey) string {
	return base64.StdEncoding.EncodeToString(key.Public())
}

// startClusterNode starts a node that serves the share media with a certificate that key
// signed, and returns the URL of the node with host in place of the loopback address.
func startClusterNode(t *testing.T, clock *fakeClock, key apitest.ClusterKey, host string) string {
	t.Helper()
	cert, err := key.IssueNode([]string{certificateName}, clock.Now().Add(-time.Hour), clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	media := apitest.NewShare(fileTime())
	if err := media.Replace(sharedFile, []byte("content"), fileTime()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	auth := apitest.NewAuth(clock.Now, time.Hour)
	auth.AddUser(testUser, testPassword, "*")
	nodes := &apitest.NodeList{}
	node := apitest.StartWithCertificate(t, api.Options{
		BasePath: "/api",
		Node:     "east",
		Version:  version.String(),
		Shares:   map[string]api.Share{"media": media},
		Auth:     auth,
		Nodes:    nodes,
	}, cert)
	url := strings.Replace(node.URL(), "127.0.0.1", host, 1)
	nodes.Set(wire.Nodes{Cluster: testCluster, Nodes: []wire.Node{{Name: "east", URL: url}}})
	return url
}

// keyedEnv is a client configuration of clusters with cluster keys. Every cluster has one
// mount, /mnt/<name>, and the client builds its own HTTP client for each, as it does when
// it runs.
type keyedEnv struct {
	t     *testing.T
	clock *fakeClock
	cfg   *config.Config
}

func newKeyedEnv(t *testing.T) *keyedEnv {
	t.Helper()
	return &keyedEnv{
		t:     t,
		clock: newFakeClock(),
		cfg:   &config.Config{Cache: testCache(t.TempDir()), Clusters: map[string]config.Cluster{}},
	}
}

// addCluster adds the cluster name at url with the given cluster_key text, which may be empty.
func (e *keyedEnv) addCluster(name, url, clusterKey string) {
	e.cfg.Clusters[name] = config.Cluster{
		URL: url, User: testUser, PasswordFile: "password", BlockSize: testBlockSize,
		StreamsPerNode: 2, ReadAhead: 4 * testBlockSize, ClusterKey: clusterKey,
	}
	e.cfg.Mounts = append(e.cfg.Mounts, config.Mount{
		Cluster: name, Path: "/", Target: "/mnt/" + name, Offline: true, Cache: e.cfg.Cache,
	})
}

func (e *keyedEnv) client() *Client {
	e.t.Helper()
	client, err := newClient(e.cfg, Options{
		Now:          e.clock.Now,
		ReadPassword: func(string) (string, error) { return testPassword, nil },
		openCache:    cache.OpenUnsynced,
	}, e.clock)
	if err != nil {
		e.t.Fatalf("New: %v", err)
	}
	closeAfter(e.t, client)
	return client
}

// listMedia lists the share media of cluster name through its mount.
func (e *keyedEnv) listMedia(client *Client, name string) ([]string, error) {
	e.t.Helper()
	view, err := client.View(e.cfg.Mounts[slices.IndexFunc(e.cfg.Mounts, func(m config.Mount) bool {
		return m.Cluster == name
	})])
	if err != nil {
		e.t.Fatalf("View: %v", err)
	}
	attrs, err := view.ReadDir(context.Background(), "media")
	names := make([]string, len(attrs))
	for i, attr := range attrs {
		names[i] = attr.Name
	}
	return names, err
}

func TestClusterKeyTrustsAClusterSignedNode(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"localhost", "127.0.0.1"} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			env := newKeyedEnv(t)
			key := newTestClusterKey(t)
			env.addCluster(testCluster, startClusterNode(t, env.clock, key, host), keyText(key))
			names, err := env.listMedia(env.client(), testCluster)
			if err != nil || !slices.Equal(names, []string{sharedFile}) {
				t.Errorf("listing = %v, %v; want [%s]", names, err, sharedFile)
			}
		})
	}
}

func TestAnotherClusterKeyRefusesTheNode(t *testing.T) {
	t.Parallel()
	env := newKeyedEnv(t)
	key := newTestClusterKey(t)
	env.addCluster(testCluster, startClusterNode(t, env.clock, key, "127.0.0.1"), keyText(newTestClusterKey(t)))
	err := env.client().Connect(context.Background(), testCluster)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Connect with another key = %v, want ErrUnavailable", err)
	}
	for _, want := range []string{`cluster "` + testCluster + `"`, "not signed by the cluster key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q lacks %q", err, want)
		}
	}
}

func TestNoClusterKeyRefusesAClusterSignedNode(t *testing.T) {
	t.Parallel()
	env := newKeyedEnv(t)
	env.addCluster(testCluster, startClusterNode(t, env.clock, newTestClusterKey(t), "127.0.0.1"), "")
	err := env.client().Connect(context.Background(), testCluster)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), `cluster "`+testCluster+`"`) {
		t.Errorf("Connect without a key = %v, want ErrUnavailable naming the cluster", err)
	}
}

func TestEachClusterTrustsOnlyItsOwnKey(t *testing.T) {
	t.Parallel()
	env := newKeyedEnv(t)
	eastKey, westKey := newTestClusterKey(t), newTestClusterKey(t)
	eastURL := startClusterNode(t, env.clock, eastKey, "127.0.0.1")
	westURL := startClusterNode(t, env.clock, westKey, "127.0.0.1")
	env.addCluster("east", eastURL, keyText(eastKey))
	env.addCluster("west", westURL, keyText(westKey))
	env.addCluster("east-with-west-key", eastURL, keyText(westKey))
	env.addCluster("west-with-east-key", westURL, keyText(eastKey))
	client := env.client()

	for name, trusted := range map[string]bool{
		"east": true, "west": true, "east-with-west-key": false, "west-with-east-key": false,
	} {
		names, err := env.listMedia(client, name)
		if trusted && (err != nil || !slices.Equal(names, []string{sharedFile})) {
			t.Errorf("cluster %s: listing = %v, %v; want [%s]", name, names, err, sharedFile)
		}
		if !trusted && !errors.Is(err, ErrUnavailable) {
			t.Errorf("cluster %s: listing = %v, %v; want ErrUnavailable", name, names, err)
		}
	}
}

func TestABadClusterKeyStopsTheClientFromStarting(t *testing.T) {
	t.Parallel()
	env := newKeyedEnv(t)
	env.addCluster(testCluster, "https://files.example.com/api", "AAAA")
	_, err := newClient(env.cfg, Options{
		ReadPassword: func(string) (string, error) { return testPassword, nil },
		openCache:    cache.OpenUnsynced,
	}, env.clock)
	if !errors.Is(err, remote.ErrInvalidClusterKey) ||
		!strings.Contains(err.Error(), `cluster "`+testCluster+`": cluster_key`) {
		t.Errorf("New with a bad cluster key = %v, want an error naming the cluster and the setting", err)
	}
}

func TestAnotherClusterKeyMakesAnotherCluster(t *testing.T) {
	t.Parallel()
	env := newKeyedEnv(t)
	first := newTestClusterKey(t)
	env.addCluster(testCluster, "https://files.example.com/api", keyText(first))
	clus := env.client().clusters[testCluster]
	changed := clus.cfg
	changed.ClusterKey = keyText(newTestClusterKey(t))
	if !clus.matches(clus.cfg, testPassword, clus.ttl) || clus.matches(changed, testPassword, clus.ttl) {
		t.Error("a cluster with another cluster_key matches the old one, so a reload would keep its trust")
	}
}
