package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func exercise(r Recorder) {
	r.ConnAccepted("l")
	r.ConnRejected("l", "acl")
	r.ConnActive("l", 1)
	r.ConnClosed("l", "ok", time.Second, 10, 20)
	r.BackendDial("l", "p", "10.0.0.1:80", "ok", time.Millisecond)
	r.BackendActive("l", "p", "10.0.0.1:80", 1)
	r.BackendHealth("l", "p", "10.0.0.1:80", true, "probe")
	r.PoolSize("l", "p", 1, 2)
	r.ProbeDone("l", "p", "ok", time.Millisecond)
	r.UDPSession("l", "open")
	r.UDPDatagram("l", "in", 5)
	r.DiscoveryReconcile("applied", time.Millisecond)
	r.InformerSynced("nodes", true)
	r.ConfigReload("applied")
	r.Ready(true)
	r.PipeMode("splice")
	r.AccessLogDropped()
	r.DrainForced("l", 2)
}

func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		m[f.GetName()] = f
	}
	return m
}

func TestEveryCatalogueMetricRegistered(t *testing.T) {
	reg := NewRegistry()
	RegisterBuildInfo(reg)
	exercise(NewPrometheus(reg, true))
	got := gather(t, reg)
	for _, n := range []string{
		"build_info", "ready", "config_reload_total", "config_last_reload_timestamp_seconds",
		"connections_accepted_total", "connections_rejected_total", "connections_active",
		"connection_duration_seconds", "connection_bytes_total", "pipe_mode_total",
		"backend_dial_total", "backend_dial_duration_seconds", "backend_connections_active",
		"backend_healthy", "backend_health_transitions_total", "pool_backends",
		"health_probe_duration_seconds", "udp_sessions_active", "udp_sessions_total",
		"udp_datagrams_total", "discovery_reconcile_total", "discovery_reconcile_duration_seconds",
		"discovery_informer_synced", "discovery_last_success_timestamp_seconds", "accesslog_dropped_total", "drain_forced_total",
	} {
		if _, ok := got["nautiluslb_"+n]; !ok {
			t.Errorf("missing nautiluslb_%s", n)
		}
	}
	if _, ok := got["go_goroutines"]; !ok {
		t.Error("go collector missing")
	}
}

func TestRecorderLabelsNeverClientIP(t *testing.T) {
	for _, pb := range []bool{true, false} {
		reg := prometheus.NewRegistry()
		exercise(NewPrometheus(reg, pb))
		banned := map[string]bool{"client": true, "source": true, "ip": true, "sni": true, "host": true, "server_name": true}
		for _, f := range gather(t, reg) {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if banned[l.GetName()] {
						t.Errorf("%s has banned label %q", f.GetName(), l.GetName())
					}
				}
			}
		}
	}
}

func TestPerBackendFalseOmitsBackendLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	exercise(NewPrometheus(reg, false))
	got := gather(t, reg)
	for name, f := range got {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "backend" {
					t.Errorf("%s carries backend label", name)
				}
			}
		}
	}
	if _, ok := got["nautiluslb_backend_healthy"]; ok {
		t.Error("backend_healthy must not exist when perBackend=false")
	}
	if f := got["nautiluslb_backend_dial_total"]; f == nil || f.GetMetric()[0].GetCounter().GetValue() != 1 {
		t.Error("dial total should aggregate without backend label")
	}
}

func TestBuildInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterBuildInfo(reg)
	f := gather(t, reg)["nautiluslb_build_info"]
	if f == nil || len(f.GetMetric()) != 1 {
		t.Fatal("build_info missing")
	}
	m := f.GetMetric()[0]
	if m.GetGauge().GetValue() != 1 {
		t.Error("value != 1")
	}
	var names []string
	for _, l := range m.GetLabel() {
		names = append(names, l.GetName())
	}
	if strings.Join(names, ",") != "commit,go_version,version" {
		t.Errorf("labels = %v", names)
	}
}

func TestNopAndDeltas(t *testing.T) {
	exercise(NewNop())
	reg := prometheus.NewRegistry()
	r := NewPrometheus(reg, true)
	r.ConnActive("l", 2)
	r.ConnActive("l", -1)
	if v := gather(t, reg)["nautiluslb_connections_active"].GetMetric()[0].GetGauge().GetValue(); v != 1 {
		t.Errorf("active = %v; want 1", v)
	}
}

func TestDrainForcedAddsN(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewPrometheus(reg, false)
	r.DrainForced("l", 2)
	r.DrainForced("l", 3)
	r.DrainForced("l", 0)
	r.DrainForced("l", -4)
	f := gather(t, reg)["nautiluslb_drain_forced_total"]
	if f == nil || len(f.GetMetric()) != 1 {
		t.Fatalf("family = %v", f)
	}
	if v := f.GetMetric()[0].GetCounter().GetValue(); v != 5 {
		t.Fatalf("drain_forced_total = %v, want 5", v)
	}
}
