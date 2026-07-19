package core

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "8h".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"8h\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
