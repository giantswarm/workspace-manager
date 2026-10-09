package buildinfo

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolve(t *testing.T) {
	tagged := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/giantswarm/workspace-manager", Version: "v1.1.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "6e3caf2d9c1f4b7a8e2d0c5b6a7f8e9d0c1b2a3f"},
			{Key: "vcs.time", Value: "2026-09-16T08:00:00Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	untaggedDirty := &debug.BuildInfo{
		Main:     debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef1234"}, {Key: "vcs.modified", Value: "true"}},
	}

	tests := map[string]struct {
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  Info
	}{
		"tag at HEAD fills the ldflags defaults": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: tagged,
			want: Info{Version: "1.1.9", Commit: "6e3caf2", Date: "2026-09-16T08:00:00Z"},
		},
		"tag at HEAD in a modified tree: the version stays the release, the commit is marked": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate,
			bi: &debug.BuildInfo{
				Main:     debug.Module{Version: "v1.1.9+dirty"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "6e3caf2d9c1f4b7a8e2d0c5b6a7f8e9d0c1b2a3f"}, {Key: "vcs.modified", Value: "true"}},
			},
			want: Info{Version: "1.1.9", Commit: "6e3caf2-dirty", Date: "unknown"},
		},
		"ldflags values win": {
			version: "1.2.0", commit: "1234567", date: "2026-10-01T00:00:00Z", bi: tagged,
			want: Info{Version: "1.2.0", Commit: "1234567", Date: "2026-10-01T00:00:00Z"},
		},
		"untagged commit reports the toolchain's pseudo-version": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate,
			bi: &debug.BuildInfo{
				Main:     debug.Module{Version: "v1.1.9-0.20260916142110-aef0725928df"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "aef0725928df0c1b2a3f6e3caf2d9c1f4b7a8e2d"}, {Key: "vcs.time", Value: "2026-09-16T14:21:10Z"}, {Key: "vcs.modified", Value: "false"}},
			},
			want: Info{Version: "1.1.9-0.20260916142110-aef0725928df", Commit: "aef0725", Date: "2026-09-16T14:21:10Z"},
		},
		"untagged dirty checkout stays dev and marks the commit": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: untaggedDirty,
			want: Info{Version: "dev", Commit: "abcdef1-dirty", Date: "unknown"},
		},
		"no build info leaves everything as given": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: nil,
			want: Info{Version: "dev", Commit: "unknown", Date: "unknown"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Resolve(tc.version, tc.commit, tc.date, tc.bi))
		})
	}
}
