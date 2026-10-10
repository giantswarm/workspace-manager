package v1alpha1

// The Workspace's condition types, set by the manager's controller.
const (
	// ConditionVolumeClaimed is True once the workspace's read-write-many
	// volume is claimed (status.volume names the claim), False while it is
	// not: with reason StorageClassMissing while the installation names no
	// StorageClass to claim it from, VolumeClaimFailed when the claim was
	// refused by the API server.
	ConditionVolumeClaimed = "VolumeClaimed"
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
