// Command nodedns watches Kubernetes for changes to nodes, and updates a DNS record to contain the IP addresses
// of all nodes.
package main

import (
	"context"
	"errors"
	"time"

	"github.com/jrockway/nodedns/pkg/dns"
	"github.com/jrockway/nodedns/pkg/k8s"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
)

type kflags struct {
	Kubeconfig string `long:"kubeconfig" env:"KUBECONFIG" description:"kubeconfig to use to connect to the cluster, when running outside of the cluster"`
	Master     string `long:"master" env:"KUBE_MASTER" description:"url of the kubernetes master, only necessary when running outside of the cluster and when it's not specified in the provided kubeconfig"`
}

type nodednsflags struct {
	IsDryRun bool          `long:"dry_run" env:"DRY_RUN" description:"don't actually update any dns records"`
	Resync   time.Duration `long:"resync" env:"RESYNC_INTERVAL" description:"resync the current state of nodes to DNS at this interval"`
	Internal string        `long:"internal_domain" env:"INTERNAL_DOMAIN" description:"the dns record that will store the nodes' internal addresses"`
	External string        `long:"external_domain" env:"EXTERNAL_DOMAIN" description:"the dns record that will store the nodes' external addresses"`
}

func main() {
	server.AppName = "nodedns"

	dnsCfg := new(dns.Config)
	server.AddFlagGroup("DigitalOcean", dnsCfg)
	kf := new(kflags)
	server.AddFlagGroup("Kubernetes", kf)
	ndf := new(nodednsflags)
	server.AddFlagGroup("NodeDNS", ndf)
	server.Setup()

	tctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	dnsClient, err := dns.NewClient(tctx, dnsCfg)
	c()
	if err != nil {
		zap.L().Fatal("problem initializing DigitalOcean client", zap.Error(err))
	}

	ns := k8s.NewNodeStore("main")
	ns.OnChange = func(req k8s.UpdateRequest) {
		var err error
		ips := req.Record.IPs
		if req.Record.IsInternal {
			zap.L().Info("current internal addresses", zap.Any("addresses", ips))
			if !ndf.IsDryRun {
				err = dnsClient.UpdateDNS(req.Ctx, ndf.Internal, ips)
			}
		} else {
			zap.L().Info("current external addresses", zap.Any("addresses", ips))
			if !ndf.IsDryRun {
				err = dnsClient.UpdateDNS(req.Ctx, ndf.External, ips)
			}
		}
		if ndf.IsDryRun {
			err = errors.New("dry_run enabled; not actually updating")
		}
		if err != nil {
			zap.L().Error("problem updating dns", zap.Error(err))
		}
	}

	go func() {
		ctx := context.Background()
		if err := k8s.WatchNodes(ctx, kf.Master, kf.Kubeconfig, ndf.Resync, ns); err != nil {
			zap.L().Fatal("watch nodes errored", zap.Error(err))
		}
	}()

	server.ListenAndServe()
}
