package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Registry maps kind names to their implementations.
type Registry map[string]Factory

// Kinds are the registered kind names, sorted.
func (r Registry) Kinds() []string {
	names := make([]string, 0, len(r))
	for k := range r {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Instance is one configured provider: a name the workspaces and the person's
// sign-in refer to, and its kind's implementation.
type Instance struct {
	Name     string
	KindName string
	Kind
}

// Config is the installation's provider configuration, the chart's
// `providers` value.
type Config struct {
	Providers []InstanceConfig `json:"providers"`
}

// InstanceConfig is one provider instance as configured.
type InstanceConfig struct {
	// Name is the instance's name: unique, a DNS label, because it is the
	// token exchange's audience and part of stored keys.
	Name string `json:"name"`
	// Kind is a registered kind's name.
	Kind string `json:"kind"`
	// Values are the kind's own settings, which its Factory validates.
	Values json.RawMessage `json:"values,omitempty"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Load parses a provider configuration (YAML or JSON) and builds every
// instance, refusing the whole configuration with every problem named: an
// unknown field, a missing or invalid name, a duplicate name, an unknown kind,
// a kind's invalid values or a Secret reference without a name or key.
func (r Registry) Load(data []byte) ([]Instance, error) {
	js, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("provider configuration: %w", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("provider configuration: %w", err)
	}

	var errs []error
	seen := map[string]bool{}
	instances := make([]Instance, 0, len(cfg.Providers))
	for i, ic := range cfg.Providers {
		label := fmt.Sprintf("provider %d", i+1)
		if ic.Name != "" {
			label = fmt.Sprintf("provider %q", ic.Name)
		}
		switch {
		case ic.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", label))
			continue
		case !dnsLabel.MatchString(ic.Name):
			errs = append(errs, fmt.Errorf("%s: name must be a DNS label (lower-case letters, digits, '-')", label))
			continue
		case seen[ic.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate name", label))
			continue
		}
		seen[ic.Name] = true
		factory, ok := r[ic.Kind]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: unknown kind %q (known: %s)", label, ic.Kind, strings.Join(r.Kinds(), ", ")))
			continue
		}
		values := ic.Values
		if len(values) == 0 || string(values) == "null" {
			values = json.RawMessage("{}")
		}
		k, err := factory(values)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s (kind %s): %w", label, ic.Kind, err))
			continue
		}
		for _, ref := range k.SecretRefs() {
			if ref.Name == "" || ref.Key == "" {
				errs = append(errs, fmt.Errorf("%s (kind %s): a Secret reference needs a name and a key, got %q", label, ic.Kind, ref.String()))
			}
		}
		instances = append(instances, Instance{Name: ic.Name, KindName: ic.Kind, Kind: k})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return instances, nil
}
