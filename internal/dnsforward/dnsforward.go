// Package dnsforward contains a DNS forwarding server.
package dnsforward

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/agherr"
	"github.com/AdguardTeam/AdGuardHome/internal/aghnet"
	"github.com/AdguardTeam/AdGuardHome/internal/dhcpd"
	"github.com/AdguardTeam/AdGuardHome/internal/dnsfilter"
	"github.com/AdguardTeam/AdGuardHome/internal/querylog"
	"github.com/AdguardTeam/AdGuardHome/internal/stats"
	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/golibs/log"
	"github.com/miekg/dns"
)

// DefaultTimeout is the default upstream timeout
const DefaultTimeout = 10 * time.Second

const (
	safeBrowsingBlockHost = "standard-block.dns.adguard.com"
	parentalBlockHost     = "family-block.dns.adguard.com"
)

var defaultDNS = []string{
	"https://dns10.quad9.net/dns-query",
}
var defaultBootstrap = []string{"9.9.9.10", "149.112.112.10", "2620:fe::10", "2620:fe::fe:10"}

// Often requested by all kinds of DNS probes
var defaultBlockedHosts = []string{"version.bind", "id.server", "hostname.bind"}

var webRegistered bool

// Server is the main way to start a DNS server.
//
// Example:
//  s := dnsforward.Server{}
//  err := s.Start(nil) // will start a DNS server listening on default port 53, in a goroutine
//  err := s.Reconfigure(ServerConfig{UDPListenAddr: &net.UDPAddr{Port: 53535}}) // will reconfigure running DNS server to listen on UDP port 53535
//  err := s.Stop() // will stop listening on port 53535 and cancel all goroutines
//  err := s.Start(nil) // will start listening again, on port 53535, in a goroutine
//
// The zero Server is empty and ready for use.
type Server struct {
	dnsProxy   *proxy.Proxy          // DNS proxy instance
	dnsFilter  *dnsfilter.DNSFilter  // DNS filter instance
	dhcpServer dhcpd.ServerInterface // DHCP server instance (optional)
	queryLog   querylog.QueryLog     // Query log instance
	stats      stats.Stats
	access     *accessCtx

	// autohostSuffix is the suffix used to detect internal hosts.  It must
	// be a valid top-level domain plus dots on each side.
	autohostSuffix string

	ipset          ipsetCtx
	subnetDetector *aghnet.SubnetDetector
	localResolvers aghnet.Exchanger

	tableHostToIP     map[string]net.IP // "hostname -> IP" table for internal addresses (DHCP)
	tableHostToIPLock sync.Mutex

	tablePTR     map[string]string // "IP -> hostname" table for reverse lookup
	tablePTRLock sync.Mutex

	// DNS proxy instance for internal usage
	// We don't Start() it and so no listen port is required.
	internalProxy *proxy.Proxy

	isRunning bool

	sync.RWMutex
	conf ServerConfig
}

// defaultAutohostSuffix is the default suffix used to detect internal hosts
// when no suffix is provided.  See the documentation for Server.autohostSuffix.
const defaultAutohostSuffix = ".lan."

// DNSCreateParams are parameters to create a new server.
type DNSCreateParams struct {
	DNSFilter      *dnsfilter.DNSFilter
	Stats          stats.Stats
	QueryLog       querylog.QueryLog
	DHCPServer     dhcpd.ServerInterface
	SubnetDetector *aghnet.SubnetDetector
	AutohostTLD    string
}

// tldToSuffix converts a top-level domain into an autohost suffix.
func tldToSuffix(tld string) (suffix string) {
	l := len(tld) + 2
	b := make([]byte, l)
	b[0] = '.'
	copy(b[1:], tld)
	b[l-1] = '.'

	return string(b)
}

// NewServer creates a new instance of the dnsforward.Server
// Note: this function must be called only once
func NewServer(p DNSCreateParams) (s *Server, err error) {
	var autohostSuffix string
	if p.AutohostTLD == "" {
		autohostSuffix = defaultAutohostSuffix
	} else {
		err = validateDomainNameLabel(p.AutohostTLD)
		if err != nil {
			return nil, fmt.Errorf("autohost tld: %w", err)
		}

		autohostSuffix = tldToSuffix(p.AutohostTLD)
	}

	s = &Server{
		dnsFilter:      p.DNSFilter,
		stats:          p.Stats,
		queryLog:       p.QueryLog,
		subnetDetector: p.SubnetDetector,
		autohostSuffix: autohostSuffix,
	}

	if p.DHCPServer != nil {
		s.dhcpServer = p.DHCPServer
		s.dhcpServer.SetOnLeaseChanged(s.onDHCPLeaseChanged)
		s.onDHCPLeaseChanged(dhcpd.LeaseChangedAdded)
	}

	if runtime.GOARCH == "mips" || runtime.GOARCH == "mipsle" {
		// Use plain DNS on MIPS, encryption is too slow
		defaultDNS = defaultBootstrap
	}

	return s, nil
}

// NewCustomServer creates a new instance of *Server with custom internal proxy.
func NewCustomServer(internalProxy *proxy.Proxy) *Server {
	s := &Server{}
	if internalProxy != nil {
		s.internalProxy = internalProxy
	}

	return s
}

// Close - close object
func (s *Server) Close() {
	s.Lock()
	s.dnsFilter = nil
	s.stats = nil
	s.queryLog = nil
	s.dnsProxy = nil

	err := s.ipset.Close()
	if err != nil {
		log.Error("closing ipset: %s", err)
	}

	s.Unlock()
}

// WriteDiskConfig - write configuration
func (s *Server) WriteDiskConfig(c *FilteringConfig) {
	s.RLock()
	sc := s.conf.FilteringConfig
	*c = sc
	c.RatelimitWhitelist = stringArrayDup(sc.RatelimitWhitelist)
	c.BootstrapDNS = stringArrayDup(sc.BootstrapDNS)
	c.AllowedClients = stringArrayDup(sc.AllowedClients)
	c.DisallowedClients = stringArrayDup(sc.DisallowedClients)
	c.BlockedHosts = stringArrayDup(sc.BlockedHosts)
	c.UpstreamDNS = stringArrayDup(sc.UpstreamDNS)
	s.RUnlock()
}

// RDNSSettings returns the copy of actual RDNS configuration.
func (s *Server) RDNSSettings() (localPTRResolvers []string, resolveClients bool) {
	s.RLock()
	defer s.RUnlock()

	localPTRResolvers = stringArrayDup(s.conf.LocalPTRResolvers)
	resolveClients = s.conf.ResolveClients

	return localPTRResolvers, resolveClients
}

// Resolve - get IP addresses by host name from an upstream server.
// No request/response filtering is performed.
// Query log and Stats are not updated.
// This method may be called before Start().
func (s *Server) Resolve(host string) ([]net.IPAddr, error) {
	s.RLock()
	defer s.RUnlock()
	return s.internalProxy.LookupIPAddr(host)
}

// RDNSExchanger is a resolver for clients' addresses.
type RDNSExchanger interface {
	// Exchange tries to resolve the ip in a suitable way, e.g. either as
	// local or as external.
	Exchange(ip net.IP) (host string, err error)
}

const (
	// rDNSEmptyAnswerErr is returned by Exchange method when the answer
	// section of respond is empty.
	rDNSEmptyAnswerErr agherr.Error = "the answer section is empty"

	// rDNSNotPTRErr is returned by Exchange method when the response is not
	// of PTR type.
	rDNSNotPTRErr agherr.Error = "the response is not a ptr"
)

// Exchange implements the RDNSExchanger interface for *Server.
func (s *Server) Exchange(ip net.IP) (host string, err error) {
	s.RLock()
	defer s.RUnlock()

	if !s.conf.ResolveClients {
		return "", nil
	}

	arpa := dns.Fqdn(aghnet.ReverseAddr(ip))
	req := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Id:               dns.Id(),
			RecursionDesired: true,
		},
		Compress: true,
		Question: []dns.Question{{
			Name:   arpa,
			Qtype:  dns.TypePTR,
			Qclass: dns.ClassINET,
		}},
	}

	var resp *dns.Msg
	if s.subnetDetector.IsLocallyServedNetwork(ip) {
		resp, err = s.localResolvers.Exchange(req)
	} else {
		ctx := &proxy.DNSContext{
			Proto:     "udp",
			Req:       req,
			StartTime: time.Now(),
		}
		err = s.internalProxy.Resolve(ctx)

		resp = ctx.Res
	}
	if err != nil {
		return "", err
	}

	if len(resp.Answer) == 0 {
		return "", fmt.Errorf("lookup for %q: %w", arpa, rDNSEmptyAnswerErr)
	}

	ptr, ok := resp.Answer[0].(*dns.PTR)
	if !ok {
		return "", fmt.Errorf("type checking: %w", rDNSNotPTRErr)
	}

	return strings.TrimSuffix(ptr.Ptr, "."), nil
}

// Start starts the DNS server.
func (s *Server) Start() error {
	s.Lock()
	defer s.Unlock()
	return s.startLocked()
}

// startLocked starts the DNS server without locking. For internal use only.
func (s *Server) startLocked() error {
	err := s.dnsProxy.Start()
	if err == nil {
		s.isRunning = true
	}
	return err
}

const defaultLocalTimeout = 5 * time.Second

// stringsSetSubtract subtracts b from a interpreted as sets.
//
// TODO(e.burkov): Move into our internal package for working with strings.
func stringsSetSubtract(a, b []string) (c []string) {
	// unit is an object to be used as value in set.
	type unit = struct{}

	cSet := make(map[string]unit)
	for _, k := range a {
		cSet[k] = unit{}
	}

	for _, k := range b {
		delete(cSet, k)
	}

	c = make([]string, len(cSet))
	i := 0
	for k := range cSet {
		c[i] = k
		i++
	}

	return c
}

// collectDNSIPAddrs returns the slice of IP addresses without port number which
// we are listening on.  For internal use only.
func (s *Server) collectDNSIPAddrs() (addrs []string, err error) {
	addrs = make([]string, len(s.conf.TCPListenAddrs)+len(s.conf.UDPListenAddrs))
	var i int
	var ip net.IP
	for _, addr := range s.conf.TCPListenAddrs {
		if addr == nil {
			continue
		}

		if ip = addr.IP; ip.IsUnspecified() {
			return aghnet.CollectAllIfacesAddrs()
		}

		addrs[i] = ip.String()
		i++
	}
	for _, addr := range s.conf.UDPListenAddrs {
		if addr == nil {
			continue
		}

		if ip = addr.IP; ip.IsUnspecified() {
			return aghnet.CollectAllIfacesAddrs()
		}

		addrs[i] = ip.String()
		i++
	}

	return addrs[:i], nil
}

// setupResolvers initializes the resolvers for local addresses.  For internal
// use only.
func (s *Server) setupResolvers(localAddrs []string) (err error) {
	if len(localAddrs) == 0 {
		var sysRes aghnet.SystemResolvers
		// TODO(e.burkov): Enable the refresher after the actual
		// implementation passes the public testing.
		sysRes, err = aghnet.NewSystemResolvers(0, nil)
		if err != nil {
			return err
		}

		localAddrs = sysRes.Get()
	}

	var ourAddrs []string
	ourAddrs, err = s.collectDNSIPAddrs()
	if err != nil {
		return err
	}

	// TODO(e.burkov): The approach of subtracting sets of strings
	// is not really applicable here since in case of listening on
	// all network interfaces we should check the whole interface's
	// network to cut off all the loopback addresses as well.
	localAddrs = stringsSetSubtract(localAddrs, ourAddrs)

	s.localResolvers, err = aghnet.NewMultiAddrExchanger(localAddrs, defaultLocalTimeout)
	if err != nil {
		return err
	}

	return nil
}

// Prepare the object
func (s *Server) Prepare(config *ServerConfig) error {
	// Initialize the server configuration
	// --
	if config != nil {
		s.conf = *config
		if s.conf.BlockingMode == "custom_ip" {
			if s.conf.BlockingIPv4 == nil || s.conf.BlockingIPv6 == nil {
				return fmt.Errorf("dns: invalid custom blocking IP address specified")
			}
		}
	}

	// Set default values in the case if nothing is configured
	// --
	s.initDefaultSettings()

	// Initialize IPSET configuration
	// --
	err := s.ipset.init(s.conf.IPSETList)
	if err != nil {
		if !errors.Is(err, os.ErrInvalid) && !errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot initialize ipset: %w", err)
		}

		// ipset cannot currently be initialized if the server was
		// installed from Snap or when the user or the binary doesn't
		// have the required permissions, or when the kernel doesn't
		// support netfilter.
		//
		// Log and go on.
		//
		// TODO(a.garipov): The Snap problem can probably be solved if
		// we add the netlink-connector interface plug.
		log.Error("cannot initialize ipset: %s", err)
	}

	// Prepare DNS servers settings
	// --
	err = s.prepareUpstreamSettings()
	if err != nil {
		return err
	}

	// Create DNS proxy configuration
	// --
	var proxyConfig proxy.Config
	proxyConfig, err = s.createProxyConfig()
	if err != nil {
		return err
	}

	// Prepare a DNS proxy instance that we use for internal DNS queries
	// --
	s.prepareIntlProxy()

	// Initialize DNS access module
	// --
	s.access = &accessCtx{}
	err = s.access.Init(s.conf.AllowedClients, s.conf.DisallowedClients, s.conf.BlockedHosts)
	if err != nil {
		return err
	}

	// Register web handlers if necessary
	// --
	if !webRegistered && s.conf.HTTPRegister != nil {
		webRegistered = true
		s.registerHandlers()
	}

	// Create the main DNS proxy instance
	// --
	s.dnsProxy = &proxy.Proxy{Config: proxyConfig}

	err = s.setupResolvers(s.conf.LocalPTRResolvers)
	if err != nil {
		return fmt.Errorf("setting up resolvers: %w", err)
	}

	return nil
}

// Stop stops the DNS server.
func (s *Server) Stop() error {
	s.Lock()
	defer s.Unlock()
	return s.stopLocked()
}

// stopLocked stops the DNS server without locking. For internal use only.
func (s *Server) stopLocked() error {
	if s.dnsProxy != nil {
		err := s.dnsProxy.Stop()
		if err != nil {
			return fmt.Errorf("could not stop the DNS server properly: %w", err)
		}
	}

	s.isRunning = false
	return nil
}

// IsRunning returns true if the DNS server is running
func (s *Server) IsRunning() bool {
	s.RLock()
	defer s.RUnlock()
	return s.isRunning
}

// Reconfigure applies the new configuration to the DNS server
func (s *Server) Reconfigure(config *ServerConfig) error {
	s.Lock()
	defer s.Unlock()

	log.Print("Start reconfiguring the server")
	err := s.stopLocked()
	if err != nil {
		return fmt.Errorf("could not reconfigure the server: %w", err)
	}

	// It seems that net.Listener.Close() doesn't close file descriptors right away.
	// We wait for some time and hope that this fd will be closed.
	time.Sleep(100 * time.Millisecond)

	err = s.Prepare(config)
	if err != nil {
		return fmt.Errorf("could not reconfigure the server: %w", err)
	}

	err = s.startLocked()
	if err != nil {
		return fmt.Errorf("could not reconfigure the server: %w", err)
	}

	return nil
}

// ServeHTTP is a HTTP handler method we use to provide DNS-over-HTTPS
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.RLock()
	p := s.dnsProxy
	s.RUnlock()
	if p != nil { // an attempt to protect against race in case we're here after Close() was called
		p.ServeHTTP(w, r)
	}
}

// IsBlockedIP - return TRUE if this client should be blocked
func (s *Server) IsBlockedIP(ip net.IP) (bool, string) {
	if ip == nil {
		return false, ""
	}

	return s.access.IsBlockedIP(ip)
}
