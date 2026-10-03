package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Environment variables overriding settings.* (explicit table, no reflection).
const (
	EnvLogLevel         = "NLB_LOG_LEVEL"
	EnvKubeconfig       = "NLB_KUBECONFIG"
	EnvAdminAddress     = "NLB_ADMIN_ADDRESS"
	EnvPprof            = "NLB_PPROF"
	EnvAccessLogEnabled = "NLB_ACCESS_LOG_ENABLED"
	EnvMaxConnections   = "NLB_MAX_CONNECTIONS"
	EnvDrainTimeout     = "NLB_DRAIN_TIMEOUT"
)

// ApplyEnv overrides settings from NLB_* variables via lookup (os.LookupEnv in
// production). A set variable always wins over the file, including an empty
// NLB_ADMIN_ADDRESS (disables the admin server). Invalid values are reported
// together, each naming its variable.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) error {
	var errs []error
	s := &c.Settings

	if v, ok := lookup(EnvLogLevel); ok {
		s.LogLevel = v
	}
	if v, ok := lookup(EnvKubeconfig); ok {
		s.Kubernetes.Kubeconfig = v
		s.KubeconfigPath = "" // the env override replaces the deprecated alias too
	}
	if v, ok := lookup(EnvAdminAddress); ok {
		s.Admin.Address = v
	}
	if v, ok := lookup(EnvPprof); ok {
		if b, err := strconv.ParseBool(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", EnvPprof, v))
		} else {
			s.Admin.Pprof = b
		}
	}
	if v, ok := lookup(EnvAccessLogEnabled); ok {
		if b, err := strconv.ParseBool(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", EnvAccessLogEnabled, v))
		} else {
			s.AccessLog.Enabled = b
		}
	}
	if v, ok := lookup(EnvMaxConnections); ok {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s: invalid value %q: must be an integer >= 0", EnvMaxConnections, v))
		} else {
			s.Limits.MaxConnections = n
		}
	}
	if v, ok := lookup(EnvDrainTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			if secs, err2 := strconv.Atoi(v); err2 == nil {
				d, err = time.Duration(secs)*time.Second, nil
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q: use a value like \"30s\"", EnvDrainTimeout, v))
		} else {
			s.Drain.Timeout = Duration(d)
		}
	}
	return errors.Join(errs...)
}
