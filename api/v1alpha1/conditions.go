package v1alpha1

// The Workspace's condition types, set by the manager's controller.
const (
	// ConditionVolumeClaimed is True once the workspace's read-write-many
	// volume is claimed (status.volume names the claim), False while it is
	// not: with reason StorageClassMissing while the installation names no
	// StorageClass to claim it from, VolumeClaimFailed when the claim was
	// refused by the API server.
	ConditionVolumeClaimed = "VolumeClaimed"
	// ConditionSynced is True once the last sync cycle brought the volume's
	// mirrors to the selection (reason Synced) or found nothing to do
	// (NoChange); Unknown while a cycle runs (Syncing); False when the last
	// cycle failed, with the reason: the listing (ListingFailed), the sync
	// Job (SyncFailed), the volume's size past the installation's ceiling
	// (SizeCeiling) or no volume to sync onto (VolumeMissing).
	ConditionSynced = "Synced"
)

// The reasons of ConditionVolumeClaimed.
const (
	// ReasonVolumeClaimed: the claim exists and names the StorageClass.
	ReasonVolumeClaimed = "Claimed"
	// ReasonStorageClassMissing: the manager runs without --storage-class (the
	// chart's storage.storageClassName); no claim is made, the cluster's
	// default class is never used.
	ReasonStorageClassMissing = "StorageClassMissing"
	// ReasonVolumeClaimFailed: the API server refused the claim; the message
	// carries its error.
	ReasonVolumeClaimFailed = "VolumeClaimFailed"
)

// The reasons of ConditionSynced.
const (
	// ReasonSynced: the sync Job fetched what changed.
	ReasonSynced = "Synced"
	// ReasonNoChange: the selection equals the last manifest and nothing was
	// pushed since; no Job ran.
	ReasonNoChange = "NoChange"
	// ReasonSyncing: a sync Job runs; the message names it.
	ReasonSyncing = "Syncing"
	// ReasonListingFailed: a provider's listing failed; the message carries
	// its error.
	ReasonListingFailed = "ListingFailed"
	// ReasonSyncFailed: the sync Job failed; the mirrors stay as they were.
	ReasonSyncFailed = "SyncFailed"
	// ReasonSizeCeiling: the volume would pass the installation's ceiling;
	// the message names the size needed and the ceiling.
	ReasonSizeCeiling = "SizeCeiling"
	// ReasonVolumeMissing: the volume is not claimed, so nothing can sync
	// onto it; the VolumeClaimed condition says why.
	ReasonVolumeMissing = "VolumeMissing"
)

// The annotations a sync request leaves on a Workspace: sync_workspace sets
// them, the controller starts a cycle for them and removes them.
const (
	// AnnotationSyncRequested is when the sync was requested, RFC 3339.
	AnnotationSyncRequested = "workspace-manager.giantswarm.io/sync-requested"
	// AnnotationSyncRequestedBy is who requested it.
	AnnotationSyncRequestedBy = "workspace-manager.giantswarm.io/sync-requested-by"
)
