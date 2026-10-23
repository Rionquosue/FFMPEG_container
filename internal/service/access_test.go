package service

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseWhitelist(t *testing.T) {
	list, e := ParseWhitelist("203.0.113.7, 198.51.100.0/24 2001:db8::/64,::ffff:192.0.2.1")
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"203.0.113.7/32", "198.51.100.0/24", "2001:db8::/64", "192.0.2.1/32"}
	for i, p := range list {
		if p.String() != want[i] {
			t.Fatalf("%d: %s", i, p)
		}
	}
	for _, bad := range []string{"example.com", "10.0.0.0/33", "1.2.3"} {
		if _, e := ParseWhitelist(bad); e == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestRouteParsingKeepsOnlyAttachedPrivateNetworks(t *testing.T) {
	routes4 := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" + // default via 172.18.0.1
		"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" + // 172.18.0.0/16
		"eth1\t0010A8C0\t00000000\t0001\t0\t0\t0\t00F0FFFF\t0\t0\t0\n" + // 192.168.16.0/20
		"eth2\t00710CCB\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" + // 203.12.113.0/24, public
		"lo\t0000007F\t00000000\t0001\t0\t0\t0\t000000FF\t0\t0\t0\n"
	got := parseRoutes4(strings.NewReader(routes4))
	if len(got) != 2 || got[0].String() != "172.18.0.0/16" || got[1].String() != "192.168.16.0/20" {
		t.Fatal(got)
	}
	routes6 := "fd00000000000000000000000000ab00 78 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001 eth0\n" +
		"fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001 eth0\n" +
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fd000000000000000000000000000001 00000400 00000001 00000000 00000003 eth0\n"
	got = parseRoutes6(strings.NewReader(routes6))
	if len(got) != 1 || got[0].String() != "fd00::ab00/120" {
		t.Fatal(got)
	}
}

func TestAccessPolicy(t *testing.T) {
	p := &AccessPolicy{
		whitelist: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")},
		subnets:   func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")} },
	}
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "::ffff:127.0.0.1": true, // loopback
		"172.18.0.5": true, "::ffff:172.18.3.9": true, // attached Docker network
		"203.0.113.7": true,                                           // WHITELIST_IPS
		"172.19.0.5":  false, "192.168.1.20": false, "8.8.8.8": false, // anything else
	} {
		if got := p.Allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}

func TestFilteredListenerDropsRejectedPeers(t *testing.T) {
	raw, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var allow atomic.Bool
	l := &FilteredListener{Listener: raw, Allow: func(netip.Addr) bool { return allow.Load() }}
	defer l.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })}
	go server.Serve(l)
	defer server.Close()
	client := http.Client{Timeout: 2 * time.Second}
	url := "http://" + raw.Addr().String() + "/"
	if _, e := client.Get(url); e == nil {
		t.Fatal("rejected peer was served")
	}
	allow.Store(true)
	r, e := client.Get(url)
	if e != nil || r.StatusCode != 204 {
		t.Fatal(e)
	}
	r.Body.Close()
}

func TestServesOverUnixSocket(t *testing.T) {
	s := setup(t, fakeProcessor{})
	path := filepath.Join(t.TempDir(), "api.sock")
	l, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	server := &http.Server{Handler: s}
	go server.Serve(l)
	defer server.Close()
	client := http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}}
	req, _ := http.NewRequest("GET", "http://unix/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	r, e := client.Do(req)
	if e != nil || r.StatusCode != 200 {
		t.Fatal(e)
	}
	r.Body.Close()
}

func TestConfigListenerOptions(t *testing.T) {
	c := DefaultConfig()
	c.Addr = "off"
	if c.Validate() == nil {
		t.Fatal("no listener accepted")
	}
	c.UnixSocket = "relative.sock"
	if c.Validate() == nil {
		t.Fatal("relative socket accepted")
	}
	c.UnixSocket = "/run/ffmpeg-api/api.sock"
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
}
