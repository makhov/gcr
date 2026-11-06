// Package v1alpha1 is gcr's Kubernetes API (gcr.dev/v1alpha1): one
// resource per operation on a running pod, which gcr-operator carries out
// and reports on in its status (docs/operations.md).
//
//   - Snapshot pushes a pod's container to a registry as an image that
//     pods run, carrying on from where it was.
//   - Migration moves a pod to another node, its processes running on.
//   - TimePoint takes a point on a container's timeline.
//   - Rewind takes a container back to a point on its timeline.
//
// Each is done once: it is Running from when gcr-operator picks it up,
// then Succeeded or Failed. Changing its spec afterwards does nothing;
// create another.
package v1alpha1

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group   = "gcr.dev"
	Version = "v1alpha1"
)

// The resources, for dynamic clients.
var (
	Snapshots  = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "snapshots"}
	Migrations = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "migrations"}
	TimePoints = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "timepoints"}
	Rewinds    = schema.GroupVersionResource{Group: Group, Version: Version, Resource: "rewinds"}
)

// Resource is the resource of kind, one of this package's: Snapshot,
// Migration, TimePoint or Rewind.
func Resource(kind string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: strings.ToLower(kind) + "s"}
}

// Phase is where an operation is. A new one has none.
type Phase string

const (
	Running   Phase = "Running"
	Succeeded Phase = "Succeeded"
	Failed    Phase = "Failed"
)

// Done says whether an operation in phase p is over.
func (p Phase) Done() bool { return p == Succeeded || p == Failed }

// Target names the pod an operation is on, in the operation's own
// namespace, and its container: which may be left out in a pod with one.
type Target struct {
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"`
}

// Status is what every operation reports.
type Status struct {
	Phase Phase `json:"phase,omitempty"`
	// Node is the pod's node when the operation started.
	Node string `json:"node,omitempty"`
	// Message says why it failed, or what it is doing.
	Message        string       `json:"message,omitempty"`
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// Snapshot pushes a running pod's container to a registry as an image
// that pods run, restoring its processes (docs/snapshot-images.md). The
// pod keeps running.
type Snapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SnapshotSpec   `json:"spec"`
	Status            SnapshotStatus `json:"status,omitempty"`
}

type SnapshotSpec struct {
	Target `json:",inline"`
	// Image is the reference to push to. Without it, the image goes to
	// Registry, or else to the registry gcr-operator keeps Snapshots in
	// (-snapshot-registry), as NAMESPACE/POD tagged with the Snapshot's
	// name. Status.Image is what was pushed, by digest, either way.
	Image string `json:"image,omitempty"`
	// Registry is the repository (host[:port]/path) to put the image in
	// when Image is not given, instead of gcr-operator's own.
	Registry string `json:"registry,omitempty"`
	// PushSecret names a Secret of type kubernetes.io/dockerconfigjson,
	// in the Snapshot's namespace, with credentials for Image's registry.
	// Without it, the node's own are used (root's docker login there).
	PushSecret string `json:"pushSecret,omitempty"`
	// PlainHTTP talks to the registry without TLS.
	PlainHTTP bool `json:"plainHTTP,omitempty"`
}

type SnapshotStatus struct {
	Status `json:",inline"`
	// Image is the image pushed, by digest.
	Image string `json:"image,omitempty"`
}

// Migration moves a running pod to another node, its processes carrying
// on there (docs/migration.md). The pod's replacement takes its name if
// it has a StatefulSet, and its own otherwise.
type Migration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MigrationSpec   `json:"spec"`
	Status            MigrationStatus `json:"status,omitempty"`
}

type MigrationSpec struct {
	Pod string `json:"pod"`
	// Node is the node to move it to. Without, the scheduler picks one
	// other than its own.
	Node string `json:"node,omitempty"`
	// ThroughRegistry moves it through a checkpoint in gcr-operator's
	// registry (its -registry) rather than node to node: seconds of
	// downtime, but nothing needed from the source node once pushed. A
	// StatefulSet's pod always moves this way.
	ThroughRegistry bool `json:"throughRegistry,omitempty"`
	// Mode is how its memory moves node to node: PreCopy (the default)
	// or PostCopy (docs/migration.md). Not through a registry.
	Mode MigrationMode `json:"mode,omitempty"`
	// Container is the container to move, of a pod with several; without,
	// the one the pod's gcr.dev/migrate-container annotation names. The
	// pod's other containers start anew on the new node.
	Container string `json:"container,omitempty"`
}

// MigrationMode is when a migration's memory moves to the target.
type MigrationMode string

const (
	// PreCopy copies the memory while the pod runs, then freezes it for
	// what changed meanwhile: should anything fail before the target
	// runs, the pod carries on where it was. Where the kernel cannot
	// track which pages changed (arm64), all of it moves in the freeze.
	PreCopy MigrationMode = "PreCopy"
	// PostCopy runs the target before its memory moves: the shortest
	// pause, whatever the memory, but until all of it arrives the pod
	// needs both nodes and the connection between them, and is lost if
	// one goes.
	PostCopy MigrationMode = "PostCopy"
)

type MigrationStatus struct {
	Status `json:",inline"`
	// TargetNode is where it moved to, and Target the pod that runs it
	// there.
	TargetNode string `json:"targetNode,omitempty"`
	Target     string `json:"target,omitempty"`
	// Downtime is how long its processes were stopped.
	Downtime string `json:"downtime,omitempty"`
	// MemoryMoved, after a post-copy migration, is when the last of its
	// memory reached the target, from the freeze.
	MemoryMoved string `json:"memoryMoved,omitempty"`
}

// TimePoint takes a point on the timeline of a container that has time
// travel on (gcr.dev/time-travel), to rewind it to (docs/time-travel.md).
type TimePoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TimePointSpec   `json:"spec"`
	Status            TimePointStatus `json:"status,omitempty"`
}

type TimePointSpec struct {
	Target `json:",inline"`
	// Label is shown on the timeline beside the point.
	Label string `json:"label,omitempty"`
}

type TimePointStatus struct {
	Status `json:",inline"`
	// Point is the point's number on the container's timeline, which a
	// Rewind names.
	Point *int `json:"point,omitempty"`
}

// Rewind takes a container back to a point on its timeline: its
// processes and files are as they were then.
type Rewind struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RewindSpec `json:"spec"`
	Status            Status     `json:"status,omitempty"`
}

type RewindSpec struct {
	Target `json:",inline"`
	// Point is the point's number on the timeline: a TimePoint's
	// status.point, or one gcr took itself (gcr.dev/time-travel: execve).
	Point int `json:"point"`
}

// FromUnstructured converts u, read with a dynamic client, to out, one of
// this package's types.
func FromUnstructured(u *unstructured.Unstructured, out any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
		return fmt.Errorf("%s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
	}
	return nil
}

// ToUnstructured converts in, one of this package's types, for a dynamic
// client.
func ToUnstructured(in any) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(in)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}
