package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// JanusGuard is a specification for a JanusGuard resource.
type JanusGuard struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JanusGuardSpec   `json:"spec"`
	Status JanusGuardStatus `json:"status"`
}

// JanusGuardSpec is the spec for a JanusGuard resource.
type JanusGuardSpec struct {
	Selector  *metav1.LabelSelector `json:"selector" protobuf:"bytes,1,req,name=selector"`
	Subjects  []*JanusGuardSubject  `json:"subjects" protobuf:"bytes,2,rep,name=subjects"`
	LogFormat string                `json:"logFormat,omitempty" protobuf:"bytes,3,opt,name=logFormat"`
}

// JanusGuardSubject is the spec for a JanusGuardSubject resource.
type JanusGuardSubject struct {
	Allow   []string          `json:"allow" protobuf:"bytes,1,rep,name=allow"`
	Deny    []string          `json:"deny" protobuf:"bytes,2,rep,name=deny"`
	Events  []string          `json:"events" protobuf:"bytes,3,rep,name=events"`
	OnlyDir bool              `json:"onlyDir,omitempty" protobuf: "bytes,4,,opt,name=onlyDir"`
	Tags    map[string]string `json:"tags,omitempty" protobuf:"bytes,5,rep,name=tags"`
}

// JanusGuardStatus is the status for a JanusGuard resource.
type JanusGuardStatus struct {
	ObservedGeneration int64 `json:"observedGeneration"`

	ObservablePods int32 `json:"observablePods"`
	GuardedPods    int32 `json:"guardedPods"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// JanusGuardList is a list of JanusGuard resources.
type JanusGuardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []JanusGuard `json:"items"`
}
