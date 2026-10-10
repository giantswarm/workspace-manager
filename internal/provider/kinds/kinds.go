// Package kinds is the one list of provider kinds compiled into the binary.
// It is the only package outside a kind's own that imports one (enforced by
// its tests): adding a kind is its package and one line here.
package kinds

import (
	"net/http"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
)

// Registry returns every kind the binary serves; hc is the HTTP client their
// API calls use (nil for http.DefaultClient).
func Registry(hc *http.Client) provider.Registry {
	return provider.Registry{
		github.KindName: github.Factory(hc),
	}
}
