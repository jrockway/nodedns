// Package k8s communicates with Kubernetes to watch nodes.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	nodeChangeEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_change_events",
			Help: "A counter of node change events, by event type and the store they affected.",
		},
		[]string{"store", "event"},
	)
	nodeCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_count",
			Help: "The number of nodes that we are currently tracking.",
		},
		[]string{"store"},
	)
)

// Record is a DNS record that contains the full set of nodes.
type Record struct {
	IsInternal bool // Whether this record contains internal IPs or external IPs.
	IPs        []net.IP
}

// UpdateRequest is a request to change a DNS address.
type UpdateRequest struct {
	Ctx    context.Context
	Record Record
}

// Node contains Address information about Kubernetes nodes.
type Node struct {
	Name     string
	Internal []net.IP
	External []net.IP
}

// NodeStore is a cache.Store that maintains the full set of nodes, and notifies interested parties
// of changes.
type NodeStore struct {
	sync.Mutex
	Name    string             // The name of the NodeStore, for observability (logging, metrics, tracing).
	Timeout time.Duration      // How long to block (worst case) on events.
	Ch      chan UpdateRequest // A channel that will emit the full desired DNS record.
	Logger  *zap.Logger
	nodes   map[string]Node // The nodes, a map from hostname to information about that host.
}

// NewNodeStore returns an initialized NodeStore.
func NewNodeStore(name string) *NodeStore {
	return &NodeStore{Name: name, Timeout: 10 * time.Second, Ch: make(chan UpdateRequest), Logger: zap.L().Named(name), nodes: make(map[string]Node)}
}

func (s *NodeStore) startOp(opName string) (context.Context, func()) {
	nodeChangeEvents.WithLabelValues(s.Name, opName).Inc()
	tctx, c := context.WithTimeout(context.Background(), s.Timeout)
	span := opentracing.StartSpan("node-change")
	ctx := opentracing.ContextWithSpan(tctx, span)

	return ctx, func() {
		select {
		case <-ctx.Done():
			ext.Error.Set(span, true)
			s.Logger.Error("context expired during notification", zap.String("op", opName), zap.Error(ctx.Err()))
		default:
		}
		c()
		span.Finish()
	}
}

func toNode(obj interface{}) Node {
	n, ok := obj.(*v1.Node)
	if !ok {
		// The reflector also does this check, so this should never happen.
		zap.L().Error("wrong-type object", zap.Any("obj", obj))
		return Node{}
	}
	result := Node{Name: n.GetName()}
	for _, addr := range n.Status.Addresses {
		parsed := net.ParseIP(addr.Address)
		switch addr.Type {
		case v1.NodeExternalIP:
			result.External = append(result.External, parsed)
		case v1.NodeInternalIP:
			result.Internal = append(result.Internal, parsed)
		case v1.NodeHostName:
		case v1.NodeExternalDNS:
		case v1.NodeInternalDNS:
			// We ignore these, but they could be used to generate CNAME records.
		}
	}
	return result
}

func (s *NodeStore) externalRecord() Record {
	result := Record{IsInternal: false}
	for _, node := range s.nodes {
		result.IPs = append(result.IPs, node.External...)
	}
	cleanupRecord(&result)
	return result
}

func (s *NodeStore) internalRecord() Record {
	result := Record{IsInternal: true}
	for _, node := range s.nodes {
		result.IPs = append(result.IPs, node.Internal...)
	}
	cleanupRecord(&result)
	return result
}

func cleanupRecord(r *Record) {
	dedup := make(map[string]net.IP)
	for _, addr := range r.IPs {
		dedup[addr.To16().String()] = addr
	}
	keys := make([]string, 0, len(dedup))
	for key := range dedup {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	r.IPs = make([]net.IP, 0, len(dedup))
	for _, key := range keys {
		r.IPs = append(r.IPs, dedup[key])
	}
}

func (s *NodeStore) mutateNodes(f func(*map[string]Node)) []Record {
	s.Lock()
	defer s.Unlock()
	beforeInternal, beforeExternal := s.externalRecord(), s.internalRecord()
	f(&s.nodes)
	nodeCount.WithLabelValues(s.Name).Set(float64(len(s.nodes)))
	afterInternal, afterExternal := s.externalRecord(), s.internalRecord()

	var result []Record
	if diff := cmp.Diff(beforeExternal, afterExternal); diff != "" {
		result = append(result, afterExternal)
	}
	if diff := cmp.Diff(beforeInternal, afterInternal); diff != "" {
		result = append(result, afterInternal)
	}
	return result
}

// notify sends an update request to the notification channel.  Because the actual update happens
// asyncronously, we can't tell if it worked or not.  We also can't really apply backpressure when
// the context times out; Kubernetes is not going to stop adding or removing nodes because we are
// slow at updating the DNS.
func (s *NodeStore) notify(ctx context.Context, changes []Record) {
	for _, change := range changes {
		span, ctx := opentracing.StartSpanFromContext(ctx, "update_dns")
		select {
		case s.Ch <- UpdateRequest{Ctx: ctx, Record: change}:
		case <-ctx.Done():
			ext.Error.Set(span, true)
		}
		span.Finish()
	}
}

// Add implements cache.Store.
func (s *NodeStore) Add(obj interface{}) error {
	ctx, c := s.startOp("add")
	defer c()
	node := toNode(obj)
	changes := s.mutateNodes(func(nodes *map[string]Node) {
		(*nodes)[node.Name] = node
	})
	s.notify(ctx, changes)
	return nil
}

// Update implements cache.Store.
func (s *NodeStore) Update(obj interface{}) error {
	ctx, c := s.startOp("update")
	defer c()
	node := toNode(obj)
	changes := s.mutateNodes(func(nodes *map[string]Node) {
		(*nodes)[node.Name] = node
	})
	s.notify(ctx, changes)
	return nil
}

// Delete implements cache.Store.
func (s *NodeStore) Delete(obj interface{}) error {
	ctx, c := s.startOp("delete")
	defer c()
	node := toNode(obj)
	changes := s.mutateNodes(func(nodes *map[string]Node) {
		delete(*nodes, node.Name)
	})
	s.notify(ctx, changes)
	return nil
}

// Replace implements cache.Store.
func (s *NodeStore) Replace(objs []interface{}, unusedResourceVersion string) error {
	ctx, c := s.startOp("replace")
	defer c()
	changes := s.mutateNodes(func(nodes *map[string]Node) {
		newNodes := make(map[string]Node)
		for _, obj := range objs {
			node := toNode(obj)
			newNodes[node.Name] = node
		}
		*nodes = newNodes
	})
	s.notify(ctx, changes)
	return nil
}

// Resync implements cache.Store.
func (s *NodeStore) Resync() error {
	ctx, c := s.startOp("resync")
	defer c()
	ext, int := s.externalRecord(), s.internalRecord()
	s.notify(ctx, []Record{ext, int})
	return nil
}

// We only implement cache.Store for cache.Reflector, and cache.Reflector does not call List/Get methods.
func (s *NodeStore) List() []interface{} { return nil }
func (s *NodeStore) ListKeys() []string  { return nil }
func (s *NodeStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	return nil, false, errors.New("unimplemented")
}
func (s *NodeStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	return nil, false, errors.New("unimplemented")
}

// WatchNodes connects to the k8s API server (using an in-cluster configuration if kubconfig and
// master are empty), watches nodes until the provided context is finished, and publishes any
// changes to the provided cache.Store.
//
// The provided watcher will be resync'd at a scheduled interval regardless of any changes if
// resync is non-zero.
func WatchNodes(ctx context.Context, master, kubeconfig string, resync time.Duration, store cache.Store) error {
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		return fmt.Errorf("kubernetes: build config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("kubernetes: new client: %w", err)
	}

	lw := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "nodes", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Node{}, store, resync)
	r.Run(ctx.Done())
	return nil
}
