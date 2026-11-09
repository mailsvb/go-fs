package sonos

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

// sweepDial bounds the connection to one address of a sweep: a speaker on the
// network accepts at once, an address no one has times out.
const sweepDial = 700 * time.Millisecond

// sweepParallel is how many addresses a sweep tries at a time.
const sweepParallel = 128

// sweepMaxBits is the largest network a sweep tries whole, a /22 of 1022
// addresses; of a larger one it tries the /24 the server is in.
const sweepMaxBits = 22

// Sweep looks for speakers where a search finds none: it tries the port of
// the speakers on every address of the networks Subnets names, and describes
// those that accept. Multicast may not cross from a wired network to a
// wireless one, and a firewall that follows connections drops the answers to
// a search, as they come from the speaker rather than from the group the
// search went to; a connection the server opens is let through both. A
// speaker answers a search sent to its address alone no more than one sent
// to the group, so it is the port that is tried. One speaker found is
// enough, as it knows the groups of its whole household.
func (c *Client) Sweep(ctx context.Context) []Player {
	if c.Subnets == nil {
		return nil
	}
	var hosts []string
	for _, prefix := range c.Subnets() {
		for _, addr := range sweepAddrs(prefix) {
			hosts = append(hosts, net.JoinHostPort(addr.String(), c.port))
		}
	}
	if len(hosts) == 0 {
		return nil
	}

	var mu sync.Mutex
	var players []Player
	work := make(chan string)
	var wg sync.WaitGroup
	for range min(sweepParallel, len(hosts)) {
		wg.Go(func() {
			dialer := net.Dialer{Timeout: sweepDial}
			for host := range work {
				conn, err := dialer.DialContext(ctx, "tcp", host)
				if err != nil {
					continue
				}
				_ = conn.Close()
				player, err := c.describe(ctx, "http://"+host+"/xml/device_description.xml")
				if err != nil {
					continue
				}
				mu.Lock()
				players = append(players, player)
				mu.Unlock()
			}
		})
	}
send:
	for _, host := range hosts {
		select {
		case work <- host:
		case <-ctx.Done():
			break send
		}
	}
	close(work)
	wg.Wait()
	return players
}

// sweepAddrs is the addresses of a network a sweep tries: all but the
// network and broadcast addresses, of no more than a /22, else of the /24
// around the address given.
func sweepAddrs(prefix netip.Prefix) []netip.Addr {
	if !prefix.Addr().Is4() {
		return nil
	}
	if prefix.Bits() < sweepMaxBits {
		prefix = netip.PrefixFrom(prefix.Addr(), 24)
	}
	prefix = prefix.Masked()
	var addrs []netip.Addr
	for a := prefix.Addr(); prefix.Contains(a); a = a.Next() {
		addrs = append(addrs, a)
	}
	if prefix.Bits() <= 30 {
		addrs = addrs[1 : len(addrs)-1]
	}
	return addrs
}

// localSubnets is the private IPv4 networks of the interfaces that are up,
// the loopback and link-local ones left out: speakers are on a home or an
// office network, and a server on a public address is not to try the port
// on its neighbours.
func localSubnets() []netip.Prefix {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var subnets []netip.Prefix
	for _, ifi := range interfaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(n.IP.To4())
			if !ok || !addr.IsPrivate() {
				continue
			}
			bits, _ := n.Mask.Size()
			subnets = append(subnets, netip.PrefixFrom(addr, bits))
		}
	}
	return subnets
}
