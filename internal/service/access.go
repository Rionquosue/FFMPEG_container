package service

import (
	"bufio"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccessPolicy decides which TCP peers may talk to the API at all. Allowed:
//
//   - loopback (the container itself, e.g. the Docker healthcheck);
//   - the private subnets this container is directly attached to, which are
//     its Docker networks (read from the kernel routing table, refreshed
//     periodically so `docker network connect` takes effect);
//   - addresses and CIDR ranges listed in WHITELIST_IPS.
//
// Everything else is dropped at accept, before any HTTP is read. Unix
// socket connections are not filtered here; filesystem permissions on the
// socket govern them.
type AccessPolicy struct {
	whitelist []netip.Prefix
	subnets   func() []netip.Prefix
	mu        sync.Mutex
	cached    []netip.Prefix
	loadedAt  time.Time
}

const subnetRefresh = 10 * time.Second

func NewAccessPolicy(whitelist []netip.Prefix) *AccessPolicy {
	return &AccessPolicy{whitelist: whitelist, subnets: attachedSubnets}
}

// ParseWhitelist reads a comma- or space-separated list of IPs and CIDRs.
func ParseWhitelist(value string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if strings.Contains(item, "/") {
			p, e := netip.ParsePrefix(item)
			if e != nil {
				return nil, errors.New("invalid WHITELIST_IPS entry " + strconv.Quote(item))
			}
			out = append(out, p.Masked())
			continue
		}
		a, e := netip.ParseAddr(item)
		if e != nil {
			return nil, errors.New("invalid WHITELIST_IPS entry " + strconv.Quote(item))
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(out) > 256 {
		return nil, errors.New("WHITELIST_IPS allows at most 256 entries")
	}
	return out, nil
}

func (p *AccessPolicy) Allowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	for _, prefix := range p.whitelist {
		if prefix.Contains(addr) {
			return true
		}
	}
	for _, prefix := range p.attached() {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (p *AccessPolicy) attached() []netip.Prefix {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == nil || time.Since(p.loadedAt) > subnetRefresh {
		p.cached = p.subnets()
		p.loadedAt = time.Now()
	}
	return p.cached
}

// attachedSubnets returns the private, directly connected (no gateway)
// routes of this network namespace. net.Interfaces is not used because it
// needs a netlink socket, which the API's seccomp filter forbids.
func attachedSubnets() []netip.Prefix {
	var out []netip.Prefix
	if f, e := os.Open("/proc/net/route"); e == nil {
		out = append(out, parseRoutes4(f)...)
		f.Close()
	}
	if f, e := os.Open("/proc/net/ipv6_route"); e == nil {
		out = append(out, parseRoutes6(f)...)
		f.Close()
	}
	if out == nil {
		out = []netip.Prefix{}
	}
	return out
}

// usable keeps only private, non-default networks: Docker's address pools
// are private, and a public or default route would admit the internet.
func usable(p netip.Prefix) bool {
	return p.Bits() > 0 && p.Addr().IsPrivate()
}

// /proc/net/route: Iface Destination Gateway Flags RefCnt Use Metric Mask ...
// with little-endian hex addresses.
func parseRoutes4(r io.Reader) []netip.Prefix {
	var out []netip.Prefix
	scanner := bufio.NewScanner(r)
	scanner.Scan() // header
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) < 8 || f[0] == "lo" || f[2] != "00000000" {
			continue
		}
		dest, ok1 := le32(f[1])
		mask, ok2 := le32(f[7])
		if !ok1 || !ok2 {
			continue
		}
		bits := 0
		for m := mask; m&0x80000000 != 0; m <<= 1 {
			bits++
		}
		addr := netip.AddrFrom4([4]byte{byte(dest >> 24), byte(dest >> 16), byte(dest >> 8), byte(dest)})
		if p := netip.PrefixFrom(addr, bits).Masked(); usable(p) {
			out = append(out, p)
		}
	}
	return out
}

func le32(s string) (uint32, bool) {
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != 4 {
		return 0, false
	}
	return uint32(b[3])<<24 | uint32(b[2])<<16 | uint32(b[1])<<8 | uint32(b[0]), true
}

// /proc/net/ipv6_route: dest destlen src srclen nexthop metric refcnt use flags iface
func parseRoutes6(r io.Reader) []netip.Prefix {
	var out []netip.Prefix
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) < 10 || f[9] == "lo" || f[4] != strings.Repeat("0", 32) {
			continue
		}
		raw, e := hex.DecodeString(f[0])
		bits, e2 := strconv.ParseUint(f[1], 16, 8)
		if e != nil || e2 != nil || len(raw) != 16 || bits > 128 {
			continue
		}
		if p := netip.PrefixFrom(netip.AddrFrom16([16]byte(raw)), int(bits)).Masked(); usable(p) {
			out = append(out, p)
		}
	}
	return out
}

// FilteredListener closes connections from peers the policy rejects. Unix
// socket peers have no IP address and are passed through.
type FilteredListener struct {
	net.Listener
	Allow func(netip.Addr) bool
	Log   *slog.Logger
}

func (l *FilteredListener) Accept() (net.Conn, error) {
	for {
		conn, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return conn, nil
		}
		peer := tcp.AddrPort().Addr()
		if l.Allow(peer) {
			return conn, nil
		}
		conn.Close()
		if l.Log != nil {
			l.Log.Warn("connection refused by access policy", "peer", peer.String())
		}
	}
}
