package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cloudresty/emit"

	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/pool"
)

type poolUpdate struct {
	p    *pool.Pool
	spec config.PoolSpec
}

// Reload applies cfg (plan D.3). It never applies partially: everything that
// can fail (listener and pool construction, ACL and CIDR parsing, binding,
// discovery.Rebind) happens before the first irreversible step, and any
// failure there closes what this call bound, stops the pools it created and
// leaves the running state untouched.
//
// Order: diff by configuration name; build new pools; dry-run the in-place
// updates; bind every added/replaced listener; start the new pools and
// register them with the discovery sink; discovery.Rebind (the last step that
// can fail, and it is atomic). Only then the commit: pool.Update and
// Listener.Update on updated configurations, Serve the new listeners, and
// finally drain removed/replaced listeners in the background (drain.timeout)
// before stopping their orphaned pools. Unchanged configurations are never
// touched: same listener, same pools, same health state.
//
// Deviation from the plan text: new pools are started before Rebind (not
// after), because Rebind's reconcile sends endpoints to them at once and
// discovery will not resend an unchanged set; starting is reversible, Rebind
// is not worth rolling back.
//
// Replaced configurations keep their pool objects for pool keys that exist in
// both versions (pool.Update), so endpoints and health survive an address move
// and discovery's "unchanged" cache stays valid.
func (r *Runtime) Reload(cfg *config.Config) (Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateRunning {
		r.rec.ConfigReload("rejected")
		return Summary{}, ErrNotRunning
	}
	sum, err := r.reload(cfg)
	if err != nil {
		r.rec.ConfigReload("rejected")
		emit.Error.StructuredFields("Configuration reload rejected, keeping the running configuration",
			emit.ZString("error", err.Error()))
		return Summary{}, err
	}
	changed := len(sum.Added)+len(sum.Removed)+len(sum.Updated)+len(sum.Replaced) > 0
	if changed {
		r.rec.ConfigReload("applied")
	} else {
		r.rec.ConfigReload("unchanged")
	}
	if len(sum.Ignored) > 0 {
		emit.Warn.StructuredFields("Settings changed but are not hot-reloadable, restart to apply",
			emit.ZString("fields", strings.Join(sum.Ignored, ",")))
	}
	return sum, nil
}

func (r *Runtime) reload(cfg *config.Config) (Summary, error) {
	var sum Summary
	if cfg == nil {
		return sum, errors.New("nil configuration")
	}
	sum.Ignored = diffSettings(r.settings, cfg.Settings)

	// 1. Diff by name.
	var added, updated, replaced []config.Configuration
	newNames := make(map[string]bool, len(cfg.Configurations))
	for _, c := range cfg.Configurations {
		newNames[c.Name] = true
		old, ok := r.entries[c.Name]
		switch {
		case !ok:
			added = append(added, c)
			sum.Added = append(sum.Added, c.Name)
		case reflect.DeepEqual(old.cfg, c):
			sum.Unchanged = append(sum.Unchanged, c.Name)
		case old.cfg.ListenerAddress == c.ListenerAddress && normProto(old.cfg.Protocol) == normProto(c.Protocol):
			updated = append(updated, c)
			sum.Updated = append(sum.Updated, c.Name)
		default:
			replaced = append(replaced, c)
			sum.Replaced = append(sum.Replaced, c.Name)
		}
	}
	var removed []*entry
	for name, e := range r.entries {
		if !newNames[name] {
			removed = append(removed, e)
			sum.Removed = append(sum.Removed, name)
		}
	}

	if len(added)+len(updated)+len(replaced)+len(removed) == 0 {
		return sum, nil // nothing to apply: no pool, listener or discovery call
	}

	// 2. Pools: reuse by key, create the missing ones, dry-run the algorithm.
	r.poolsMu.RLock()
	view := make(map[string]*pool.Pool, len(r.pools))
	for k, p := range r.pools {
		view[k] = p
	}
	r.poolsMu.RUnlock()

	newPools := map[string]*pool.Pool{}
	var updates []poolUpdate
	for _, c := range append(append(append([]config.Configuration{}, added...), updated...), replaced...) {
		for _, spec := range c.Pools() {
			if p, ok := view[spec.Key]; ok {
				if _, err := balancer.New(spec.Balancer.Algorithm, balancer.Options{SlowStart: spec.Balancer.SlowStart.Std()}); err != nil {
					return Summary{}, fmt.Errorf("configuration %q: pool %q: %w", c.Name, spec.Key, err)
				}
				updates = append(updates, poolUpdate{p, spec})
				continue
			}
			p, err := pool.New(pool.Options{Spec: spec, Listener: c.Name, Recorder: r.rec})
			if err != nil {
				return Summary{}, fmt.Errorf("configuration %q: pool %q: %w", c.Name, spec.Key, err)
			}
			newPools[spec.Key] = p
			view[spec.Key] = p
		}
	}

	// Orphans: pools of the old version whose key is not in the new one.
	nextKeys := map[string]bool{}
	for _, c := range cfg.Configurations {
		for _, k := range poolKeys(c) {
			nextKeys[k] = true
		}
	}
	orphansOf := func(e *entry) []*pool.Pool {
		var out []*pool.Pool
		for _, k := range e.keys {
			if !nextKeys[k] {
				out = append(out, view[k])
			}
		}
		return out
	}

	// 3. Listeners: real ones for added/replaced, throwaway ones that dry-run
	// the in-place updates (ACL, CIDRs, SNI router) so Update cannot fail later.
	var fresh []*entry // bound new listeners, to Serve
	discardFresh := func() {
		for _, e := range fresh {
			e.discard()
		}
	}
	for _, c := range updated {
		probe, err := r.newEntry(c, view)
		if err != nil {
			return Summary{}, err
		}
		probe.discard()
	}
	freshBy := map[string]*entry{}
	for _, c := range append(append([]config.Configuration{}, added...), replaced...) {
		e, err := r.newEntry(c, view)
		if err != nil {
			discardFresh()
			return Summary{}, err
		}
		fresh = append(fresh, e)
		freshBy[c.Name] = e
	}

	// 4. Bind first. Nothing has been applied yet.
	for _, e := range fresh {
		if err := e.listen(); err != nil {
			discardFresh()
			return Summary{}, fmt.Errorf("configuration %q: %w", e.cfg.Name, err)
		}
	}
	r.trace("reload.bound")

	// 5. New pools live and visible to the sink, then the atomic Rebind.
	for _, p := range newPools {
		p.Start(r.runCtx)
	}
	r.poolsMu.Lock()
	for k, p := range newPools {
		r.pools[k] = p
	}
	r.poolsMu.Unlock()
	if err := r.disc.Rebind(r.allSpecs(cfg.Configurations)); err != nil {
		r.poolsMu.Lock()
		for k := range newPools {
			delete(r.pools, k)
		}
		r.poolsMu.Unlock()
		for _, p := range newPools {
			p.Stop()
		}
		discardFresh()
		return Summary{}, fmt.Errorf("rebinding discovery: %w", err)
	}

	// 6. Commit. Nothing below is expected to fail.
	for _, u := range updates {
		if err := u.p.Update(u.spec); err != nil {
			emit.Error.StructuredFields("Pool update failed after validation",
				emit.ZString("pool", u.spec.Key), emit.ZString("error", err.Error()))
		}
	}
	r.poolsMu.RLock()
	all := make(map[string]*pool.Pool, len(r.pools))
	for k, p := range r.pools {
		all[k] = p
	}
	r.poolsMu.RUnlock()

	var stopNow []*pool.Pool // orphans of in-place updates: no listener routes to them any more
	for _, c := range updated {
		e := r.entries[c.Name]
		stopNow = append(stopNow, orphansOf(e)...)
		if err := e.update(c, all); err != nil {
			emit.Error.StructuredFields("Listener update failed after validation",
				emit.ZString("listener", c.Name), emit.ZString("error", err.Error()))
			continue
		}
		e.cfg, e.keys = c, poolKeys(c)
	}

	var drainOld []*entry
	var drainOrphans []*pool.Pool
	for _, e := range removed {
		drainOld = append(drainOld, e)
		drainOrphans = append(drainOrphans, orphansOf(e)...)
		delete(r.entries, e.cfg.Name)
	}
	for _, c := range replaced {
		old := r.entries[c.Name]
		drainOld = append(drainOld, old)
		drainOrphans = append(drainOrphans, orphansOf(old)...)
	}
	for name, e := range freshBy {
		r.entries[name] = e
		r.serveLocked(e)
	}

	// Orphans leave the sink map now; the pools themselves stop once nothing
	// can still be routing through them.
	r.poolsMu.Lock()
	for _, p := range stopNow {
		delete(r.pools, p.Key())
	}
	for _, p := range drainOrphans {
		delete(r.pools, p.Key())
	}
	r.poolsMu.Unlock()
	for _, p := range stopNow {
		p.Stop()
	}
	if len(drainOld) > 0 || len(drainOrphans) > 0 {
		r.drainInBackground(drainOld, drainOrphans)
	}
	return sum, nil
}

// drainInBackground drains old listeners within drain.timeout, then stops
// their orphaned pools. Shutdown waits for it (and forces it when its own
// context ends).
func (r *Runtime) drainInBackground(old []*entry, orphans []*pool.Pool) {
	r.bg.Add(1)
	go func() {
		defer r.bg.Done()
		ctx, cancel := context.WithTimeout(r.bgCtx, r.drainTimeout())
		defer cancel()
		var wg sync.WaitGroup
		for _, e := range old {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.trace("reload.drain.begin")
				if n := e.drain(ctx); n > 0 {
					r.bgForced.Add(int64(n))
					emit.Warn.StructuredFields("Force-closed connections of a retired listener",
						emit.ZString("listener", e.cfg.Name), emit.ZInt("forced", n))
				}
			}()
		}
		wg.Wait()
		for _, p := range orphans {
			p.Stop()
		}
		r.trace("reload.drain.end")
	}()
}

// WaitDrained blocks until background drains started by Reload have finished
// or d elapses (tests and diagnostics).
func (r *Runtime) WaitDrained(d time.Duration) bool {
	done := make(chan struct{})
	go func() { r.bg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// diffSettings lists the dotted yaml paths whose value differs.
func diffSettings(a, b config.Settings) []string {
	var out []string
	walkDiff("settings", reflect.ValueOf(a), reflect.ValueOf(b), &out)
	return out
}

func walkDiff(path string, a, b reflect.Value, out *[]string) {
	if a.Kind() == reflect.Struct {
		t := a.Type()
		for i := 0; i < a.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name == "" {
				name = strings.ToLower(f.Name[:1]) + f.Name[1:]
			}
			walkDiff(path+"."+name, a.Field(i), b.Field(i), out)
		}
		return
	}
	if !reflect.DeepEqual(a.Interface(), b.Interface()) {
		*out = append(*out, path)
	}
}
