# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **Critical**: Fixed premature upgrade completion events for MachinePools. CAPI conditions (`WorkerMachinesUpToDate`) can report `True` based on ASG/Launch Template state while actual nodes in the workload cluster are still being replaced. Now ALWAYS verifies actual workload cluster node versions instead of trusting CAPI conditions.

## [1.0.3] - 2026-01-17

### Fixed

- Fixed new cluster creation triggering false "Upgrading" events with empty "from version". Now only sends upgrade events when there is a valid previous version.
- Fixed duplicate completion events being sent by concurrent reconciles. Now tracks "Upgraded" event in annotations to prevent race conditions.
- Fixed potential duplicate "Upgrading" events from concurrent reconciles by refetching cluster state before sending event.

## [1.0.2] - 2026-01-16

### Fixed

- Fixed race condition where upgrade completion events were sent immediately after upgrade started, before CAPI had time to update conditions. Added minimum upgrade duration check (30 seconds) and new `giantswarm.io/upgrade-start-time` annotation to track actual upgrade start time.
- Handle missing `giantswarm.io/upgrade-start-time` annotation for existing upgrades (started before this fix was deployed or after controller restart) by falling back to `lastKnownTransitionTime` or setting it to current time.

## [1.0.1] - 2026-01-16

### Fixed

- Fixed MachinePools always being reported as "not ready" in CAPI v1beta2 because `Available`/`Ready` conditions are no longer in `status.conditions` (moved to `status.deprecated.v1beta1.conditions`). Now uses `status.phase` for MachinePool readiness checks.
- Fixed clusters getting stuck in "upgrading" state forever because `timeProgressed` check required the `AvailableCondition.lastTransitionTime` to change, but clusters often stay Available throughout upgrades. Replaced with `RollingOut: False` condition check.
- Fixed upgrade completion detection by using Cluster-level v1beta2 conditions (`WorkerMachinesReady`, `WorkerMachinesUpToDate`) as primary source instead of checking individual MachinePool conditions.
- Skip Karpenter-managed MachinePools (annotation: `cluster.x-k8s.io/replicas-managed-by: external-autoscaler`) in individual resource checks since they are externally managed; rely on Cluster-level conditions for these pools.

## [1.0.0] - 2026-01-16

### Changed

- Switched to using `Available` condition for Cluster status checks instead of deprecated `Ready` condition, aligning with CAPI v1beta2.
- Updated worker node readiness checks to use v1beta2 `Available` condition with fallback to v1beta1 `Ready` condition for backward compatibility.
- Simplified MachineDeployment version checking by leveraging v1beta2 `MachinesUpToDate` condition instead of manually traversing Machine ownership chains.
- **Kept workload cluster node version checking as primary verification method for MachinePools** (including Karpenter-managed nodes), as v1beta1 MachinePools lack `upToDateReplicas` field and Karpenter provisions nodes independently. Falls back to v1beta2 `upToDateReplicas` status field only when workload cluster is inaccessible AND field is available.

### Fixed

- Fixed upgrade event detection with v1beta2 to send "Upgrading" events when release version changes, regardless of `RollingOutCondition` state. The `RollingOutCondition` may not be set immediately or consistently by all infrastructure/control plane providers during version upgrades.
- Fixed `last-known-cluster-upgrade-version` annotation not being updated when upgrade completes, which would cause incorrect version comparison on subsequent upgrades.
- Fixed control plane upgrade event firing too early by adding check for `ControlPlaneMachinesUpToDateCondition`. Event now waits for all control plane machines to be upgraded and up-to-date.
- Fixed worker node readiness check using deprecated v1beta1 `Ready` condition instead of v1beta2 `Available` condition for MachineDeployments and MachinePools, causing false "not ready" detection.
- Fixed MachinePool version checking to be conservative when workload cluster is inaccessible and `upToDateReplicas` field is not available (v1beta1), preventing false "ready" reports during upgrades.

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

[Unreleased]: https://github.com/giantswarm/cluster-api-events/compare/v1.0.3...HEAD
[1.0.3]: https://github.com/giantswarm/cluster-api-events/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/giantswarm/cluster-api-events/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/giantswarm/cluster-api-events/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/giantswarm/cluster-api-events/compare/v0.8.0...v1.0.0
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
