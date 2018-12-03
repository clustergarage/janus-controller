package januscontroller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	janusv1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
)

func TestCalculateStatus(t *testing.T) {
	jg := newJanusGuard("foo", jgMatchedLabel)
	jgStatusTests := []struct {
		name                     string
		jg                       *janusv1alpha1.JanusGuard
		filteredPods             []*corev1.Pod
		expectedJanusGuardStatus janusv1alpha1.JanusGuardStatus
	}{{
		"no matching pods",
		jg,
		[]*corev1.Pod{},
		janusv1alpha1.JanusGuardStatus{
			ObservablePods: 0,
			GuardedPods:    0,
		},
	}, {
		"1 matching pod",
		jg,
		[]*corev1.Pod{
			newPod("bar", jg, corev1.PodRunning, true, false),
		},
		janusv1alpha1.JanusGuardStatus{
			ObservablePods: 1,
			GuardedPods:    0,
		},
	}, {
		"1 matching, annotated pod",
		jg,
		[]*corev1.Pod{
			newPod("bar", jg, corev1.PodRunning, true, true),
		},
		janusv1alpha1.JanusGuardStatus{
			ObservablePods: 1,
			GuardedPods:    1,
		},
	}}

	for _, test := range jgStatusTests {
		janusGuardStatus := calculateStatus(test.jg, test.filteredPods, nil)
		if !reflect.DeepEqual(janusGuardStatus, test.expectedJanusGuardStatus) {
			t.Errorf("%s: unexpected JanusGuard status: expected %+v, got %+v", test.name, test.expectedJanusGuardStatus, janusGuardStatus)
		}
	}
}

func TestUpdateAnnotations(t *testing.T) {
	jg := newJanusGuard("foo", jgMatchedLabel)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)

	updateAnnotations(nil, map[string]string{JanusGuardAnnotationKey: jg.Name}, pod)
	if got, want := len(pod.Annotations), 1; got != want {
		t.Errorf("unexpected pod annotations, expected %v, got %v", want, got)
	}

	updateAnnotations([]string{JanusGuardAnnotationKey}, nil, pod)
	if got, want := len(pod.Annotations), 0; got != want {
		t.Errorf("unexpected pod annotations, expected %v, got %v", want, got)
	}
}

func TestGetPodContainerIDs(t *testing.T) {
	jg := newJanusGuard("foo", jgMatchedLabel)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)

	cids := getPodContainerIDs(pod)
	if got, want := len(cids), 0; got != want {
		t.Errorf("unexpected container ids, expected %v, got %v", want, got)
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{ContainerID: "abc123"}}
	cids = getPodContainerIDs(pod)
	if got, want := len(cids), 1; got != want {
		t.Errorf("unexpected container ids, expected %v, got %v", want, got)
	}
	if cids[0] != "abc123" {
		t.Errorf("expected container id to match %v", cids[0])
	}
}
