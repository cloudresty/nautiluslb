package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudresty/emit"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/admin"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/kube"
	"github.com/cloudresty/nautiluslb/internal/metrics"
	nlbruntime "github.com/cloudresty/nautiluslb/internal/runtime"
	"github.com/cloudresty/nautiluslb/internal/version"
)

const (
	watchInterval  = 2 * time.Second
	shutdownMargin = 2 * time.Second
	closeTimeout   = 2 * time.Second
)

// newKubeClient is kube.NewClient; tests substitute a fake clientset.
var newKubeClient = kube.NewClient

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout))
}

// run is main without os.Exit, so the flag paths are testable.
func run(args []string, getenv func(string) string, stdout io.Writer) int {
	fs := flag.NewFlagSet("nautiluslb", flag.ContinueOnError)
	fs.SetOutput(stdout)
	showHelp := fs.Bool("help", false, "Show help information")
	showVersion := fs.Bool("version", false, "Print version and exit")
	validate := fs.Bool("validate", false, "Load and validate the configuration, print a summary and exit")
	pprofOn := fs.Bool("pprof", false, "Enable /debug/pprof on the admin server")
	defaultConfig := "config.yaml"
	if env := getenv("NLB_CONFIG"); env != "" {
		defaultConfig = env
	}
	configPath := fs.String("config", defaultConfig, "Path to the configuration file (env NLB_CONFIG sets the default)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}
	if *showHelp {
		printHelp(stdout)
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		emit.Error.StructuredFields("Failed to load configuration",
			emit.ZString("config_file", *configPath),
			emit.ZString("error", err.Error()))
		if *validate {
			_, _ = fmt.Fprintf(stdout, "invalid: %v\n", err)
		}
		return 1
	}

	if *validate {
		printSummary(stdout, *configPath, cfg)
		return 0
	}

	return serve(*configPath, cfg, *pprofOn)
}

func printHelp(w io.Writer) {
	lines := []string{
		"NautilusLB - Kubernetes-native Layer 4 Load Balancer",
		"",
		"Usage:",
		"  nautiluslb [options]",
		"",
		"Options:",
		"  --config     Path to the configuration file (default config.yaml, env NLB_CONFIG)",
		"  --validate   Load and validate the configuration, print a summary and exit (0 valid, 1 invalid; no network)",
		"  --pprof      Enable /debug/pprof on the admin server",
		"  --version    Print version and exit",
		"  --help       Show this help message",
		"",
		"Configuration:",
		"  Each configuration lists the namespaces it discovers Services in",
		"  (namespaces: [a, b]), or namespaces: [\"*\"] for cluster-wide discovery.",
		"",
		"  A Service is a backend of a configuration only when all of these hold:",
		"    " + config.ServiceEnabledAnnotation + ": \"true\"",
		"    " + config.ServiceConfigurationsAnnotation + ": \"<name>[,<name>...]\" names the configuration",
		"    the Service is in one of the configuration's namespaces",
		"    a Service port is named the configuration's backendPortName",
		"",
		"Admin endpoints (settings.admin.address):",
		"  /metrics  /healthz  /health/live  /readyz  /health/ready  /debug/pprof/* (when enabled)",
		"",
		"Signals:",
		"  SIGHUP         reload the configuration without dropping unchanged listeners",
		"  SIGTERM/SIGINT graceful drain; a second signal forces an immediate close",
		"",
		"See config.example.yaml for a complete example.",
		"For more information, visit: https://github.com/cloudresty/nautiluslb",
	}
	_, _ = fmt.Fprintln(w, strings.Join(lines, "\n"))
}

func printSummary(w io.Writer, path string, cfg *config.Config) {
	_, _ = fmt.Fprintf(w, "configuration %s is valid: %d configuration(s)\n", path, len(cfg.Configurations))
	for i := range cfg.Configurations {
		bc := &cfg.Configurations[i]
		ns := strings.Join(bc.DiscoveryNamespaces(), ",")
		if ns == "" {
			ns = config.AllNamespaces
		}
		_, _ = fmt.Fprintf(w, "  %s: protocol=%s listener=%s pools=%d namespaces=%s\n",
			bc.Name, bc.Protocol, bc.ListenerAddress, len(bc.Pools()), ns)
	}
	for _, d := range cfg.Deprecations() {
		_, _ = fmt.Fprintf(w, "deprecation: %s\n", d)
	}
}

func serve(configPath string, cfg *config.Config, pprofOn bool) int {
	emit.Info.Msg("Starting NautilusLB...")
	emit.Info.StructuredFields("Application Information",
		emit.ZString("app_name", "NautilusLB"),
		emit.ZString("repository", "https://github.com/cloudresty/nautiluslb"),
		emit.ZString("version", version.String()))
	emit.SetLevel(cfg.Settings.LogLevel)

	// Install the handler before anything binds, so SIGTERM always reaches
	// the shutdown path.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	reg := metrics.NewRegistry()
	metrics.RegisterBuildInfo(reg)
	rec := metrics.NewPrometheus(reg, cfg.Settings.Admin.Metrics.PerBackend)

	alog, closeLog, err := accesslog.New(cfg.Settings.AccessLog, rec)
	if err != nil {
		emit.Error.StructuredFields("Failed to open access log", emit.ZString("error", err.Error()))
		return 1
	}

	client, kubeContext, err := newKubeClient(cfg.Settings.Kubernetes, "nautiluslb/"+version.Version)
	if err != nil {
		emit.Error.StructuredFields("Failed to initialize Kubernetes client",
			emit.ZString("kubeconfig_path", cfg.Settings.Kubernetes.Kubeconfig),
			emit.ZString("error", err.Error()))
		return 1
	}
	emit.Info.StructuredFields("Initialized Kubernetes client", emit.ZString("context", kubeContext))

	rt, err := nlbruntime.New(nlbruntime.Options{Config: cfg, Client: client, Recorder: rec, AccessLog: alog})
	if err != nil {
		emit.Error.StructuredFields("Failed to bind listener", emit.ZString("error", err.Error()))
		return 1
	}

	adm := admin.New(admin.Options{Settings: cfg.Settings.Admin, Pprof: pprofOn, Gatherer: reg, Readiness: rt})
	if err := adm.Start(); err != nil {
		emit.Error.StructuredFields("Failed to start admin server",
			emit.ZString("admin_addr", cfg.Settings.Admin.Address),
			emit.ZString("error", err.Error()))
		// Release the bound listeners.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt.Shutdown(ctx)
		return 1
	}

	closeAll := func() { closeResources(closeLog, adm.Shutdown) }

	return lifecycle(rt, rec, cfg, configPath, sigs, closeAll)
}

// closeResources closes the access log first (so the last records are
// flushed), then shuts the admin server down, each bounded by closeTimeout.
func closeResources(closeLog, shutdownAdmin func(context.Context) error) {
	cctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	if err := closeLog(cctx); err != nil {
		emit.Warn.StructuredFields("Closing access log", emit.ZString("error", err.Error()))
	}
	cancel()
	actx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	if err := shutdownAdmin(actx); err != nil {
		emit.Warn.StructuredFields("Closing admin server", emit.ZString("error", err.Error()))
	}
	cancel()
}

// lifecycleRuntime is the part of the runtime the supervisor drives.
type lifecycleRuntime interface {
	Start(ctx context.Context) error
	Reload(cfg *config.Config) (nlbruntime.Summary, error)
	Shutdown(ctx context.Context) nlbruntime.Summary
}

// startOrSignal runs rt.Start in the background so that a shutdown signal
// received while it waits for discovery to sync is acted on at once: the
// signal cancels Start's context and the function waits for Start to return.
// SIGHUP during Start is re-queued for the supervisor. It returns Start's
// error and the terminating signal, if one arrived.
func startOrSignal(rt lifecycleRuntime, sigs chan os.Signal, bg context.Context, stopBg context.CancelFunc) (os.Signal, error) {
	done := make(chan error, 1)
	go func() { done <- rt.Start(bg) }()
	hup := false
	requeue := func() {
		if hup {
			select {
			case sigs <- syscall.SIGHUP:
			default:
			}
		}
	}
	for {
		select {
		case err := <-done:
			requeue()
			return nil, err
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				hup = true
				continue
			}
			stopBg()
			err := <-done
			return sig, err
		}
	}
}

// lifecycle starts rt, then supervises signals, reloads and shutdown. closeAll
// runs after the runtime has drained (access log, admin server).
func lifecycle(rt lifecycleRuntime, rec interface{ ConfigReload(string) }, cfg *config.Config, configPath string,
	sigs chan os.Signal, closeAll func()) int {
	bg, stopBg := context.WithCancel(context.Background())
	defer stopBg()

	timeout := cfg.Settings.Drain.Timeout.Std() + cfg.Settings.Drain.ReadinessDelay.Std() + shutdownMargin
	shutdown := func(ctx context.Context) {
		stopBg()
		sum := rt.Shutdown(ctx)
		emit.Info.StructuredFields("Drain complete", emit.ZInt("forced_connections", sum.Forced))
		closeAll()
	}

	sig, startErr := startOrSignal(rt, sigs, bg, stopBg)
	switch {
	case sig != nil:
		emit.Info.StructuredFields("Shutting down gracefully...", emit.ZString("signal", sig.String()))
		runShutdown(sigs, shutdown, timeout)
		emit.Info.Msg("Shutdown complete.")
		return 0
	case errors.Is(startErr, context.Canceled):
		runShutdown(sigs, shutdown, timeout)
		emit.Info.Msg("Shutdown complete.")
		return 0
	case startErr != nil:
		emit.Error.StructuredFields("Failed to start", emit.ZString("error", startErr.Error()))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt.Shutdown(ctx)
		return 1
	}

	watch := make(chan struct{}, 1)
	if cfg.Settings.Reload.WatchFile {
		go watchFile(bg, configPath, watchInterval, watch)
	}

	reload := func() {
		next, err := config.Load(configPath)
		if err != nil {
			emit.Error.StructuredFields("Failed to reload configuration",
				emit.ZString("config_file", configPath),
				emit.ZString("error", err.Error()))
			rec.ConfigReload("rejected")
			return
		}
		// A rejection is logged and counted by the runtime; only successes
		// are summarised here.
		sum, err := rt.Reload(next)
		if err != nil {
			return
		}
		emit.Info.StructuredFields("Configuration reloaded",
			emit.ZString("added", strings.Join(sum.Added, ",")),
			emit.ZString("removed", strings.Join(sum.Removed, ",")),
			emit.ZString("updated", strings.Join(sum.Updated, ",")),
			emit.ZString("unchanged", strings.Join(sum.Unchanged, ",")))
		if len(sum.Ignored) > 0 {
			emit.Warn.StructuredFields("Settings changed but need a restart",
				emit.ZString("ignored", strings.Join(sum.Ignored, ",")))
		}
	}

	supervise(sigs, watch, reload, shutdown, timeout)
	emit.Info.Msg("Shutdown complete.")
	return 0
}
