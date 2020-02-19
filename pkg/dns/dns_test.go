package dns

import (
	"net"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
