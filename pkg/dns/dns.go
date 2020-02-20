// DNS updates DNS records on DigitalOcean DNS.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/digitalocean/godo"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

var (
	dnsUpdateAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_update_attempts",
			Help: "The number of attempts to update DNS.",
		},
		[]string{"provider", "zone", "record"},
	)
	dnsUpdatedOK = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_update_success",
			Help: "The number of attempts to update DNS that ended in succcess.",
		},
		[]string{"provider", "zone", "record"},
	)
	dnsRecordsCreated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_records_created",
			Help: "The number of A/AAAA records added to DNS.",
		},
		[]string{"provider", "zone", "record"},
	)
	dnsRecordsDeleted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_records_deleted",
			Help: "The number of A/AAAA records removed from DNS.",
		},
		[]string{"provider", "zone", "record"},
	)
	doRequestsRemaining = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "digitalocean_requests_remaining",
			Help: "The number of API requests remaining on the DigitalOcean client.",
		},
	)
)

// Config is configuration for the DigitalOcean client that will update records.
type Config struct {
	// Personal authentication token.
	PAToken string `long:"token" env:"DIGITALOCEAN_TOKEN" description:"The DigitalOcean personal access token to use to update DNS."`
	// Name of the DNS zone to create/update the record in.
	Zone string `long:"zone" env:"DNS_ZONE" description:"The name of the DigitalOcean DNS zone that your records are in."`
	// TTL of the created DNS records.
	TTL time.Duration `long:"ttl" env:"DNS_TTL" description:"The TTL to apply to newly-created records." default:"60s"`
}

// transport is an http.RoundTripper that adds the DO token to each request, and traces the request
// with opentracing.
type transport struct {
	Token            *oauth2.Token
	nethttpTransport *nethttp.Transport
}

// RoundTrip implements http.RoundTripper.
func (t *transport) RoundTrip(orig *http.Request) (*http.Response, error) {
	req, tr := nethttp.TraceRequest(opentracing.GlobalTracer(), orig)
	t.Token.SetAuthHeader(req)
	defer tr.Finish()
	return t.nethttpTransport.RoundTrip(req)
}

// Client is a DigitalOcean API client configured to use opentracing.
type Client struct {
	c    *godo.Client
	zone string
	ttl  time.Duration
}

// NewClient creates a new DigitalOcean API client and checks that it works.
func NewClient(ctx context.Context, c *Config) (*Client, error) {
	httpClient := &http.Client{
		Transport: &transport{
			Token: &oauth2.Token{
				AccessToken: c.PAToken,
			},
			nethttpTransport: &nethttp.Transport{},
		},
	}
	godoClient := godo.NewClient(httpClient)
	godoClient.OnRequestCompleted(func(req *http.Request, res *http.Response) {
		if res == nil {
			return
		}
		if remaining := res.Header.Get("RateLimit-Remaining"); remaining != "" {
			val, err := strconv.Atoi(remaining)
			if err == nil {
				doRequestsRemaining.Set(float64(val))
			}
		}
	})
	domains, _, err := godoClient.Domains.List(ctx, &godo.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	var found bool
	for _, d := range domains {
		if d.Name == c.Zone {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no domain named %q found", c.Zone)
	}

	return &Client{c: godoClient, zone: c.Zone, ttl: c.TTL}, nil
}

func (c *Client) getRecords(ctx context.Context, name string) (map[string]int, error) {
	result := make(map[string]int)
	for page := 0; page < 100; page++ {
		recs, res, err := c.c.Domains.Records(ctx, c.zone, &godo.ListOptions{
			Page:    page,
			PerPage: 100,
		})
		if err != nil {
			return nil, fmt.Errorf("get page %d of records for domain %s: %w", page, c.zone, err)
		}
		for _, rec := range recs {
			if (rec.Type == "A" || rec.Type == "AAAA") && rec.Name == name {
				result[rec.Data] = rec.ID
			}
		}
		if res.Links != nil && res.Links.IsLastPage() {
			return result, nil
		}
	}
	return result, errors.New("more than 100 pages!")
}

// diffDNS diffs the desired addresses against the existing map[address]id records, and returns a
// slice of IDs to delete, a slice of A/AAAA records to create, and a slice of the data in the
// records to delete (for logging).
func diffDNS(desired []net.IP, existing map[string]int) ([]int, []net.IP, []string) {
	addrs := make(map[string]struct{})
	for _, addr := range desired {
		addrs[addr.String()] = struct{}{}
	}

	toDeleteMap := make(map[int]struct{})
	var toDeleteAddrs []string
	for ip, id := range existing {
		if _, ok := addrs[ip]; !ok {
			toDeleteMap[id] = struct{}{}
			toDeleteAddrs = append(toDeleteAddrs, ip)
		}
	}
	var toDelete []int
	for id := range toDeleteMap {
		toDelete = append(toDelete, id)
	}

	var toCreate []net.IP
	for _, addr := range desired {
		if _, ok := existing[addr.String()]; !ok {
			toCreate = append(toCreate, addr)
		}
	}
	return toDelete, toCreate, toDeleteAddrs
}

func (c *Client) UpdateDNS(ctx context.Context, record string, addresses []net.IP) error {
	if record == "" {
		return nil
	}
	span, ctx := opentracing.StartSpanFromContext(ctx, "digitalocean_dns_update")
	defer span.Finish()
	dnsUpdateAttempts.WithLabelValues("digitalocean", c.zone, record).Inc()

	existing, err := c.getRecords(ctx, record)
	if err != nil {
		return fmt.Errorf("get existing records: %w", err)
	}
	toDelete, toCreate, toDeleteAddrs := diffDNS(addresses, existing)
	if len(toDelete) > 0 || len(toCreate) > 0 {
		zap.L().Named("digitalocean-dns").Debug("dns changes needed", zap.Any("to_create", toCreate), zap.Strings("to_delete", toDeleteAddrs))
	}

	for _, ip := range toCreate {
		kind := "A"
		if ip.To4() == nil {
			kind = "AAAA"
		}
		_, _, err := c.c.Domains.CreateRecord(ctx, c.zone, &godo.DomainRecordEditRequest{
			Name: record,
			Data: ip.String(),
			TTL:  int(c.ttl.Round(time.Second).Seconds()),
			Type: kind,
		})
		if err != nil {
			return fmt.Errorf("creating record %s %s: %w", kind, ip.String(), err)
		}
		dnsRecordsCreated.WithLabelValues("digitalocean", c.zone, record).Inc()
		zap.L().Debug("created record")
	}
	for _, id := range toDelete {
		if _, err := c.c.Domains.DeleteRecord(ctx, c.zone, id); err != nil {
			return fmt.Errorf("deleting record id %d: %w", id, err)
		}
		dnsRecordsDeleted.WithLabelValues("digitalocean", c.zone, record).Inc()
		zap.L().Debug("deleted record")
	}

	dnsUpdatedOK.WithLabelValues("digitalocean", c.zone, record).Inc()
	return nil
}
