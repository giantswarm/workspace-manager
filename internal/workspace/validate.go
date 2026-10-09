package workspace

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// ErrInvalid is returned for a Workspace the API server would accept but the
// installation cannot serve.
var ErrInvalid = errors.New("invalid workspace")

// Validate checks what the CRD's schema cannot: every source names a provider
// instance the installation configures. The schema's own rules (a selector per
// source, the schedule, non-negative sizing) are the API server's.
func Validate(ws *v1alpha1.Workspace, providers []string) error {
	var errs []error
	for i, s := range ws.Spec.Sources {
		if !slices.Contains(providers, s.Provider) {
			errs = append(errs, fmt.Errorf("sources[%d]: unknown provider instance %q (configured: %s)", i, s.Provider, known(providers)))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w %q: %w", ErrInvalid, ws.Name, errors.Join(errs...))
	}
	return nil
}

func known(providers []string) string {
	if len(providers) == 0 {
		return "none"
	}
	sorted := slices.Sorted(slices.Values(providers))
	return strings.Join(sorted, ", ")
}
