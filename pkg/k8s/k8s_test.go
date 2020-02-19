package k8s

import (
	"net"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCache(t *testing.T) {
	l := zaptest.NewLogger(t)
	zap.ReplaceGlobals(l)
	ns := NewNodeStore("test")
	ns.Timeout = time.Second
	ch := make(chan UpdateRequest)
	ns.OnChange = func(req UpdateRequest) { ch <- req }
	readNext := func(n int) []Record {
		t.Helper()
		var result []Record
		a := time.After(time.Second)
		for i := 0; i < n; i++ {
			select {
			case <-a:
				t.Fatalf("channel read timed out waiting for item %d", i)
			case req := <-ch:
				result = append(result, req.Record)
			}
		}
		return result
	}
	go ns.Replace([]interface{}{
		&v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "host-1",
			},
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{
					{
						Type:    v1.NodeHostName,
						Address: "host-1",
					},
					{
						Type:    v1.NodeExternalDNS,
						Address: "host-1.example.com",
					},
					{
						Type:    v1.NodeExternalIP,
						Address: "42.0.0.1",
					},
					{ // dup on purpose
						Type:    v1.NodeExternalIP,
						Address: "42.0.0.1",
					},
					{
						Type:    v1.NodeInternalIP,
						Address: "10.0.0.1",
					},
				},
			},
		},
	}, "")
	got := readNext(2)
	want := []Record{
		{IsInternal: true, IPs: []net.IP{net.IPv4(10, 0, 0, 1)}},
		{IsInternal: false, IPs: []net.IP{net.IPv4(42, 0, 0, 1)}},
	}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("replace:\n%s", diff)
	}

	go ns.Update(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "host-1",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "host-1",
				},
				{
					Type:    v1.NodeExternalDNS,
					Address: "host-1.k8s.example.com",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "42.0.0.1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
			},
		},
	})
	select {
	case <-ch:
		t.Fatal("unexpected update")
	case <-time.After(100 * time.Millisecond):
		// This is not an ideal test, but if there really was a write here we'll eventually
		// catch it.
	}

	go ns.Update(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "host-1",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "host-1",
				},
				{
					Type:    v1.NodeExternalDNS,
					Address: "host-1.k8s.example.com",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "42.0.0.123",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
			},
		},
	})
	got = readNext(1)
	want = []Record{{IsInternal: false, IPs: []net.IP{net.IPv4(42, 0, 0, 123)}}}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("update:\n %s", diff)
	}

	go ns.Add(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "host-2",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "host-2",
				},
				{
					Type:    v1.NodeExternalDNS,
					Address: "host-2.k8s.example.com",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "42.0.0.2",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.2",
				},
			},
		},
	})
	got = readNext(2)
	want = []Record{
		{IsInternal: true, IPs: []net.IP{net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2)}},
		{IsInternal: false, IPs: []net.IP{net.IPv4(42, 0, 0, 123), net.IPv4(42, 0, 0, 2)}},
	}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("update:\n%s", diff)
	}

	go ns.Update(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "host-2",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "host-2",
				},
				{
					Type:    v1.NodeExternalDNS,
					Address: "host-2.k8s.example.com",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.2",
				},
			},
		},
	})
	got = readNext(1)
	want = []Record{
		{IsInternal: false, IPs: []net.IP{net.IPv4(42, 0, 0, 123)}},
	}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("update:\n%s", diff)
	}

	go ns.Delete(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "host-2",
		},
	})
	got = readNext(1)
	want = []Record{
		{IsInternal: true, IPs: []net.IP{net.IPv4(10, 0, 0, 1)}},
	}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("delete:\n%s", diff)
	}

	go ns.Resync()
	got = readNext(2)
	want = []Record{
		{IsInternal: false, IPs: []net.IP{net.IPv4(42, 0, 0, 123)}},
		{IsInternal: true, IPs: []net.IP{net.IPv4(10, 0, 0, 1)}},
	}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("resync:\n%s", diff)
	}
}
