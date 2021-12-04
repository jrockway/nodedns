package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jrockway/opinionated-server/client"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
)

func lessIPs(a, b net.IP) bool {
	return string(a) < string(b)
}

func TestDiffDNS(t *testing.T) {
	testData := []struct {
		existing   map[string]int
		desired    []net.IP
		wantDelete []int
		wantCreate []net.IP
	}{
		{
			existing:   nil,
			desired:    nil,
			wantDelete: nil,
			wantCreate: nil,
		},
		{
			existing:   map[string]int{},
			desired:    []net.IP{net.IPv4(1, 2, 3, 4), net.IPv4(1, 2, 3, 5)},
			wantDelete: nil,
			wantCreate: []net.IP{net.IPv4(1, 2, 3, 4), net.IPv4(1, 2, 3, 5)},
		},
		{
			existing:   map[string]int{"1.2.3.4": 1234},
			desired:    nil,
			wantDelete: []int{1234},
			wantCreate: nil,
		},
		{
			existing:   map[string]int{"1.2.3.4": 1234},
			desired:    []net.IP{net.IPv4(1, 2, 3, 4)},
			wantDelete: nil,
			wantCreate: nil,
		},
		{
			existing:   map[string]int{"1.2.3.4": 1234},
			desired:    []net.IP{net.IPv4(1, 2, 3, 5)},
			wantDelete: []int{1234},
			wantCreate: []net.IP{net.IPv4(1, 2, 3, 5)},
		},
		{
			existing:   map[string]int{"1.2.3.4": 1234, "1.2.3.5": 1235},
			desired:    []net.IP{net.IPv4(1, 2, 3, 5), net.IPv4(1, 2, 3, 6)},
			wantDelete: []int{1234},
			wantCreate: []net.IP{net.IPv4(1, 2, 3, 6)},
		},
		{
			existing:   map[string]int{"1.2.3.4": 1234},
			desired:    []net.IP{net.IPv4(1, 2, 3, 4).To16()},
			wantDelete: nil,
			wantCreate: nil,
		},
	}

	for i, test := range testData {
		gotDelete, gotCreate, _ := diffDNS(test.desired, test.existing)
		if diff := cmp.Diff(gotDelete, test.wantDelete, cmpopts.EquateEmpty(), cmpopts.SortSlices(lessIPs)); diff != "" {
			t.Errorf("test %d: to delete:\n%s", i, diff)
		}
		if diff := cmp.Diff(gotCreate, test.wantCreate, cmpopts.EquateEmpty(), cmpopts.SortSlices(lessIPs)); diff != "" {
			t.Errorf("test %d: to create:\n%s", i, diff)
		}
	}
}

type testTransport struct {
	t     *testing.T
	pause time.Duration
	err   error
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.pause != 0 {
		t.t.Logf("pause %v", t.pause.String())
		time.Sleep(t.pause)
	}
	if t.err != nil {
		t.t.Logf("transport erroring: %v", t.err)
		return nil, t.err
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if req.URL.Path == "/v2/domains/example.com/records/1" {
		if req.Method == "DELETE" {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Status:     "204 No Content",
				Body:       jsonReader(make(map[string]interface{})),
			}, nil
		}
	}
	if req.URL.Path == "/v2/domains/example.com/records" {
		if req.Method == "POST" {
			return &http.Response{
				StatusCode: http.StatusCreated,
				Status:     "201 Created",
				Body:       jsonReader(make(map[string]interface{})),
			}, nil
		}
		if page := req.URL.Query().Get("page"); page == "1" || page == "" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body: jsonReader(map[string]interface{}{
					"domain_records": []godo.DomainRecord{
						{
							ID:   1,
							Type: "A",
							Name: "nodes.example.com",
							Data: "10.0.0.1",
						},
					},
					"meta": godo.Meta{},
					"links": godo.Links{
						Pages: &godo.Pages{},
					},
				}),
			}, nil
		} else {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}
	}
	panic("no error or response to return")
}

func jsonReader(obj map[string]interface{}) io.ReadCloser {
	buf := new(bytes.Buffer)
	b, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("invalid json: %v", err))
	}
	buf.Write(b)
	return io.NopCloser(buf)
}

func TestUpdateDNS(t *testing.T) {
	l := zaptest.NewLogger(t, zaptest.Level(zapcore.DebugLevel))
	zap.ReplaceGlobals(l)
	tr := &testTransport{t: t}
	doc := godo.NewClient(&http.Client{
		Transport: client.WrapRoundTripper(tr),
	})
	c := &Client{
		c:    doc,
		zone: "example.com",
		ttl:  time.Second,
	}

	// Test a "change" flow.
	ctx := context.Background()
	if err := c.UpdateDNS(ctx, "nodes.example.com", []net.IP{net.IPv4(1, 2, 3, 4)}); err != nil {
		t.Fatal(err)
	}

	// Test the change flow with a context that expires.
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	tr.pause = time.Second
	err := c.UpdateDNS(ctx, "nodes.example.com", []net.IP{net.IPv4(10, 0, 0, 1)})
	if err == nil {
		t.Fatal("expected error, but got success")
	}
	cancel()
}
