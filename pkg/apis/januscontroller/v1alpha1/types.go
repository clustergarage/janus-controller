package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// JanusWatcher is a specification for a JanusWatcher resource.
type JanusWatcher struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JanusWatcherSpec   `json:"spec"`
	Status JanusWatcherStatus `json:"status"`
}

// JanusWatcherSpec is the spec for a JanusWatcher resource.
type JanusWatcherSpec struct {
	Selector *metav1.LabelSelector  `json:"selector" protobuf:"bytes,1,req,name=selector"`
	Subjects []*JanusWatcherSubject `json:"subjects" protobuf:"bytes,2,rep,name=subjects"`
}

// JanusWatcherSubject is the spec for a JanusWatcherSubject resource.
type JanusWatcherSubject struct {
	Allow  []string `json:"allow" protobuf:"bytes,1,rep,name=allow"`
	Deny   []string `json:"deny" protobuf:"bytes,1,rep,name=deny"`
	Events []string `json:"events" protobuf:"bytes,2,rep,name=events"`
}

// JanusWatcherStatus is the status for a JanusWatcher resource.
type JanusWatcherStatus struct {
	ObservedGeneration int64 `json:"observedGeneration"`

	ObservablePods int32 `json:"observablePods"`
	WatchedPods    int32 `json:"watchedPods"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// JanusWatcherList is a list of JanusWatcher resources.
type JanusWatcherList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []JanusWatcher `json:"items"`
}
