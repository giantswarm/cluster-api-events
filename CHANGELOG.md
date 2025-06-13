# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed 

- Disable logger development mode to avoid panicking, use zap as logger.
- Go: Update dependencies.
- Fix linting issues.

## [0.3.0] - 2024-08-21

### Added 

- Set new annotations `giantswarm.io/cluster-upgrading` on Cluster CR.

### Fixed

- Ignore updating ready condition timestamps like `ClusterReconcilerNormalFailed` which would send Upgrade events.

## [0.2.0] - 2024-08-16

### Added

- Set new annotations `giantswarm.io/last-known-cluster-upgrade-timestamp` and `giantswarm.io/last-known-cluster-upgrade-version` on Cluster CR.

## [0.1.6] - 2024-08-14

### Fixed

- Events RBAC.

## [0.1.5] - 2024-08-13

### Fixed

- Transition timestamp.

## [0.1.4] - 2024-08-13

### Fixed

- PSP RBAC.

## [0.1.3] - 2024-08-13

### Fixed

- Added `events.k8s.io` as apiGroup in RBAC.

## [0.1.2] - 2024-08-13

## [0.1.1] - 2024-08-13

## [0.1.0] - 2024-08-13

[Unreleased]: https://github.com/giantswarm/cluster-api-events/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.6...v0.2.0
[0.1.6]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/giantswarm/cluster-api-events/releases/tag/v0.1.0
