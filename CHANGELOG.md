# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `sync`: the Job that keeps a workspace's repositories on its volume as bare mirrors under `mirrors/<owner>/<name>.git`: it clones added repositories, fetches only those pushed to since the manifest, marks the mirrors of repositories that left the selection and writes the manifest (head commits, push times, measured sizes) to the volume and, with `--result-configmap`, as the Job's result. While a session directory's clone borrows a mirror's objects, the sync's repack keeps every object (`gc.pruneExpire=never`, `--keep-unreachable`); the credential reaches git through the binary's own credential helper from a projected file, never through a URL, the volume or a log.
- The image carries git for the sync Job; the chart renders the Job's ServiceAccount and Role in the workspaces' namespace, allowed to write its result ConfigMap.



[Unreleased]: https://github.com/giantswarm/workspace-manager/tree/main
