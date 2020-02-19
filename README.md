# jrockway/nodedns

This is a small Kubernetes application that asks the Kubernetes API for a list of all nodes in the
cluster, and then updates a DNS entry to contain them all.

## Gotchas

A node's inclusion in the DNS record is gated on being scheduleable and Ready (the same logic that
Kubernetes uses when including a node in a Service). This means that if all nodes become un-ready,
we will delete all the DNS records. The NXDOMAIN that clients will see will be cached for the TTL
set in your domain's SOA record, not the TTL that would be on the individual records.
