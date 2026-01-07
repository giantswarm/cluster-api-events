# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Migrate to App Build Suite (ABS) for Helm chart building.

## [0.8.0] - 2025-11-20

### Added

- Add duration time when cluster upgrade is finished.

## [0.7.0] - 2025-07-19

### Added

- Intermediate upgrade progress event `UpgradedControlPlane` when control plane upgrade completes during cluster upgrades.
- Event deduplication using `giantswarm.io/emitted-upgrade-events` annotation to prevent repeated notifications.

### Fixed

- Fixed version comparison logic that was comparing incompatible version formats (release version "31.0.0" vs Kubernetes version "v1.31.9"). Now correctly compares release version labels to determine if MachineDeployments and MachinePools match the cluster version.
- Fixed upgrade state being lost when timestamp annotation was missing. Now properly preserves upgrade state and sets timestamp when upgrade begins.
- Fixed race condition where control plane and final upgrade events could fire simultaneously on controller restart.

## [0.6.0] - 2025-07-18

### Added

- Enhanced MachinePool version checking by connecting to workload clusters to inspect individual node versions.
- Direct node version validation for MachinePools using `giantswarm.io/machine-pool` labels and workload cluster connectivity.
- Improved MachineDeployment version checking with individual Machine resource validation through ownership chain traversal.
- Graceful fallback to basic status checking when workload cluster access fails.
- Vertical Pod Autoscaler (VPA) support for automatic resource scaling based on cluster count and workload (enabled by default).
- Memory usage optimizations for handling multiple clusters simultaneously.

### Fixed

- Corrected version matching logic for pinned MachinePools and MachineDeployments that have explicit `spec.template.spec.version` set to allow control plane upgrades while keeping worker nodes at specific versions.

## [0.5.3] - 2025-07-18

### Fixed

- Fixed MachinePools rollout completion check to verify replica counts match exactly and all referenced nodes are running the expected Kubernetes version.
- Improved upgrade completion logic to ignore nodes in SchedulingDisabled state that are being drained and terminated.
- Added support for staged upgrades where MachinePools may be intentionally pinned to older versions than the cluster target version.

## [0.5.2] - 2025-07-15

### Fixed

- Ensured cluster upgrade completion is only reported when all worker nodes are fully updated and ready.

## [0.5.1] - 2025-07-08

### Fixed

- RBAC permissions for `MachinePools` and `MachineDeployments`.

## [0.5.0] - 2025-07-03

### Added

- Support for both `MachineDeployments` and `MachinePools` worker node tracking.
- Added logging for upgrade progress monitoring.

### Fixed

- Annotation update conflicts causing "object has been modified" errors.
- Premature "Upgraded" events when only control plane was ready - now waits for all worker nodes.

## [0.4.0] - 2025-06-24

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

[Unreleased]: https://github.com/giantswarm/cluster-api-events/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.5.3...v0.6.0
[0.5.3]: https://github.com/giantswarm/cluster-api-events/compare/v0.5.2...v0.5.3
[0.5.2]: https://github.com/giantswarm/cluster-api-events/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/giantswarm/cluster-api-events/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.6...v0.2.0
[0.1.6]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/giantswarm/cluster-api-events/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/giantswarm/cluster-api-events/releases/tag/v0.1.0
