package reverseproxy

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(SRVUpstreams{})
	caddy.RegisterModule(AUpstreams{})
}

// SRVUpstreams provides upstreams from SRV lookups.
// The lookup DNS name can be configured either by
// its individual parts (that is, specifying the
// service, protocol, and name separately) to form
// the standard "_service._proto.name" domain, or
// the domain can be specified directly in name by
// leaving service and proto empty. See RFC 2782.
//
// Lookups are cached and refreshed at the configured
// refresh interval.
//
// Returned upstreams are sorted by priority and weight.
type SRVUpstreams struct {
	// The interval at which to refresh the SRV lookup.
	// Results are cached between lookups. Default: 1m
	Refresh time.Duration `json:"refresh,omitempty"`

	// The service label.
	Service string `json:"service,omitempty"`

	// The protocol label; either tcp or udp.
	Proto string `json:"proto,omitempty"`

	// The name label; or, if service and proto are
	// empty, the entire domain name to look up.
	Name string `json:"name,omitempty"`

	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (SRVUpstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.srv",
		New: func() caddy.Module { return new(SRVUpstreams) },
	}
}

// String returns the RFC 2782 representation of the SRV domain.
func (su SRVUpstreams) String() string {
	return fmt.Sprintf("_%s._%s.%s", su.Service, su.Proto, su.Name)
}

func (su *SRVUpstreams) Provision(ctx caddy.Context) error {
	su.logger = ctx.Logger(su)
	if su.Proto != "tcp" && su.Proto != "udp" {
		return fmt.Errorf("invalid proto '%s'", su.Proto)
	}
	if su.Refresh == 0 {
		su.Refresh = time.Minute
	}
	return nil
}

func (su SRVUpstreams) GetUpstreams(r *http.Request) ([]*Upstream, error) {
	suStr := su.String()

	// first, use a cheap read-lock to return a cached result quickly
	srvsMu.RLock()
	cached := srvs[suStr]
	srvsMu.RUnlock()
	if cached.isFresh() {
		return cached.upstreams, nil
	}

	// otherwise, obtain a write-lock to update the cached value
	srvsMu.Lock()
	defer srvsMu.Unlock()

	// check to see if it's still stale, since we're now in a different
	// lock from when we first checked freshness; another goroutine might
	// have refreshed it in the meantime before we re-obtained our lock
	cached = srvs[suStr]
	if cached.isFresh() {
		return cached.upstreams, nil
	}

	// prepare parameters and perform the SRV lookup
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	service := repl.ReplaceAll(su.Service, "")
	proto := repl.ReplaceAll(su.Proto, "")
	name := repl.ReplaceAll(su.Name, "")

	su.logger.Debug("refreshing SRV upstreams",
		zap.String("service", service),
		zap.String("proto", proto),
		zap.String("name", name))

	_, records, err := net.DefaultResolver.LookupSRV(r.Context(), service, proto, name)
	if err != nil {
		// From LookupSRV docs: "If the response contains invalid names, those records are filtered
		// out and an error will be returned alongside the the remaining results, if any." Thus, we
		// only return an error if no records were also returned.
		if len(records) == 0 {
			return nil, err
		}
		su.logger.Warn("SRV records filtered", zap.Error(err))
	}

	upstreams := make([]*Upstream, len(records))
	for i, rec := range records {
		su.logger.Debug("discovered SRV record",
			zap.String("target", rec.Target),
			zap.Uint16("port", rec.Port),
			zap.Uint16("priority", rec.Priority),
			zap.Uint16("weight", rec.Weight))
		addr := net.JoinHostPort(rec.Target, strconv.Itoa(int(rec.Port)))
		upstreams[i] = &Upstream{
			Dial: net.JoinHostPort(rec.Target, addr),
		}
	}

	// before adding a new one to the cache (as opposed to replacing stale one), make room if cache is full
	if cached.freshness.IsZero() && len(srvs) >= 100 {
		for randomKey := range srvs {
			delete(srvs, randomKey)
			break
		}
	}

	srvs[suStr] = srvLookup{
		srvUpstreams: su,
		freshness:    time.Now(),
		upstreams:    upstreams,
	}

	return upstreams, nil
}

type srvLookup struct {
	srvUpstreams SRVUpstreams
	freshness    time.Time
	upstreams    []*Upstream
}

func (sl srvLookup) isFresh() bool {
	return time.Since(sl.freshness) < sl.srvUpstreams.Refresh
}

var (
	srvs   = make(map[string]srvLookup)
	srvsMu sync.RWMutex
)

// AUpstreams provides upstreams from A/AAAA lookups.
// Results are cached and refreshed at the configured
// refresh interval.
type AUpstreams struct {
	// The domain name to look up.
	Name string `json:"name,omitempty"`

	// The port to use with the upstreams. Default: 80
	Port string `json:"port,omitempty"`

	// The interval at which to refresh the SRV lookup.
	// Results are cached between lookups. Default: 1m
	Refresh time.Duration `json:"refresh,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (AUpstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.a",
		New: func() caddy.Module { return new(AUpstreams) },
	}
}

func (au AUpstreams) String() string { return au.Name }

func (au *AUpstreams) Provision(_ caddy.Context) error {
	if au.Refresh == 0 {
		au.Refresh = time.Minute
	}
	if au.Port == "" {
		au.Port = "80"
	}
	return nil
}

func (au AUpstreams) GetUpstreams(r *http.Request) ([]*Upstream, error) {
	auStr := au.String()

	// first, use a cheap read-lock to return a cached result quickly
	aAaaaMu.RLock()
	cached := aAaaa[auStr]
	aAaaaMu.RUnlock()
	if cached.isFresh() {
		return cached.upstreams, nil
	}

	// otherwise, obtain a write-lock to update the cached value
	aAaaaMu.Lock()
	defer aAaaaMu.Unlock()

	// check to see if it's still stale, since we're now in a different
	// lock from when we first checked freshness; another goroutine might
	// have refreshed it in the meantime before we re-obtained our lock
	cached = aAaaa[auStr]
	if cached.isFresh() {
		return cached.upstreams, nil
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	name := repl.ReplaceAll(au.Name, "")
	port := repl.ReplaceAll(au.Port, "")

	ips, err := net.DefaultResolver.LookupIPAddr(r.Context(), name)
	if err != nil {
		return nil, err
	}

	upstreams := make([]*Upstream, len(ips))
	for i, ip := range ips {
		upstreams[i] = &Upstream{
			Dial: net.JoinHostPort(ip.String(), port),
		}
	}

	// before adding a new one to the cache (as opposed to replacing stale one), make room if cache is full
	if cached.freshness.IsZero() && len(srvs) >= 100 {
		for randomKey := range aAaaa {
			delete(aAaaa, randomKey)
			break
		}
	}

	aAaaa[auStr] = aLookup{
		aUpstreams: au,
		freshness:  time.Now(),
		upstreams:  upstreams,
	}

	return upstreams, nil
}

type aLookup struct {
	aUpstreams AUpstreams
	freshness  time.Time
	upstreams  []*Upstream
}

func (al aLookup) isFresh() bool {
	return time.Since(al.freshness) < al.aUpstreams.Refresh
}

var (
	aAaaa   = make(map[string]aLookup)
	aAaaaMu sync.RWMutex
)

// Interface guards
var (
	_ caddy.Provisioner = (*SRVUpstreams)(nil)
	_ UpstreamSource    = (*SRVUpstreams)(nil)
	_ caddy.Provisioner = (*AUpstreams)(nil)
	_ UpstreamSource    = (*AUpstreams)(nil)
)
