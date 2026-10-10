package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Workspace is a set of repositories, selected from one or more provider
// sources, that agent Sessions work on. It belongs to an Organization, whose
// members manage it; the workspace-manager writes the object, its volume and
// its sync Jobs.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ws
// +kubebuilder:printcolumn:name="Organization",type=string,JSONPath=`.spec.organization`
// +kubebuilder:printcolumn:name="Last sync",type=date,JSONPath=`.status.lastSync.time`
// +kubebuilder:printcolumn:name="Result",type=string,JSONPath=`.status.lastSync.result`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// WorkspaceSpec is what a member chooses.
type WorkspaceSpec struct {
	// Organization owns the workspace: only its members read and write it.
	// It never changes, so a workspace cannot be moved out of an
	// Organization's reach.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="organization is immutable"
	Organization string `json:"organization"`

	// Sources select the workspace's repositories, one owner each.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Sources []Source `json:"sources"`

	// Sync is when the mirrors are fetched.
	// +kubebuilder:default={schedule: nightly}
	// +optional
	Sync Sync `json:"sync,omitempty"`

	// Sizing overrides the computed size of the workspace's volume, where its
	// StorageClass provisions a size. There is no cap.
	// +optional
	Sizing *Sizing `json:"sizing,omitempty"`
}

// Source is one owner's repositories on one provider instance. At least one
// selector picks repositories: names, a language filter or a topic filter.
//
// +kubebuilder:validation:XValidation:rule="(has(self.repositories) && size(self.repositories) > 0) || (has(self.filters) && ((has(self.filters.languages) && size(self.filters.languages) > 0) || has(self.filters.topics)))",message="a source needs at least one selector: repositories, filters.languages or filters.topics"
type Source struct {
	// Provider is the name of a provider instance the installation configures.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Provider string `json:"provider"`

	// Owner is the account on the provider: an organization or a user.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Owner string `json:"owner"`

	// Repositories are selected by name; a named repository counts even
	// when archived or a fork.
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=255
	// +listType=set
	// +optional
	Repositories []string `json:"repositories,omitempty"`

	// Filters select the owner's repositories by language and topics.
	// +optional
	Filters *Filters `json:"filters,omitempty"`

	// Exclude names repositories the filters would otherwise select.
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=255
	// +listType=set
	// +optional
	Exclude []string `json:"exclude,omitempty"`

	// IncludeArchived lets the filters select archived repositories.
	// +kubebuilder:default=false
	// +optional
	IncludeArchived bool `json:"includeArchived,omitempty"`

	// IncludeForks lets the filters select forks.
	// +kubebuilder:default=false
	// +optional
	IncludeForks bool `json:"includeForks,omitempty"`
}

// Filters select repositories by what the provider reports about them. A
// repository matches when it matches every filter set.
type Filters struct {
	// Languages selects repositories whose primary language is any of these.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	// +optional
	Languages []string `json:"languages,omitempty"`

	// Topics selects repositories by their topics.
	// +optional
	Topics *TopicFilter `json:"topics,omitempty"`
}

// TopicMatch is how a topic filter combines its topics.
// +kubebuilder:validation:Enum=any;all
type TopicMatch string

const (
	// TopicMatchAny selects a repository carrying any of the topics.
	TopicMatchAny TopicMatch = "any"
	// TopicMatchAll selects a repository carrying all of the topics.
	TopicMatchAll TopicMatch = "all"
)

// TopicFilter selects repositories carrying any, or all, of its topics.
type TopicFilter struct {
	// Match is any (the default) or all.
	// +kubebuilder:default=any
	// +optional
	Match TopicMatch `json:"match,omitempty"`

	// Names are the topics.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	Names []string `json:"names"`
}

// Schedule is a sync cycle.
// +kubebuilder:validation:Enum=nightly;weekly;custom
type Schedule string

const (
	// ScheduleNightly syncs every night, the default.
	ScheduleNightly Schedule = "nightly"
	// ScheduleWeekly syncs once a week.
	ScheduleWeekly Schedule = "weekly"
	// ScheduleCustom syncs on the chosen weekdays at the chosen hour.
	ScheduleCustom Schedule = "custom"
)

// Weekday is a day of the week.
// +kubebuilder:validation:Enum=monday;tuesday;wednesday;thursday;friday;saturday;sunday
type Weekday string

// Sync is the workspace's sync cycle. A workspace also syncs after every
// change and on request.
//
// +kubebuilder:validation:XValidation:rule="self.schedule == 'custom' ? (has(self.weekdays) && size(self.weekdays) > 0 && has(self.hour)) : (!has(self.weekdays) && !has(self.hour))",message="weekdays and hour are set for, and only for, a custom schedule"
type Sync struct {
	// Schedule is nightly, weekly, or custom (chosen weekdays at an hour).
	// +kubebuilder:default=nightly
	Schedule Schedule `json:"schedule"`

	// Weekdays a custom schedule syncs on.
	// +kubebuilder:validation:MaxItems=7
	// +listType=set
	// +optional
	Weekdays []Weekday `json:"weekdays,omitempty"`

	// Hour of the day, 0 to 23 UTC, a custom schedule syncs at.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=23
	// +optional
	Hour *int32 `json:"hour,omitempty"`
}

// Sizing overrides the volume's computed size, where its StorageClass
// provisions one.
type Sizing struct {
	// Headroom replaces the computed headroom when larger.
	// +kubebuilder:validation:XValidation:rule="!quantity(string(self)).isLessThan(quantity('0'))",message="headroom must not be negative"
	// +optional
	Headroom *resource.Quantity `json:"headroom,omitempty"`

	// Minimum is the smallest size the volume gets.
	// +kubebuilder:validation:XValidation:rule="!quantity(string(self)).isLessThan(quantity('0'))",message="minimum must not be negative"
	// +optional
	Minimum *resource.Quantity `json:"minimum,omitempty"`
}

// SyncResult is how a sync ended.
// +kubebuilder:validation:Enum=Succeeded;Failed;NoChange
type SyncResult string

const (
	// SyncSucceeded fetched what changed.
	SyncSucceeded SyncResult = "Succeeded"
	// SyncFailed did not complete; the mirrors stay as they were.
	SyncFailed SyncResult = "Failed"
	// SyncNoChange found nothing changed since the last manifest.
	SyncNoChange SyncResult = "NoChange"
)

// WorkspaceStatus is what the workspace-manager observed.
type WorkspaceStatus struct {
	// Conditions are the workspace's conditions (Ready, Synced).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastSync is the last sync's time and result.
	// +optional
	LastSync *LastSync `json:"lastSync,omitempty"`

	// Repositories are the repositories the last sync resolved, as
	// `<owner>/<name>`.
	// +listType=set
	// +optional
	Repositories []string `json:"repositories,omitempty"`

	// NeededSize is what the mirrors and the live session directories need.
	// +optional
	NeededSize *resource.Quantity `json:"neededSize,omitempty"`

	// VolumeSize is the volume's provisioned size; empty where the
	// StorageClass provisions none.
	// +optional
	VolumeSize *resource.Quantity `json:"volumeSize,omitempty"`

	// Volume is the workspace's read-write-many volume.
	// +optional
	Volume *VolumeRef `json:"volume,omitempty"`

	// Manifest names the ConfigMap holding the last sync's manifest.
	// +optional
	Manifest string `json:"manifest,omitempty"`

	// Sessions are the session directories on the volume.
	// +listType=map
	// +listMapKey=session
	// +optional
	Sessions []SessionDirectory `json:"sessions,omitempty"`
}

// LastSync is a sync's outcome.
type LastSync struct {
	// Time the sync ended.
	Time metav1.Time `json:"time"`
	// Result of the sync.
	Result SyncResult `json:"result"`
	// Message says why a sync failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// VolumeRef is the volume as its CSI driver knows it.
type VolumeRef struct {
	// ClaimName is the PersistentVolumeClaim's name.
	ClaimName string `json:"claimName"`
	// Driver is the CSI driver.
	// +optional
	Driver string `json:"driver,omitempty"`
	// Handle is the CSI volume handle.
	// +optional
	Handle string `json:"handle,omitempty"`
}

// SessionDirectory is one Session's directory on the volume.
type SessionDirectory struct {
	// Session is the Session's name.
	Session string `json:"session"`
	// Size measured at the last sync.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
	// CleanupAfter is when the directory is deleted: 30 days after its
	// Session's last turn.
	// +optional
	CleanupAfter *metav1.Time `json:"cleanupAfter,omitempty"`
}

// WorkspaceList is a list of Workspaces.
//
// +kubebuilder:object:root=true
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}
