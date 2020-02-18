// Command nodedns watches Kubernetes for changes to nodes, and updates a DNS record to contain the IP addresses
// of all nodes.
package main

import (
	"context"
	"time"

	"github.com/jrockway/nodedns/pkg/k8s"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
)

type kflags struct {
	Kubeconfig string `long:"kubeconfig" env:"KUBECONFIG" description:"kubeconfig to use to connect to the cluster, when running outside of the cluster"`
	Master     string `long:"master" env:"KUBE_MASTER" description:"url of the kubernetes master, only necessary when running outside of the cluster and when it's not specified in the provided kubeconfig"`
}

type nodednsflags struct {
	Resync time.Duration `long:"resync" env:"RESYNC_INTERVAL" description:"resync the current state of nodes to DNS at this interval"`
}

func main() {
	server.AppName = "nodedns"

	kf := new(kflags)
	server.AddFlagGroup("Kubernetes", kf)
	ndf := new(nodednsflags)
	server.AddFlagGroup("NodeDNS", ndf)
	server.Setup()

	ns := k8s.NewNodeStore("main")
	go func() {
		for req := range ns.Ch {
			if req.Record.IsInternal {
				zap.L().Info("current internal addresses", zap.Any("addresses", req.Record.IPs))
			} else {
				zap.L().Info("current external addresses", zap.Any("addresses", req.Record.IPs))
			}
		}
	}()

	ctx := context.Background()
	go func() {
		if err := k8s.WatchNodes(ctx, kf.Master, kf.Kubeconfig, ndf.Resync, ns); err != nil {
			zap.L().Fatal("watch nodes errored", zap.Error(err))
		}
	}()

	server.ListenAndServe()
}
