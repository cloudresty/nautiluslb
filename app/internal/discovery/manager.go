package discovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/cloudresty/emit"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// ErrNotSynced is returned by Start when the informers did not complete their
// first list within the sync timeout. Discovery keeps running regardless.
var ErrNotSynced = errors.New("discovery: informers not synced")

const (
	// maxDebounceWait bounds how long a stream of events can postpone a
	// reconcile.
	maxDebounceWait = 2 * time.Second
	// defaultSyncTimeout is how long Start waits for the first sync.
	defaultSyncTimeout = 60 * time.Second
	// syncPollInterval is how often Start checks the informers' sync state.
	syncPollInterval = 10 * time.Millisecond
)

// Sink receives the endpoints of a pool whenever they change.
type Sink interface {
	SetEndpoints(poolKey string, eps []backend.Endpoint)
}

// Options configures a Manager.
type Options struct {
	Settings config.DiscoverySettings
	Recorder metrics.Recorder
	Sink     Sink
	// SyncTimeout is how long Start waits for the first sync; 0 means 60s.
	SyncTimeout time.Duration
}

// nsFactory is the informer set of one namespace ("" = cluster-wide).
type nsFactory struct {
	ns       string
	factory  informers.SharedInformerFactory
	services cache.SharedIndexInformer
	slices   cache.SharedIndexInformer
	svcList  func() ([]*corev1.Service, error)
	sliceLst func() ([]*discoveryv1.EndpointSlice, error)
	cancel   context.CancelFunc
}

func (f *nsFactory) synced() bool { return f.services.HasSynced() && f.slices.HasSynced() }

// Manager watches Services, EndpointSlices and Nodes and feeds the endpoints
// of every pool to a Sink. Events only signal a debounced reconcile that
// recomputes everything from the informers' caches.
type Manager struct {
	client kubernetes.Interface
	opts   Options
	warned *warnTracker

	// syncTimeout is how long Start waits for the first sync (tests shorten it).
	syncTimeout time.Duration

	trigger chan struct{}

	// syncedCh is closed (once, by syncOnce) the first time every store the
	// current specs need has synced and a reconcile has applied it.
	syncedCh   chan struct{}
	syncedOnce sync.Once

	mu      sync.Mutex
	started bool
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	specs   []config.PoolSpec
	facs    map[string]*nsFactory
	nodes   *nodeFactory
	wg      sync.WaitGroup // loop and sync watchers

	// factoryHook replaces newNSFactory when set (tests inject failures).
	factoryHook func(ns string) (*nsFactory, error)

	recMu   sync.Mutex // serialises reconciles and guards applied
	applied map[string][]backend.Endpoint
}

type nodeFactory struct {
	factory informers.SharedInformerFactory
	nodes   cache.SharedIndexInformer
	list    func() ([]*corev1.Node, error)
	cancel  context.CancelFunc
}

// NewManager returns a Manager that is idle until Start.
func NewManager(client kubernetes.Interface, opts Options) *Manager {
	if opts.Recorder == nil {
		opts.Recorder = metrics.NewNop()
	}
	timeout := opts.SyncTimeout
	if timeout <= 0 {
		timeout = defaultSyncTimeout
	}
	return &Manager{
		client:      client,
		opts:        opts,
		warned:      newWarnTracker(),
		syncTimeout: timeout,
		syncedCh:    make(chan struct{}),
		trigger:     make(chan struct{}, 1),
		facs:        make(map[string]*nsFactory),
		applied:     make(map[string][]backend.Endpoint),
	}
}

// Synced returns a channel closed, once, when every store the current specs
// need has synced for the first time and a reconcile has applied it. It is
// closed by Start's wait or by the first later reconcile that sees everything
// synced, so it also fires after Start returned ErrNotSynced. It is never
// re-opened: a later Rebind that adds a not-yet-synced namespace does not
// un-close it.
func (m *Manager) Synced() <-chan struct{} { return m.syncedCh }

func (m *Manager) markSynced() { m.syncedOnce.Do(func() { close(m.syncedCh) }) }

// signal requests a reconcile; requests made while one is pending coalesce.
func (m *Manager) signal() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Manager) handler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { m.signal() },
		UpdateFunc: func(_, _ any) { m.signal() },
		DeleteFunc: func(any) { m.signal() },
	}
}

// namespacesOf returns the distinct namespaces the specs need, "" meaning
// cluster-wide. A scoped spec never needs the cluster-wide factory.
func namespacesOf(specs []config.PoolSpec) []string {
	set := map[string]bool{}
	for _, s := range specs {
		for _, ns := range s.Namespaces {
			set[ns] = true
		}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// newNSFactory registers and starts the Services and EndpointSlices informers
// of one namespace. The caller holds m.mu with m.ctx set.
func (m *Manager) newNSFactory(ns string) (*nsFactory, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(m.client, m.opts.Settings.ResyncPeriod.Std(), informers.WithNamespace(ns))
	svcs := factory.Core().V1().Services()
	sls := factory.Discovery().V1().EndpointSlices()
	f := &nsFactory{
		ns:       ns,
		factory:  factory,
		services: svcs.Informer(),
		slices:   sls.Informer(),
		svcList:  func() ([]*corev1.Service, error) { return svcs.Lister().List(labels.Everything()) },
		sliceLst: func() ([]*discoveryv1.EndpointSlice, error) { return sls.Lister().List(labels.Everything()) },
	}
	for _, inf := range []cache.SharedIndexInformer{f.services, f.slices} {
		if _, err := inf.AddEventHandler(m.handler()); err != nil {
			return nil, fmt.Errorf("adding event handler for namespace %q: %w", ns, err)
		}
	}
	ctx, cancel := context.WithCancel(m.ctx)
	f.cancel = cancel
	factory.Start(ctx.Done())
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// Informers fire no event for an empty first list; signal once synced.
		if cache.WaitForCacheSync(ctx.Done(), f.services.HasSynced, f.slices.HasSynced) {
			m.signal()
		}
	}()
	return f, nil
}

func (m *Manager) newNodeFactory() (*nodeFactory, error) {
	factory := informers.NewSharedInformerFactory(m.client, m.opts.Settings.ResyncPeriod.Std())
	nodes := factory.Core().V1().Nodes()
	n := &nodeFactory{
		factory: factory,
		nodes:   nodes.Informer(),
		list:    func() ([]*corev1.Node, error) { return nodes.Lister().List(labels.Everything()) },
	}
	if _, err := n.nodes.AddEventHandler(m.handler()); err != nil {
		return nil, fmt.Errorf("adding node event handler: %w", err)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	n.cancel = cancel
	factory.Start(ctx.Done())
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if cache.WaitForCacheSync(ctx.Done(), n.nodes.HasSynced) {
			m.signal()
		}
	}()
	return n, nil
}

// Start creates one informer factory per distinct namespace the specs need
// (the cluster-wide factory only if some spec is cluster-wide) plus one for
// Nodes, then waits for the first sync. It returns ErrNotSynced after the sync
// timeout, or ctx.Err() if ctx ends first; in both cases discovery keeps
// running until Stop, and pools whose stores have not synced are left alone.
// The Manager's lifetime is Stop, not ctx.
func (m *Manager) Start(ctx context.Context, specs []config.PoolSpec) error {
	m.mu.Lock()
	if m.started || m.stopped {
		m.mu.Unlock()
		return errors.New("discovery: manager already started")
	}
	m.started = true
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.specs = slices.Clone(specs)
	for _, ns := range namespacesOf(specs) {
		f, err := m.newNSFactory(ns) //nolint:contextcheck // factories derive from m.ctx (set just above), not the caller ctx; Start ctx only bounds the initial sync
		if err != nil {
			m.mu.Unlock()
			m.Stop()
			return err
		}
		m.facs[ns] = f
	}
	n, err := m.newNodeFactory() //nolint:contextcheck // factories derive from m.ctx (set just above), not the caller ctx; Start ctx only bounds the initial sync
	if err != nil {
		m.mu.Unlock()
		m.Stop()
		return err
	}
	m.nodes = n
	m.wg.Add(1)
	go m.loop()
	m.mu.Unlock()

	deadline := time.NewTimer(m.syncTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(syncPollInterval)
	defer tick.Stop()
	for !m.allSynced() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			emit.Warn.Msg("Discovery informers did not sync in time, pools of unsynced stores keep their current endpoints")
			return ErrNotSynced
		case <-tick.C:
		}
	}
	m.reconcile()
	m.markSynced()
	return nil
}

func (m *Manager) allSynced() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.facs {
		if !f.synced() {
			return false
		}
	}
	return m.nodes != nil && m.nodes.nodes.HasSynced()
}

// Rebind swaps the pool set (hot reload): it starts the factories of new
// namespaces, stops those no longer needed and recomputes. Before Start it only
// records the specs. It is atomic: if any new factory fails to start, the ones
// already started are stopped, the previous specs and factories stay in place
// (reconcile keeps serving the old pools) and the error is returned.
func (m *Manager) Rebind(specs []config.PoolSpec) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	if !m.started {
		m.specs = slices.Clone(specs)
		m.mu.Unlock()
		return nil
	}
	want := namespacesOf(specs)
	added := make(map[string]*nsFactory)
	for _, ns := range want {
		if _, ok := m.facs[ns]; ok {
			continue
		}
		f, err := m.makeFactory(ns)
		if err != nil {
			m.mu.Unlock()
			for _, a := range added {
				a.cancel()
				a.factory.Shutdown()
			}
			return fmt.Errorf("rebinding discovery, keeping previous pools: %w", err)
		}
		added[ns] = f
	}
	oldKeys := make(map[string]bool, len(m.specs))
	for _, sp := range m.specs {
		oldKeys[sp.Key] = true
	}
	newKeys := make(map[string]bool, len(specs))
	for _, sp := range specs {
		newKeys[sp.Key] = true
	}
	m.specs = slices.Clone(specs)
	maps.Copy(m.facs, added)
	var removed []*nsFactory
	for ns, f := range m.facs {
		if !slices.Contains(want, ns) {
			removed = append(removed, f)
			delete(m.facs, ns)
		}
	}
	m.mu.Unlock()

	// Forget what was last sent for keys that vanished or are new, so a key
	// removed and re-added within one debounce window (the pool object is new)
	// is always sent again. Done after the specs swap, under recMu, so a
	// reconcile cannot re-record a stale entry for it.
	m.recMu.Lock()
	for k := range m.applied {
		if !newKeys[k] {
			delete(m.applied, k)
		}
	}
	for k := range newKeys {
		if !oldKeys[k] {
			delete(m.applied, k)
		}
	}
	m.recMu.Unlock()

	for _, f := range removed {
		f.cancel()
		f.factory.Shutdown()
	}
	m.signal()
	return nil
}

func (m *Manager) makeFactory(ns string) (*nsFactory, error) {
	if m.factoryHook != nil {
		return m.factoryHook(ns)
	}
	return m.newNSFactory(ns)
}

// Stop stops every informer and goroutine and waits for them. It is safe to
// call more than once, and before Start.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	cancel := m.cancel
	facs := m.facs
	nodes := m.nodes
	m.facs = make(map[string]*nsFactory)
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, f := range facs {
		f.cancel()
		f.factory.Shutdown()
	}
	if nodes != nil {
		nodes.cancel()
		nodes.factory.Shutdown()
	}
	m.wg.Wait()
}

// loop turns event signals into reconciles: it waits for a quiet period of
// Settings.Debounce after the last event, but never longer than maxDebounceWait
// after the first.
func (m *Manager) loop() {
	defer m.wg.Done()
	quiet := m.opts.Settings.Debounce.Std()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.trigger:
		}
		first := time.Now()
		timer := time.NewTimer(quiet)
	wait:
		for {
			select {
			case <-m.ctx.Done():
				timer.Stop()
				return
			case <-m.trigger:
				left := maxDebounceWait - time.Since(first)
				if left <= 0 {
					break wait
				}
				timer.Reset(min(quiet, left))
			case <-timer.C:
				break wait
			}
		}
		timer.Stop()
		m.reconcile()
	}
}

// reconcile recomputes every pool from the caches and hands changed pools to
// the sink. A pool whose namespace factories (or, for node-fed pools, the Nodes
// informer) have not synced is skipped, never wiped.
func (m *Manager) reconcile() {
	m.recMu.Lock()
	defer m.recMu.Unlock()

	begin := time.Now()

	m.mu.Lock()
	if m.stopped || !m.started {
		m.mu.Unlock()
		return
	}
	specs := slices.Clone(m.specs)
	facs := make(map[string]*nsFactory, len(m.facs))
	for ns, f := range m.facs {
		facs[ns] = f
	}
	nodes := m.nodes
	m.mu.Unlock()

	nodesSynced := nodes.nodes.HasSynced()
	svcsSynced, slicesSynced := true, true
	for _, f := range facs {
		svcsSynced = svcsSynced && f.services.HasSynced()
		slicesSynced = slicesSynced && f.slices.HasSynced()
	}
	m.opts.Recorder.InformerSynced("services", svcsSynced)
	m.opts.Recorder.InformerSynced("endpointslices", slicesSynced)
	m.opts.Recorder.InformerSynced("nodes", nodesSynced)

	var nodeList []*corev1.Node
	if nodesSynced {
		var err error
		if nodeList, err = nodes.list(); err != nil {
			nodesSynced = false
			emit.Error.StructuredFields("Failed to read node cache", emit.ZString("error", err.Error()))
		}
	}
	nodeSet := eligibleNodes(nodeList, m.opts.Settings.Nodes)

	type nsData struct {
		svcs   []*corev1.Service
		slices []*discoveryv1.EndpointSlice
		ok     bool
	}
	data := make(map[string]nsData, len(facs))
	for ns, f := range facs {
		if !f.synced() {
			continue
		}
		svcs, err1 := f.svcList()
		sls, err2 := f.sliceLst()
		if err1 != nil || err2 != nil {
			continue
		}
		sortServices(svcs)
		data[ns] = nsData{svcs: svcs, slices: sls, ok: true}
	}

	m.warned.beginPass()
	allSynced := nodesSynced
	var allSvcs []*corev1.Service
	seen := make(map[string]bool)
	for _, d := range data {
		for _, s := range d.svcs {
			if k := s.Namespace + "/" + s.Name; !seen[k] {
				seen[k] = true
				allSvcs = append(allSvcs, s)
			}
		}
	}
	for ns := range facs {
		if !data[ns].ok {
			allSynced = false
		}
	}
	sortServices(allSvcs)
	warnUnbound(specs, allSvcs, m.warned)

	applied, skipped := 0, 0
	live := make(map[string]bool, len(specs))
	for _, spec := range specs {
		live[spec.Key] = true

		// A scoped spec reads only its own namespaces' stores.
		var svcs []*corev1.Service
		var sls []*discoveryv1.EndpointSlice
		ready := true
		for _, ns := range spec.Namespaces {
			d, ok := data[ns]
			if !ok {
				ready = false
				break
			}
			svcs = append(svcs, d.svcs...)
			sls = append(sls, d.slices...)
		}
		if ready && !nodesSynced && needsNodes(spec, svcs) {
			ready = false
		}
		if !ready {
			skipped++
			continue
		}
		sortServices(svcs)

		eps := computePool(spec, svcs, indexSlices(sls), nodeSet, m.opts.Settings.Nodes, m.warned)
		last, had := m.applied[spec.Key]
		if (!had && len(eps) == 0) || (had && endpointsEqual(last, eps)) {
			m.applied[spec.Key] = eps
			continue
		}
		m.applied[spec.Key] = eps
		if m.opts.Sink != nil {
			m.opts.Sink.SetEndpoints(spec.Key, slices.Clone(eps))
		}
		applied++
		emit.Info.StructuredFields("Updated endpoints for pool",
			emit.ZInt("endpoint_count", len(eps)),
			emit.ZString("pool", spec.Key))
	}
	for key := range m.applied {
		if !live[key] {
			delete(m.applied, key)
		}
	}
	m.warned.endPass(allSynced)
	if allSynced {
		m.markSynced()
	}

	result := "unchanged"
	switch {
	case applied > 0:
		result = "applied"
	case skipped > 0:
		result = "skipped"
	}
	m.opts.Recorder.DiscoveryReconcile(result, time.Since(begin))
}

func sortServices(svcs []*corev1.Service) {
	slices.SortFunc(svcs, func(a, b *corev1.Service) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
}
