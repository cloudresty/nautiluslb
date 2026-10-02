package config

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a time.Duration that unmarshals from "30s"/"1m" strings and,
// for v1 compatibility, from bare integers meaning seconds (deprecated; Parse
// records a warning for those).
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a string like \"30s\" or an integer number of seconds", value.Line)
	}
	if value.ShortTag() == "!!int" {
		var secs int64
		if err := value.Decode(&secs); err != nil {
			return fmt.Errorf("line %d: invalid duration %q: %w", value.Line, value.Value, err)
		}
		const maxSecs = int64(1<<63-1) / int64(time.Second)
		if secs > maxSecs || secs < -maxSecs {
			return fmt.Errorf("line %d: duration %q out of range", value.Line, value.Value)
		}
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	if value.ShortTag() != "!!str" {
		return fmt.Errorf("line %d: invalid duration %q: use a string like \"30s\"", value.Line, value.Value)
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", value.Line, value.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
