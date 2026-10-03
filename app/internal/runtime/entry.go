package runtime

import (
	"context"
	"fmt"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/pool"
	"github.com/cloudresty/nautiluslb/internal/tcpproxy"
	"github.com/cloudresty/nautiluslb/internal/udpproxy"
)

// entry is one configured listener: a tcpproxy or udpproxy Listener plus the
// configuration it was built from and the keys of the pools it routes to.
type entry struct {
	cfg  config.Configuration
	keys []string // pool keys of cfg
	tcp  *tcpproxy.Listener
	udp  *udpproxy.Listener
}

func isUDP(p config.Protocol) bool { return p == config.ProtocolUDP }

// normProto treats an empty protocol as tcp, like the proxies do.
func normProto(p config.Protocol) config.Protocol {
	if p == "" {
		return config.ProtocolTCP
	}
	return p
}

func poolKeys(cfg config.Configuration) []string {
	specs := cfg.Pools()
	keys := make([]string, 0, len(specs))
	for _, s := range specs {
		keys = append(keys, s.Key)
	}
	return keys
}

// subset returns the pools of keys, looked up in all (missing keys are skipped).
func subset(all map[string]*pool.Pool, keys []string) map[string]*pool.Pool {
	out := make(map[string]*pool.Pool, len(keys))
	for _, k := range keys {
		if p, ok := all[k]; ok {
			out[k] = p
		}
	}
	return out
}

func toTCP(in map[string]*pool.Pool) map[string]tcpproxy.Pool {
	out := make(map[string]tcpproxy.Pool, len(in))
	for k, p := range in {
		out[k] = p
	}
	return out
}

func toUDP(in map[string]*pool.Pool) map[string]udpproxy.Pool {
	out := make(map[string]udpproxy.Pool, len(in))
	for k, p := range in {
		out[k] = p
	}
	return out
}

// newEntry builds (binds nothing) the listener of cfg over pools.
func (r *Runtime) newEntry(cfg config.Configuration, pools map[string]*pool.Pool) (*entry, error) {
	e := &entry{cfg: cfg, keys: poolKeys(cfg)}
	own := subset(pools, e.keys)
	var err error
	if isUDP(cfg.Protocol) {
		e.udp, err = udpproxy.New(udpproxy.Options{
			Config: cfg, Pools: toUDP(own), Global: r.gate,
			Recorder: r.rec, AccessLog: r.alog, Dial: r.opts.Dial,
		})
	} else {
		e.tcp, err = tcpproxy.New(tcpproxy.Options{
			Config: cfg, Pools: toTCP(own), Global: r.gate,
			Recorder: r.rec, AccessLog: r.alog, Watchdog: r.wd, Dial: r.opts.Dial,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("configuration %q: %w", cfg.Name, err)
	}
	return e, nil
}

func (e *entry) listen() error {
	if e.udp != nil {
		return e.udp.Listen()
	}
	return e.tcp.Listen()
}

func (e *entry) serve() {
	if e.udp != nil {
		e.udp.Serve()
		return
	}
	e.tcp.Serve()
}

func (e *entry) drain(ctx context.Context) int {
	if e.udp != nil {
		return e.udp.Drain(ctx)
	}
	return e.tcp.Drain(ctx)
}

// discard releases a listener that never served (bound or not).
func (e *entry) discard() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.drain(ctx)
}

func (e *entry) update(cfg config.Configuration, pools map[string]*pool.Pool) error {
	own := subset(pools, poolKeys(cfg))
	if e.udp != nil {
		return e.udp.Update(cfg, toUDP(own))
	}
	return e.tcp.Update(cfg, toTCP(own))
}

// addr is the bound address once listening, else the configured one.
func (e *entry) addr() string {
	if e.udp != nil {
		if a := e.udp.LocalAddr(); a != nil {
			return a.String()
		}
		return e.udp.Address()
	}
	return e.tcp.Address()
}
