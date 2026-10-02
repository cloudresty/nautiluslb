package health

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// Options configures a Checker. Listener and Pool are metric/log labels.
type Options struct {
	Interval     time.Duration
	Timeout      time.Duration
	EjectionHold time.Duration
	Rise, Fall   int
	Jitter       float64
	Prober       Prober // nil: report-only (health type none)
	Recorder     metrics.Recorder
	Listener     string
	Pool         string
}

func (o Options) normalized() Options {

	if o.Recorder == nil {
		o.Recorder = metrics.NewNop()
	}
	if o.Rise < 1 {
		o.Rise = 1
	}
	if o.Fall < 1 {
		o.Fall = 1
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = o.Interval
	}
	if o.Jitter < 0 {
		o.Jitter = 0
	}
	if o.Jitter > 1 {
		o.Jitter = 1
	}

	return o

}

type loop struct {
	b      *backend.Backend
	cancel context.CancelFunc
}

// Checker runs one probe loop per backend of a pool.
type Checker struct {
	opts atomic.Pointer[Options]

	mu      sync.Mutex
	ctx     context.Context
	started bool
	stopped bool
	desired []*backend.Backend
	loops   map[string]*loop
	wg      sync.WaitGroup

	// test hooks
	now   func() time.Time
	rnd   func() float64
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewChecker returns a Checker; call Start before probing begins.
func NewChecker(opts Options) *Checker {

	c := &Checker{
		loops: make(map[string]*loop),
		now:   time.Now,
		rnd:   rand.Float64,
		sleep: sleepCtx,
	}
	o := opts.normalized()
	c.opts.Store(&o)

	return c

}

func sleepCtx(ctx context.Context, d time.Duration) bool {

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}

}

// Start enables probing under ctx. It is idempotent.
func (c *Checker) Start(ctx context.Context) {

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started || c.stopped {
		return
	}

	c.started = true
	c.ctx = ctx
	c.reconcileLocked()

}

// Reconcile starts loops for new backends, cancels loops of removed ones and
// leaves running loops untouched. Before Start the set is remembered and
// applied by Start.
func (c *Checker) Reconcile(backends []*backend.Backend) {

	c.mu.Lock()
	defer c.mu.Unlock()

	c.desired = append(c.desired[:0:0], backends...)
	c.reconcileLocked()

}

func (c *Checker) reconcileLocked() {

	if !c.started || c.stopped || c.ctx.Err() != nil {
		return
	}

	opts := c.opts.Load()

	current := make(map[string]*backend.Backend, len(c.desired))
	for _, b := range c.desired {
		current[b.Address()] = b
	}

	for addr, l := range c.loops {
		if cur, ok := current[addr]; !ok || cur != l.b {
			l.cancel()
			delete(c.loops, addr)
		}
	}

	for addr, b := range current {

		if _, running := c.loops[addr]; running {
			continue
		}

		ctx, cancel := context.WithCancel(c.ctx)
		c.loops[addr] = &loop{b: b, cancel: cancel}
		c.wg.Add(1)

		emit.Info.StructuredFields("Starting health check for backend",
			emit.ZString("listener", opts.Listener),
			emit.ZString("pool", opts.Pool),
			emit.ZString("backend", addr),
			emit.ZInt("interval_ms", int(opts.Interval/time.Millisecond)))

		go func() {
			defer c.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					emit.Error.StructuredFields("Recovered panic in health check",
						emit.ZString("backend", addr),
						emit.ZString("panic", fmt.Sprint(r)),
						emit.ZString("stack", string(debug.Stack())))
				}
			}()
			c.run(ctx, b)
		}()

	}

}

// Stop cancels every loop and waits for them to exit.
func (c *Checker) Stop() {

	c.mu.Lock()
	c.stopped = true
	for addr, l := range c.loops {
		l.cancel()
		delete(c.loops, addr)
	}
	c.mu.Unlock()

	c.wg.Wait()

}

// Update replaces the options; loops pick them up at their next tick.
func (c *Checker) Update(opts Options) {

	o := opts.normalized()
	c.opts.Store(&o)

}

// firstDelay is the de-phasing delay: uniform in [0, interval).
func (c *Checker) firstDelay(o *Options) time.Duration {
	return time.Duration(c.rnd() * float64(o.Interval))
}

// nextDelay is interval ± jitter*interval, uniform.
func (c *Checker) nextDelay(o *Options) time.Duration {

	d := float64(o.Interval) * (1 + (c.rnd()*2-1)*o.Jitter)
	if d < 0 {
		d = 0
	}

	return time.Duration(d)

}

func (c *Checker) run(ctx context.Context, b *backend.Backend) {

	addr := b.Address()
	var okRun, failRun int

	delay := c.firstDelay(c.opts.Load())

	for c.sleep(ctx, delay) {

		o := c.opts.Load()

		if o.Prober != nil {
			c.tick(ctx, o, b, addr, &okRun, &failRun)
		} else {
			c.recoverPassive(o, b, addr)
		}

		if ctx.Err() != nil {
			return
		}

		delay = c.nextDelay(c.opts.Load())

	}

}

// recoverPassive re-admits a passively ejected backend once its hold expired;
// used when there is no prober (health type none).
func (c *Checker) recoverPassive(o *Options, b *backend.Backend, addr string) {

	if b.IsHealthy() || c.now().Before(b.EjectedUntil()) {
		return
	}

	if b.SetHealthy(true, backend.CausePassive) {
		o.Recorder.BackendHealth(o.Listener, o.Pool, addr, true, "passive")
		emit.Info.StructuredFields("Backend marked healthy",
			emit.ZString("listener", o.Listener),
			emit.ZString("pool", o.Pool),
			emit.ZString("backend", addr))
	}

}

func (c *Checker) tick(ctx context.Context, o *Options, b *backend.Backend, addr string, okRun, failRun *int) {

	pctx, cancel := context.WithTimeout(ctx, o.Timeout)
	start := c.now()
	err := o.Prober.Probe(pctx, addr)
	cancel()
	d := c.now().Sub(start)

	// A probe interrupted by Stop/removal says nothing about the backend.
	if ctx.Err() != nil {
		return
	}

	if err == nil {
		o.Recorder.ProbeDone(o.Listener, o.Pool, "ok", d)
		*okRun++
		*failRun = 0
	} else {
		o.Recorder.ProbeDone(o.Listener, o.Pool, "fail", d)
		*failRun++
		*okRun = 0
	}

	switch {
	case b.IsHealthy() && err != nil && *failRun >= o.Fall:
		if b.SetHealthy(false, backend.CauseProbe) {
			o.Recorder.BackendHealth(o.Listener, o.Pool, addr, false, "probe")
			emit.Info.StructuredFields("Backend marked unhealthy",
				emit.ZString("listener", o.Listener),
				emit.ZString("pool", o.Pool),
				emit.ZString("backend", addr),
				emit.ZString("error", err.Error()))
		}
		*failRun, *okRun = 0, 0
	case !b.IsHealthy() && err == nil && *okRun >= o.Rise && !c.now().Before(b.EjectedUntil()):
		if b.SetHealthy(true, backend.CauseProbe) {
			o.Recorder.BackendHealth(o.Listener, o.Pool, addr, true, "probe")
			emit.Info.StructuredFields("Backend marked healthy",
				emit.ZString("listener", o.Listener),
				emit.ZString("pool", o.Pool),
				emit.ZString("backend", addr))
		}
		*failRun, *okRun = 0, 0
	}

}
