// DNS updates DNS records on DigitalOcean DNS.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
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
			Help: "number of attempts to update dns",
		},
		[]string{"provider", "zone", "record"},
	)
	dnsUpdatedOK = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_update_success",
			Help: "number of attempts to update dns that ended in succcess",
		},
		[]string{"provider", "zone", "record"},
	)
	doRequestsRemaining = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "digitalocean_requests_remaining",
			Help: "number of API requests remaining on the digitalocean client",
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

// Token implements oauth2.TokenSource.
func (c *Config) Token() (*oauth2.Token, error) {
	token := &oauth2.Token{
		AccessToken: c.PAToken,
	}
	return token, nil
}

func (c *Config) doClient(ctx context.Context) *godo.Client {
	httpClient := &http.Client{Transport: &nethttp.Transport{}}
	cctx := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	oauthClient := oauth2.NewClient(cctx, c)
	return godo.NewClient(oauthClient)
}

func updateDoRateLimitMetric(res *godo.Response) {
	if res != nil && res.Rate.Limit > 0 {
		doRequestsRemaining.Set(float64(res.Rate.Remaining))
	}
}

func (c *Config) getRecords(ctx context.Context, client *godo.Client, name string) (map[string]int, error) {
	result := make(map[string]int)
	for page := 0; page < 100; page++ {
		recs, res, err := client.Domains.Records(ctx, c.Zone, &godo.ListOptions{
			Page:    page,
			PerPage: 100,
		})
		updateDoRateLimitMetric(res)
		if err != nil {
			return nil, fmt.Errorf("get page %d of records for domain %s: %v", page, c.Zone, err)
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

func (c *Config) UpdateDNS(ctx context.Context, record string, addresses []net.IP) error {
	if record == "" {
		return nil
	}
	span, ctx := opentracing.StartSpanFromContext(ctx, "digitalocean-dns-update")
	defer span.Finish()
	dnsUpdateAttempts.WithLabelValues("digitalocean", c.Zone, record).Inc()

	cl := c.doClient(ctx)
	existing, err := c.getRecords(ctx, cl, record)
	if err != nil {
		return fmt.Errorf("get existing records: %v", err)
	}
	toDelete, toCreate, toDeleteAddrs := diffDNS(addresses, existing)
	zap.L().Named("digitalocean-dns").Debug("dns changes needed", zap.Any("to_create", toCreate), zap.Strings("to_delete", toDeleteAddrs))

	for _, ip := range toCreate {
		kind := "A"
		if ip.To4() == nil {
			kind = "AAAA"
		}
		rec, res, err := cl.Domains.CreateRecord(ctx, c.Zone, &godo.DomainRecordEditRequest{
			Name: record,
			Data: ip.String(),
			TTL:  int(c.TTL.Round(time.Second).Seconds()),
			Type: kind,
		})
		updateDoRateLimitMetric(res)
		if err != nil {
			return fmt.Errorf("creating record %s %s: %v", kind, ip.String(), err)
		}
		zap.L().Debug("created record", zap.Any("record", rec), zap.Any("response", res))
	}
	for _, id := range toDelete {
		res, err := cl.Domains.DeleteRecord(ctx, c.Zone, id)
		updateDoRateLimitMetric(res)
		if err != nil {
			return fmt.Errorf("deleting record id %d: %v", id, err)
		}
		zap.L().Debug("deleted record", zap.Any("response", res))
	}

	dnsUpdatedOK.WithLabelValues("digitalocean", c.Zone, record).Inc()
	return nil
}
