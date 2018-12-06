package januscontroller

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"testing"
	"time"

	gomock "github.com/golang/mock/gomock"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	kubeinformers "k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"
	. "k8s.io/kubernetes/pkg/controller/testutil"

	janusv1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
	janusclientset "clustergarage.io/janus-controller/pkg/client/clientset/versioned"
	"clustergarage.io/janus-controller/pkg/client/clientset/versioned/fake"
	informers "clustergarage.io/janus-controller/pkg/client/informers/externalversions"
	pb "github.com/clustergarage/janus-proto/golang"
	pbmock "github.com/clustergarage/janus-proto/golang/mock"
)

const (
	janusdSvcPort = 12345

	jgGroup    = "januscontroller.clustergarage.io"
	jgVersion  = "v1alpha1"
	jgResource = "janusguards"
	jgKind     = "JanusGuard"
	jgHostURL  = "fakeurl:50051"

	podVersion  = "v1"
	podResource = "pods"
	podNodeName = "fakenode"
	podHostIP   = "fakehost"
	podIP       = "fakepod"

	epName = "fakeendpoint"

	interval = 100 * time.Millisecond
	timeout  = 60 * time.Second
)

var (
	jgMatchedLabel    = map[string]string{"foo": "bar"}
	jgNonMatchedLabel = map[string]string{"foo": "baz"}

	jgAnnotated = func(jg *janusv1alpha1.JanusGuard) map[string]string {
		return map[string]string{JanusGuardAnnotationKey: jg.Name}
	}
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

type fixture struct {
	t *testing.T

	client     *fake.Clientset
	kubeclient *kubefake.Clientset
	// Objects to put in the store.
	jgLister        []*janusv1alpha1.JanusGuard
	podLister       []*corev1.Pod
	endpointsLister []*corev1.Endpoints
	// Informer factories.
	kubeinformers  kubeinformers.SharedInformerFactory
	janusinformers informers.SharedInformerFactory
	// Actions expected to happen on the client.
	kubeactions []core.Action
	actions     []core.Action
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object
	objects     []runtime.Object
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.t = t
	f.kubeobjects = []runtime.Object{}
	f.objects = []runtime.Object{}
	return f
}

func (f *fixture) newJanusGuardController(kubeclient clientset.Interface, client janusclientset.Interface,
	janusdConnection *janusdConnection) *JanusGuardController {

	if kubeclient == nil {
		kubeclient = kubefake.NewSimpleClientset(f.kubeobjects...)
	}
	f.kubeclient = kubeclient.(*kubefake.Clientset)
	if client == nil {
		client = fake.NewSimpleClientset(f.objects...)
	}
	f.client = client.(*fake.Clientset)

	f.kubeinformers = kubeinformers.NewSharedInformerFactory(kubeclient, controller.NoResyncPeriodFunc())
	f.janusinformers = informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())

	jgc := NewJanusGuardController(kubeclient, client,
		f.janusinformers.Januscontroller().V1alpha1().JanusGuards(),
		f.kubeinformers.Core().V1().Pods(),
		f.kubeinformers.Core().V1().Endpoints(),
		janusdConnection)

	jgc.jgListerSynced = alwaysReady
	jgc.podListerSynced = alwaysReady
	jgc.endpointsListerSynced = alwaysReady
	jgc.recorder = &record.FakeRecorder{}
	jgc.backoff = retry.DefaultBackoff

	for _, pod := range f.podLister {
		f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Add(pod)
	}
	for _, ep := range f.endpointsLister {
		f.kubeinformers.Core().V1().Endpoints().Informer().GetIndexer().Add(ep)
	}
	for _, jg := range f.jgLister {
		f.janusinformers.Januscontroller().V1alpha1().JanusGuards().Informer().GetIndexer().Add(jg)
	}
	return jgc
}

func (f *fixture) waitForPodExpectationFulfillment(jgc *JanusGuardController, jgKey string, pod *corev1.Pod) {
	// Wait for sync, CreationObserved via RetryOnConflict loop.
	if err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		_, err := jgc.kubeclientset.CoreV1().Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		podExp, _, err := jgc.expectations.GetExpectations(jgKey)
		if podExp == nil {
			return false, err
		}
		return podExp.Fulfilled(), err
	}); err != nil {
		f.t.Errorf("No expectations found for JanusGuard")
	}
}

func newJanusGuard(name string, selectorMap map[string]string) *janusv1alpha1.JanusGuard {
	return &janusv1alpha1.JanusGuard{
		TypeMeta: metav1.TypeMeta{
			APIVersion: janusv1alpha1.SchemeGroupVersion.String(),
			Kind:       jgKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: janusNamespace,
		},
		Spec: janusv1alpha1.JanusGuardSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selectorMap},
		},
	}
}

func newPod(name string, jg *janusv1alpha1.JanusGuard, status corev1.PodPhase, matchLabels bool, jgGuarded bool) *corev1.Pod {
	var conditions []corev1.PodCondition
	if status == corev1.PodRunning {
		condition := corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}
		conditions = append(conditions, condition)
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name,
			Namespace: jg.Namespace,
			Labels: func() map[string]string {
				if matchLabels {
					return jgMatchedLabel
				}
				return jgNonMatchedLabel
			}(),
			Annotations: func() map[string]string {
				if jgGuarded {
					return jgAnnotated(jg)
				}
				return nil
			}(),
		},
		Spec: corev1.PodSpec{NodeName: podNodeName},
		Status: corev1.PodStatus{
			Phase:      status,
			Conditions: conditions,
			HostIP:     podHostIP,
		},
	}
}

func newPodList(name string, jg *janusv1alpha1.JanusGuard, store cache.Store, count int, status corev1.PodPhase,
	labelMap map[string]string, jgGuarded bool) *corev1.PodList {

	pods := []corev1.Pod{}
	for i := 0; i < count; i++ {
		pod := newPod(fmt.Sprintf("%s%d", name, i), jg, status, false, jgGuarded)
		pod.ObjectMeta.Labels = labelMap
		if store != nil {
			store.Add(pod)
		}
		pods = append(pods, *pod)
	}
	return &corev1.PodList{Items: pods}
}

func newDaemonPod(name string, jg *janusv1alpha1.JanusGuard) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: jg.Namespace,
			Labels:    janusdSelector,
		},
		Spec: corev1.PodSpec{NodeName: podNodeName},
		Status: corev1.PodStatus{
			HostIP: podHostIP,
			PodIP:  podIP,
		},
	}
}

func newEndpoint(name string, pod *corev1.Pod) *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: janusNamespace,
		},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{
				IP:        epName,
				TargetRef: &corev1.ObjectReference{Name: pod.Name},
			}},
			Ports: []corev1.EndpointPort{{
				Name: janusdSvcPortName,
				Port: janusdSvcPort,
			}},
		}},
	}
}

func newMockJanusdClient(ctrl *gomock.Controller) *janusdConnection {
	client := pbmock.NewMockJanusdClient(ctrl)
	conn, _ := NewJanusdConnection(jgHostURL, grpc.WithInsecure(), client)
	return conn
}

func stubGetGuardState(ctrl *gomock.Controller, conn *janusdConnection, ret *pb.JanusdHandle) {
	var client *pbmock.MockJanusdClient
	client = conn.client.(*pbmock.MockJanusdClient)
	stream := pbmock.NewMockJanusd_GetGuardStateClient(ctrl)
	stream.EXPECT().Recv().Return(ret, nil)
	stream.EXPECT().Recv().Return(nil, io.EOF)
	client.EXPECT().GetGuardState(gomock.Any(), gomock.Any()).Return(stream, nil)
}

func stubCreateGuard(ctrl *gomock.Controller, conn *janusdConnection, ret *pb.JanusdHandle) {
	var client *pbmock.MockJanusdClient
	client = conn.client.(*pbmock.MockJanusdClient)
	client.EXPECT().CreateGuard(gomock.Any(), gomock.Any()).Return(ret, nil)
}

func stubDestroyGuard(ctrl *gomock.Controller, conn *janusdConnection) {
	var client *pbmock.MockJanusdClient
	client = conn.client.(*pbmock.MockJanusdClient)
	client.EXPECT().DestroyGuard(gomock.Any(), gomock.Any()).Return(&pb.Empty{}, nil)
}

func (f *fixture) runController(jgc *JanusGuardController, jgKey string, expectError bool) {
	err := jgc.syncHandler(jgKey)
	if !expectError && err != nil {
		f.t.Errorf("error syncing jg: %v", err)
	} else if expectError && err == nil {
		f.t.Error("expected error syncing jg, got nil")
	}

	f.verifyActions()
	if f.kubeclient == nil {
		return
	}
	f.verifyKubeActions()
}

func (f *fixture) verifyActions() {
	actions := filterInformerActions(f.client.Actions())
	for i, action := range actions {
		if len(f.actions) < i+1 {
			f.t.Errorf("%d unexpected actions: %+v", len(actions)-len(f.actions), actions[i:])
			break
		}
		expectedAction := f.actions[i]
		checkAction(expectedAction, action, f.t)
	}
	if len(f.actions) > len(actions) {
		f.t.Errorf("%d additional expected actions:%+v", len(f.actions)-len(actions), f.actions[len(actions):])
	}
}

func (f *fixture) verifyKubeActions() {
	kubeactions := filterInformerActions(f.kubeclient.Actions())
	for i, action := range kubeactions {
		if len(f.kubeactions) < i+1 {
			f.t.Errorf("%d unexpected actions: %+v", len(kubeactions)-len(f.kubeactions), kubeactions[i:])
			break
		}
		expectedAction := f.kubeactions[i]
		checkAction(expectedAction, action, f.t)
	}
	if len(f.kubeactions) > len(kubeactions) {
		f.t.Errorf("%d additional expected actions:%+v", len(f.kubeactions)-len(kubeactions), f.kubeactions[len(kubeactions):])
	}
}

// checkAction verifies that expected and actual actions are equal and both
// have same attached resources.
func checkAction(expected, actual core.Action, t *testing.T) {
	if !(expected.Matches(actual.GetVerb(), actual.GetResource().Resource) &&
		actual.GetSubresource() == expected.GetSubresource()) {
		t.Errorf("Expected\n\t%#v\ngot\n\t%#v", expected, actual)
		return
	}
	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		t.Errorf("Action has wrong type. Expected: %t. Got: %t", expected, actual)
		return
	}

	switch a := actual.(type) {
	case core.CreateAction:
		e, _ := expected.(core.CreateAction)
		expObject := e.GetObject()
		object := a.GetObject()
		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintDiff(expObject, object))
		}
	case core.UpdateAction:
		e, _ := expected.(core.UpdateAction)
		expObject := e.GetObject()
		object := a.GetObject()
		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintDiff(expObject, object))
		}
	case core.PatchAction:
		e, _ := expected.(core.PatchAction)
		expPatch := e.GetPatch()
		patch := a.GetPatch()
		if !reflect.DeepEqual(expPatch, patch) {
			t.Errorf("Action %s %s has wrong patch\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintDiff(expPatch, patch))
		}
	}
}

// filterInformerActions filters list and watch actions for testing resources.
// Since list and watch don't change resource state we can filter it to lower
// noise level in our tests.
func filterInformerActions(actions []core.Action) []core.Action {
	ret := []core.Action{}
	for _, action := range actions {
		if len(action.GetNamespace()) == 0 &&
			(action.Matches("list", "janusguards") ||
				action.Matches("watch", "janusguards") ||
				action.Matches("list", "pods") ||
				action.Matches("watch", "pods") ||
				action.Matches("list", "endpoints") ||
				action.Matches("watch", "endpoints")) {
			continue
		}
		ret = append(ret, action)
	}
	return ret
}

func (f *fixture) syncHandlerSendJanusGuardName(jgc *JanusGuardController, jg *janusv1alpha1.JanusGuard,
	received chan string) func(key string) error {

	return func(key string) error {
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			f.t.Errorf("Error splitting key: %v", err)
		}
		jgSpec, err := jgc.jgLister.JanusGuards(namespace).Get(name)
		if err != nil {
			f.t.Errorf("Expected to find janus guard under key %v: %v", key, err)
		}
		received <- jgSpec.Name
		return nil
	}
}

func (f *fixture) syncHandlerCheckJanusGuardSynced(jgc *JanusGuardController, jg *janusv1alpha1.JanusGuard,
	received chan string) func(key string) error {

	return func(key string) error {
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			f.t.Errorf("Error splitting key: %v", err)
		}
		jgSpec, err := jgc.jgLister.JanusGuards(namespace).Get(name)
		if err != nil {
			f.t.Errorf("Expected to find janus guard under key %v: %v", key, err)
		}
		if !apiequality.Semantic.DeepDerivative(jgSpec, jg) {
			f.t.Errorf("\nExpected %#v,\nbut got %#v", jg, jgSpec)
		}
		close(received)
		return nil
	}
}

func (f *fixture) expectUpdateJanusGuardStatusAction(jg *janusv1alpha1.JanusGuard) {
	action := core.NewUpdateAction(schema.GroupVersionResource{
		Group:    jgGroup,
		Version:  jgVersion,
		Resource: jgResource,
	}, jg.Namespace, jg)
	action.Subresource = "status"
	f.actions = append(f.actions, action)
}

func (f *fixture) expectGetJanusGuardAction(jg *janusv1alpha1.JanusGuard) {
	f.actions = append(f.actions, core.NewGetAction(schema.GroupVersionResource{
		Group:    jgGroup,
		Resource: jgResource,
		Version:  jgVersion,
	}, jg.Namespace, jg.Name))
}

func (f *fixture) expectUpdateJanusGuardAction(jg *janusv1alpha1.JanusGuard) {
	action := core.NewUpdateAction(schema.GroupVersionResource{
		Group:    jgGroup,
		Resource: jgResource,
		Version:  jgVersion,
	}, jg.Namespace, jg)
	action.Subresource = "status"
	f.actions = append(f.actions, action)
}

type FakeJGExpectations struct {
	*controller.ControllerExpectations
	satisfied    bool
	expSatisfied func()
}

func (fe FakeJGExpectations) SatisfiedExpectations(controllerKey string) bool {
	fe.expSatisfied()
	return fe.satisfied
}

// shuffle returns a new shuffled list of container controllers.
func shuffle(controllers []*janusv1alpha1.JanusGuard) []*janusv1alpha1.JanusGuard {
	numControllers := len(controllers)
	randIndexes := rand.Perm(numControllers)
	shuffled := make([]*janusv1alpha1.JanusGuard, numControllers)
	for i := 0; i < numControllers; i++ {
		shuffled[i] = controllers[randIndexes[i]]
	}
	return shuffled
}

func TestSyncJanusGuardDoesNothing(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, false, false)
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)
	jgc := f.newJanusGuardController(nil, nil, nil)

	f.runController(jgc, GetKey(jg, t), false)
}

func TestLocaljanusdConnection(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, false, false)
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	fc := newMockJanusdClient(ctrl)
	stubGetGuardState(ctrl, fc, &pb.JanusdHandle{})
	jgc := f.newJanusGuardController(nil, nil, fc)

	f.runController(jgc, GetKey(jg, t), false)
}

func TestWatchControllers(t *testing.T) {
	f := newFixture(t)
	fakeWatch := watch.NewFake()
	client := fake.NewSimpleClientset()
	client.PrependWatchReactor("janusguards", core.DefaultWatchReactor(fakeWatch, nil))
	jgc := f.newJanusGuardController(nil, client, nil)

	stopCh := make(chan struct{})
	defer close(stopCh)
	f.janusinformers.Start(stopCh)

	var jg janusv1alpha1.JanusGuard
	received := make(chan string)
	// The update sent through the fakeWatcher should make its way into the
	// workqueue, and eventually into the syncHandler. The handler validates
	// the received controller and closes the received channel to indicate that
	// the test can finish.
	jgc.syncHandler = func(key string) error {
		obj, exists, err := f.janusinformers.Januscontroller().V1alpha1().JanusGuards().Informer().GetIndexer().GetByKey(key)
		if !exists || err != nil {
			t.Errorf("Expected to find janus guard under key %v", key)
		}
		jgSpec := *obj.(*janusv1alpha1.JanusGuard)
		if !apiequality.Semantic.DeepDerivative(jgSpec, jg) {
			t.Errorf("Expected %#v, but got %#v", jg, jgSpec)
		}
		close(received)
		return nil
	}
	// Start only the JanusGuard guard and the workqueue, send a watch event,
	// and make sure it hits the sync method.
	go wait.Until(jgc.runWorker, 10*time.Millisecond, stopCh)

	jg.Name = "foo"
	fakeWatch.Add(&jg)

	select {
	case <-received:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("unexpected timeout from result channel")
	}
}

func TestUpdateControllers(t *testing.T) {
	f := newFixture(t)
	jg1 := newJanusGuard("foo", jgMatchedLabel)
	jg2 := *jg1
	jg2.Name = "bar"
	f.jgLister = append(f.jgLister, jg1, &jg2)
	f.objects = append(f.objects, jg1, &jg2)
	pod := newPod("bar", jg1, corev1.PodRunning, true, false)
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)
	jgc := f.newJanusGuardController(nil, nil, nil)

	received := make(chan string)
	jgc.syncHandler = f.syncHandlerSendJanusGuardName(jgc, jg1, received)

	stopCh := make(chan struct{})
	defer close(stopCh)

	go wait.Until(jgc.runWorker, 10*time.Millisecond, stopCh)

	jg2.Spec.LogFormat = "{foo} {bar}"
	jg2.Spec.Subjects = []*janusv1alpha1.JanusGuardSubject{{
		Allow:  []string{"/foo"},
		Deny:   []string{"/bar"},
		Events: []string{"baz"},
	}}
	jgc.updateJanusGuard(jg1, &jg2)
	expected := sets.NewString(jg2.Name)
	for _, name := range expected.List() {
		t.Logf("Expecting update for %+v", name)
		select {
		case got := <-received:
			if !expected.Has(got) {
				t.Errorf("Expected keys %#v got %v", expected, got)
			}
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected update notifications for janus guards")
		}
	}
}

func TestWatchPods(t *testing.T) {
	f := newFixture(t)
	fakeWatch := watch.NewFake()
	kubeclient := kubefake.NewSimpleClientset()
	kubeclient.PrependWatchReactor("pods", core.DefaultWatchReactor(fakeWatch, nil))
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	jgc := f.newJanusGuardController(kubeclient, nil, nil)

	received := make(chan string)
	// The pod update sent through the fakeWatcher should figure out the
	// managing JanusGuard and send it into the syncHandler.
	jgc.syncHandler = f.syncHandlerCheckJanusGuardSynced(jgc, jg, received)

	stopCh := make(chan struct{})
	defer close(stopCh)

	// Start only the pod watcher and the workqueue, send a watch event, and
	// make sure it hits the sync method for the right JanusGuard.
	go f.kubeinformers.Core().V1().Pods().Informer().Run(stopCh)
	go jgc.Run(1, stopCh)

	pods := newPodList("bar", jg, nil, 1, corev1.PodRunning, jgMatchedLabel, false)
	pod := pods.Items[0]
	pod.Status.Phase = corev1.PodFailed
	fakeWatch.Add(&pod)

	select {
	case <-received:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("unexpected timeout from result channel")
	}
}

func TestAddDaemonPod(t *testing.T) {
	f := newFixture(t)
	fakeWatch := watch.NewFake()
	kubeclient := kubefake.NewSimpleClientset()
	kubeclient.PrependWatchReactor("pods", core.DefaultWatchReactor(fakeWatch, nil))
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	jgc := f.newJanusGuardController(kubeclient, nil, nil)

	received := make(chan string)
	// The pod update sent through the fakeWatcher should figure out the
	// managing JanusGuard and send it into the syncHandler.
	jgc.syncHandler = f.syncHandlerCheckJanusGuardSynced(jgc, jg, received)

	stopCh := make(chan struct{})
	defer close(stopCh)

	// Start only the pod watcher and the workqueue, send a watch event, and
	// make sure it hits the sync method for the right JanusGuard.
	go f.kubeinformers.Core().V1().Pods().Informer().Run(stopCh)
	go jgc.Run(1, stopCh)

	pod := newPod("bar", jg, corev1.PodRunning, true, true)
	fakeWatch.Add(pod)

	daemon := newDaemonPod("baz", jg)
	fakeWatch.Add(daemon)

	select {
	case <-received:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("unexpected timeout from result channel")
	}
}

func TestAddPodBeingDeleted(t *testing.T) {
	f := newFixture(t)
	fakeWatch := watch.NewFake()
	kubeclient := kubefake.NewSimpleClientset()
	kubeclient.PrependWatchReactor("pods", core.DefaultWatchReactor(fakeWatch, nil))
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)
	jgc := f.newJanusGuardController(kubeclient, nil, nil)

	received := make(chan string)
	// The pod update sent through the fakeWatcher should figure out the
	// managing JanusGuard and send it into the syncHandler.
	jgc.syncHandler = f.syncHandlerCheckJanusGuardSynced(jgc, jg, received)

	stopCh := make(chan struct{})
	defer close(stopCh)

	// Start only the pod watcher and the workqueue, send a watch event, and
	// make sure it hits the sync method for the right JanusGuard.
	go f.kubeinformers.Core().V1().Pods().Informer().Run(stopCh)
	go jgc.Run(1, stopCh)

	fakeWatch.Add(pod)

	select {
	case <-received:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("unexpected timeout from result channel")
	}
}

func TestUpdatePods(t *testing.T) {
	f := newFixture(t)
	jg1 := newJanusGuard("foo", jgMatchedLabel)
	jg2 := *jg1
	jg2.Spec.Selector = &metav1.LabelSelector{MatchLabels: jgNonMatchedLabel}
	jg2.Name = "bar"
	f.jgLister = append(f.jgLister, jg1, &jg2)
	f.objects = append(f.objects, jg1, &jg2)
	jgc := f.newJanusGuardController(nil, nil, nil)

	received := make(chan string)
	jgc.syncHandler = f.syncHandlerSendJanusGuardName(jgc, jg1, received)

	stopCh := make(chan struct{})
	defer close(stopCh)

	go wait.Until(jgc.runWorker, 10*time.Millisecond, stopCh)

	// case 1: Pod without JanusD guard and new ResourceVersion should enqueue
	// update.
	pod1 := newPodList("bar", jg1, f.kubeinformers.Core().V1().Pods().Informer().GetIndexer(),
		1, corev1.PodRunning, jgMatchedLabel, false).Items[0]
	pod1.ResourceVersion = "1"
	pod2 := pod1
	pod2.Labels = jgNonMatchedLabel
	pod2.ResourceVersion = "2"
	jgc.updatePod(&pod1, &pod2)
	expected := sets.NewString(jg2.Name)
	for _, name := range expected.List() {
		t.Logf("Expecting update for %+v", name)
		select {
		case got := <-received:
			if !expected.Has(got) {
				t.Errorf("Expected keys %#v got %v", expected, got)
			}
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Expected update notifications for janus guards")
		}
	}

	// case 2: Pod without JanusD guard and same ResourceVersion should not
	// enqueue update.
	pod1 = *newPod("bar", jg1, corev1.PodRunning, true, false)
	pod1.ResourceVersion = "2"
	pod1.Labels = jgNonMatchedLabel
	pod2 = pod1
	pod2.ResourceVersion = "2"
	jgc.updatePod(&pod1, &pod2)
	t.Logf("Not expecting update for %+v", jg2.Name)
	select {
	case got := <-received:
		t.Errorf("Not expecting update for %v", got)
	case <-time.After(time.Millisecond):
	}
}

func TestDeleteDaemonPod(t *testing.T) {
	f := newFixture(t)
	fakeWatch := watch.NewFake()
	kubeclient := kubefake.NewSimpleClientset()
	kubeclient.PrependWatchReactor("pods", core.DefaultWatchReactor(fakeWatch, nil))
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	jgc := f.newJanusGuardController(kubeclient, nil, nil)

	received := make(chan string)
	// The pod update sent through the fakeWatcher should figure out the
	// managing JanusGuard and send it into the syncHandler.
	jgc.syncHandler = f.syncHandlerSendJanusGuardName(jgc, jg, received)

	stopCh := make(chan struct{})
	defer close(stopCh)

	// Start only the pod watcher and the workqueue, send a watch event, and
	// make sure it hits the sync method for the right JanusGuard.
	go f.kubeinformers.Core().V1().Pods().Informer().Run(stopCh)
	go jgc.Run(1, stopCh)

	pod := newPod("bar", jg, corev1.PodRunning, true, false)
	fakeWatch.Add(pod)

	daemon := newDaemonPod("baz", jg)
	ep := newEndpoint(janusdService, daemon)
	f.kubeinformers.Core().V1().Endpoints().Informer().GetIndexer().Add(ep)
	fakeWatch.Add(daemon)

	select {
	case <-received:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("unexpected timeout from result channel")
	}

	fakeWatch.Delete(daemon)

	annotationMux.RLock()
	defer annotationMux.RUnlock()
	if _, found := pod.Annotations[JanusGuardAnnotationKey]; found {
		t.Errorf("Expected pod annotations to be updated %#v", pod.Name)
	}
}

func TestDeleteFinalStateUnknown(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, false, true)
	jgc := f.newJanusGuardController(nil, nil, nil)

	received := make(chan string)
	jgc.syncHandler = func(key string) error {
		received <- key
		return nil
	}
	// The DeletedFinalStateUnknown object should cause the JanusGuard manager
	// to insert the controller matching the selectors of the deleted pod into
	// the work queue.
	jgc.deletePod(cache.DeletedFinalStateUnknown{
		Key: "foo",
		Obj: pod,
	})
	go jgc.runWorker()

	expected := GetKey(jg, t)
	select {
	case key := <-received:
		if key != expected {
			t.Errorf("Unexpected sync all for JanusGuards %v, expected %v", key, expected)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("Processing DeleteFinalStateUnknown took longer than expected")
	}
}

func TestControllerUpdateRequeue(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	jg.Status = janusv1alpha1.JanusGuardStatus{ObservablePods: 2}
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	client := fake.NewSimpleClientset(f.objects...)
	client.PrependReactor("update", "janusguards", func(action core.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		return true, nil, errors.New("failed to update status")
	})
	jgc := f.newJanusGuardController(nil, client, nil)

	// This server should force a requeue of the controller because it fails to
	// update status.ObservablePods.
	newPodList("bar", jg, f.kubeinformers.Core().V1().Pods().Informer().GetIndexer(),
		1, corev1.PodRunning, jgMatchedLabel, false)

	// Enqueue once. Then process it. Disable rate-limiting for this.
	jgc.workqueue = workqueue.NewRateLimitingQueue(workqueue.NewMaxOfRateLimiter())
	jgc.enqueueJanusGuard(jg)
	jgc.processNextWorkItem()
	// It should have been requeued.
	if got, want := jgc.workqueue.Len(), 1; got != want {
		t.Errorf("queue.Len() = %v, want %v", got, want)
	}
}

func TestControllerUpdateWithFailure(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	jgc := f.newJanusGuardController(nil, nil, nil)

	jgc.workqueue.AddRateLimited(nil)
	jgc.processNextWorkItem()
	// It should have errored.
	if got, want := jgc.workqueue.Len(), 0; got != want {
		t.Errorf("queue.Len() = %v, want %v", got, want)
	}
}

func TestControllerUpdateStatusWithFailure(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	client := fake.NewSimpleClientset(f.objects...)
	client.PrependReactor("get", "janusguards", func(action core.Action) (bool, runtime.Object, error) {
		return true, jg, nil
	})
	client.PrependReactor("*", "*", func(action core.Action) (bool, runtime.Object, error) {
		return true, &janusv1alpha1.JanusGuard{}, fmt.Errorf("Fake error")
	})
	f.newJanusGuardController(nil, client, nil)

	numObservablePods := int32(10)
	expectedJG := jg.DeepCopy()
	expectedJG.Status.ObservablePods = numObservablePods
	f.expectUpdateJanusGuardAction(expectedJG)
	f.expectGetJanusGuardAction(expectedJG)

	newStatus := janusv1alpha1.JanusGuardStatus{ObservablePods: numObservablePods}
	updateJanusGuardStatus(f.client.JanuscontrollerV1alpha1().JanusGuards(jg.Namespace), jg, newStatus)
	f.verifyActions()
}

// TestJanusGuardSyncExpectations tests that a pod cannot sneak in between
// counting active pods and checking expectations.
func TestJanusGuardSyncExpectations(t *testing.T) {
	f := newFixture(t)
	jgc := f.newJanusGuardController(nil, nil, nil)

	jg := newJanusGuard("foo", jgMatchedLabel)
	f.janusinformers.Januscontroller().V1alpha1().JanusGuards().Informer().GetIndexer().Add(jg)
	pods := newPodList("bar", jg, nil, 2, corev1.PodPending, jgMatchedLabel, false)
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Add(&pods.Items[0])
	postExpectationsPod := pods.Items[1]

	jgc.expectations = controller.NewUIDTrackingControllerExpectations(FakeJGExpectations{
		controller.NewControllerExpectations(), true, func() {
			// If we check active pods before checking expectataions, the
			// JanusGuard will create a new watcher because it doesn't see this
			// pod, but has fulfilled its expectations.
			f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Add(&postExpectationsPod)
		},
	})

	f.expectUpdateJanusGuardStatusAction(jg)
	jgc.syncJanusGuard(GetKey(jg, t))
}

func TestJanusGuardSyncToCreateGuarder(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)
	daemon := newDaemonPod("baz", jg)
	ep := newEndpoint(janusdService, daemon)
	f.podLister = append(f.podLister, pod, daemon)
	f.endpointsLister = append(f.endpointsLister, ep)
	f.kubeobjects = append(f.kubeobjects, pod, daemon, ep)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	handle := &pb.JanusdHandle{NodeName: podNodeName}
	fc := newMockJanusdClient(ctrl)
	stubGetGuardState(ctrl, fc, handle)
	jgc := f.newJanusGuardController(nil, nil, fc)

	stubCreateGuard(ctrl, fc, handle)
	jgc.syncJanusGuard(GetKey(jg, t))

	jgKey, err := controller.KeyFunc(jg)
	if err != nil {
		f.t.Errorf("Couldn't get key for object %#v: %v", jg, err)
	}
	f.waitForPodExpectationFulfillment(jgc, jgKey, pod)
}

func TestJanusGuardSyncToDestroyGuarder(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{ContainerID: "abc123"}}
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	handle := &pb.JanusdHandle{
		NodeName: podNodeName,
		PodName:  "bar",
	}
	fc := newMockJanusdClient(ctrl)
	fc.handle = handle
	stubGetGuardState(ctrl, fc, handle)
	jgc := f.newJanusGuardController(nil, nil, fc)

	stubDestroyGuard(ctrl, fc)
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	jgc.syncJanusGuard(GetKey(jg, t))
}

func TestDeleteControllerAndExpectations(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)
	daemon := newDaemonPod("baz", jg)
	ep := newEndpoint(janusdService, daemon)
	f.podLister = append(f.podLister, pod, daemon)
	f.endpointsLister = append(f.endpointsLister, ep)
	f.kubeobjects = append(f.kubeobjects, pod, daemon, ep)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	handle := &pb.JanusdHandle{NodeName: podNodeName}
	fc := newMockJanusdClient(ctrl)
	stubGetGuardState(ctrl, fc, handle)
	jgc := f.newJanusGuardController(nil, nil, fc)

	stubCreateGuard(ctrl, fc, handle)
	// This should set expectations for the JanusGuard
	jgc.syncJanusGuard(GetKey(jg, t))

	// Get the JanusGuard key.
	jgKey, err := controller.KeyFunc(jg)
	if err != nil {
		t.Errorf("Couldn't get key for object %#v: %v", jg, err)
	}
	f.waitForPodExpectationFulfillment(jgc, jgKey, pod)

	// This is to simulate a concurrent addPod, that has a handle on the
	// expectations as the controller deletes it.
	podExp, exists, err := jgc.expectations.GetExpectations(jgKey)
	if !exists || err != nil {
		t.Errorf("No expectations found for JanusGuard")
	}

	f.janusinformers.Januscontroller().V1alpha1().JanusGuards().Informer().GetIndexer().Delete(jg)
	jgc.syncJanusGuard(GetKey(jg, t))
	if !podExp.Fulfilled() {
		t.Errorf("Found expectations, expected none since the JanusGuard has been deleted.")
	}

	// This should have no effect, since we've deleted the JanusGuard.
	podExp.Add(-1, 0)
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Replace(make([]interface{}, 0), "0")
	jgc.syncJanusGuard(GetKey(jg, t))
}

func TestDeletionTimestamp(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod1 := newPodList("bar", jg, nil, 1, corev1.PodRunning, jgMatchedLabel, true).Items[0]
	pod1.ResourceVersion = "1"
	pod1.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	jgc := f.newJanusGuardController(nil, nil, nil)

	jgKey, err := controller.KeyFunc(jg)
	if err != nil {
		t.Errorf("Couldn't get key for object %#v: %v", jg, err)
	}
	jgc.expectations.ExpectDeletions(jgKey, []string{controller.PodKey(&pod1)})

	// A pod added with a deletion timestamp should decrement deletions, not
	// creations.
	jgc.addPod(&pod1)

	queueJG, _ := jgc.workqueue.Get()
	if queueJG != jgKey {
		t.Fatalf("Expected to find key %v in queue, found %v", jgKey, queueJG)
	}
	jgc.workqueue.Done(jgKey)

	podExp, exists, err := jgc.expectations.GetExpectations(jgKey)
	if !exists || err != nil || !podExp.Fulfilled() {
		t.Fatalf("Wrong expectations %#v", podExp)
	}

	// An update from no deletion timestamp to having one should be treated as
	// a deletion.
	pod2 := newPodList("baz", jg, nil, 1, corev1.PodPending, jgMatchedLabel, false).Items[0]
	pod2.ResourceVersion = "2"
	jgc.expectations.ExpectDeletions(jgKey, []string{controller.PodKey(&pod1)})
	jgc.updatePod(&pod2, &pod1)

	queueJG, _ = jgc.workqueue.Get()
	if queueJG != jgKey {
		t.Fatalf("Expected to find key %v in queue, found %v", jgKey, queueJG)
	}
	jgc.workqueue.Done(jgKey)

	podExp, exists, err = jgc.expectations.GetExpectations(jgKey)
	if !exists || err != nil || !podExp.Fulfilled() {
		t.Fatalf("Wrong expectations %#v", podExp)
	}

	// An update to the pod (including an update to the deletion timestamp)
	// should not be counted as a second delete.
	pod3 := newPod("qux", jg, corev1.PodRunning, true, true)
	jgc.expectations.ExpectDeletions(jgKey, []string{controller.PodKey(pod3)})
	pod2.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pod2.ResourceVersion = "2"
	pod2.Labels = jgNonMatchedLabel
	jgc.updatePod(&pod2, &pod1)

	podExp, exists, err = jgc.expectations.GetExpectations(jgKey)
	if !exists || err != nil || podExp.Fulfilled() {
		t.Fatalf("Wrong expectations %#v", podExp)
	}

	// A pod with a non-nil deletion timestamp should also be ignored by the
	// delete handler, because it's already been counted in the update.
	jgc.deletePod(&pod1)
	podExp, exists, err = jgc.expectations.GetExpectations(jgKey)
	if !exists || err != nil || podExp.Fulfilled() {
		t.Fatalf("Wrong expectations %#v", podExp)
	}

	// Deleting the second pod should clear expectations.
	jgc.deletePod(pod3)

	queueJG, _ = jgc.workqueue.Get()
	if queueJG != jgKey {
		t.Fatalf("Expected to find key %v in queue, found %v", jgKey, queueJG)
	}
	jgc.workqueue.Done(jgKey)

	podExp, exists, err = jgc.expectations.GetExpectations(jgKey)
	if !exists || err != nil || !podExp.Fulfilled() {
		t.Fatalf("Wrong expectations %#v", podExp)
	}
}

func TestOverlappingJanusGuards(t *testing.T) {
	f := newFixture(t)
	// Create 10 JanusGuards, shuffled them randomly and insert them into the
	// JanusGuard controller's store. All use the same CreationTimestamp.
	timestamp := metav1.Date(2000, time.January, 0, 0, 0, 0, 0, time.Local)
	var controllers []*janusv1alpha1.JanusGuard
	for i := 1; i < 10; i++ {
		jg := newJanusGuard(fmt.Sprintf("jg%d", i), jgMatchedLabel)
		jg.CreationTimestamp = timestamp
		controllers = append(controllers, jg)
	}
	shuffledControllers := shuffle(controllers)
	for i := range shuffledControllers {
		f.jgLister = append(f.jgLister, shuffledControllers[i])
		f.objects = append(f.objects, shuffledControllers[i])
	}
	// Add a pod and make sure only the corresponding JanusGuard is synced.
	// Pick a JG in the middle since the old code used to sort by name if all
	// timestamps were equal.
	jg := controllers[3]
	pod := newPodList("bar", jg, nil, 1, corev1.PodRunning, jgMatchedLabel, true).Items[0]
	f.podLister = append(f.podLister, &pod)
	f.kubeobjects = append(f.kubeobjects, &pod)
	jgc := f.newJanusGuardController(nil, nil, nil)

	jgKey := GetKey(jg, t)
	jgc.addPod(&pod)

	queueJG, _ := jgc.workqueue.Get()
	if queueJG != jgKey {
		t.Fatalf("Expected to find key %v in queue, found %v", jgKey, queueJG)
	}
}

func TestPodControllerLookup(t *testing.T) {
	f := newFixture(t)
	jgc := f.newJanusGuardController(nil, nil, nil)

	testCases := []struct {
		inJGs       []*janusv1alpha1.JanusGuard
		pod         *corev1.Pod
		outJGName   string
		expectError bool
	}{{
		// Pods without labels don't match any JanusGuards.
		inJGs: []*janusv1alpha1.JanusGuard{{
			ObjectMeta: metav1.ObjectMeta{Name: "lorem"},
		}},
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: metav1.NamespaceAll,
			},
		},
		outJGName: "",
	}, {
		// Matching labels, not namespace.
		inJGs: []*janusv1alpha1.JanusGuard{{
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
			Spec: janusv1alpha1.JanusGuardSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
		}},
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "lorem",
				Namespace: "ipsum",
				Labels:    map[string]string{"foo": "bar"},
			}},
		outJGName: "",
	}, {
		// Matching namespace and labels returns the key to the JanusGuard, not
		// the JanusGuard name.
		inJGs: []*janusv1alpha1.JanusGuard{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bar",
				Namespace: "ipsum",
			},
			Spec: janusv1alpha1.JanusGuardSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
		}},
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "lorem",
				Namespace: "ipsum",
				Labels:    map[string]string{"foo": "bar"},
			},
		},
		outJGName: "bar",
	}, {
		// Pod with invalid labelSelector causes an error.
		inJGs: []*janusv1alpha1.JanusGuard{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bar",
				Namespace: "ipsum",
			},
			Spec: janusv1alpha1.JanusGuardSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"/foo": ""},
				},
			},
		}},
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "lorem",
				Namespace: "ipsum",
				Labels:    map[string]string{"foo": "bar"},
			},
		},
		outJGName:   "",
		expectError: true,
	}, {
		// More than one JanusGuard selected for a pod creates an error.
		inJGs: []*janusv1alpha1.JanusGuard{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bar",
				Namespace: "ipsum",
			},
			Spec: janusv1alpha1.JanusGuardSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
		}, {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "baz",
				Namespace: "ipsum",
			},
			Spec: janusv1alpha1.JanusGuardSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"foo": "bar"},
				},
			},
		}},
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "lorem",
				Namespace: "ipsum",
				Labels:    map[string]string{"foo": "bar"},
			},
		},
		outJGName:   "",
		expectError: true,
	}}

	for _, tc := range testCases {
		for _, jg := range tc.inJGs {
			f.janusinformers.Januscontroller().V1alpha1().JanusGuards().Informer().GetIndexer().Add(jg)
		}
		if jgs := jgc.getPodJanusGuards(tc.pod); jgs != nil {
			if len(jgs) > 1 && tc.expectError {
				continue
			} else if len(jgs) != 1 {
				t.Errorf("len(jgs) = %v, want %v", len(jgs), 1)
				continue
			}
			jg := jgs[0]
			if tc.outJGName != jg.Name {
				t.Errorf("Got janus guard %+v expected %+v", jg.Name, tc.outJGName)
			}
		} else if tc.outJGName != "" {
			t.Errorf("Expected a janus guard %v pod %v, found none", tc.outJGName, tc.pod.Name)
		}
	}
}

func TestGetPodKeys(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod1 := newPod("bar", jg, corev1.PodRunning, true, true)
	pod2 := newPod("baz", jg, corev1.PodRunning, true, true)
	f.podLister = append(f.podLister, pod1, pod2)
	f.kubeobjects = append(f.kubeobjects, pod1, pod2)
	f.newJanusGuardController(nil, nil, nil)

	tests := []struct {
		name            string
		pods            []*corev1.Pod
		expectedPodKeys []string
	}{{
		"len(pods) = 0 (i.e., pods = nil)",
		[]*corev1.Pod{},
		[]string{},
	}, {
		"len(pods) > 0",
		[]*corev1.Pod{pod1, pod2},
		[]string{"janus/bar", "janus/baz"},
	}}

	for _, test := range tests {
		podKeys := getPodKeys(test.pods)
		if len(podKeys) != len(test.expectedPodKeys) {
			t.Errorf("%s: unexpected keys for pods to delete, expected %v, got %v", test.name, test.expectedPodKeys, podKeys)
		}
		for i := 0; i < len(podKeys); i++ {
			if podKeys[i] != test.expectedPodKeys[i] {
				t.Errorf("%s: unexpected keys for pods to delete, expected %v, got %v", test.name, test.expectedPodKeys, podKeys)
			}
		}
	}
}

func TestUpdatePodOnceValid(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping updatePodOnceValid in short mode")
	}

	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	fc := newMockJanusdClient(ctrl)
	jgc := f.newJanusGuardController(nil, nil, fc)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar",
			Namespace: jg.Namespace,
		},
		Spec: corev1.PodSpec{NodeName: ""},
		Status: corev1.PodStatus{
			HostIP:            "",
			ContainerStatuses: []corev1.ContainerStatus{{}},
		},
	}
	f.podLister = append(f.podLister, pod)
	// Failure: host name/ip not available
	jgc.updatePodOnceValid(pod.Name, jg)
	if fc.handle != nil {
		t.Errorf("expected handle to be nil: %v", fc.handle)
	}

	pod.Spec.NodeName = podNodeName
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Update(pod)
	// Failure: pod container id not available
	jgc.updatePodOnceValid(pod.Name, jg)
	if fc.handle != nil {
		t.Errorf("expected handle to be nil: %v", fc.handle)
	}

	pod.Status.HostIP = podHostIP
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Update(pod)
	// Failure: available pod container count does not match ready
	jgc.updatePodOnceValid(pod.Name, jg)
	if fc.handle != nil {
		t.Errorf("expected handle to be nil: %v", fc.handle)
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{ContainerID: "abc123"}}
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Update(pod)
	// Failure: available pod container count does not match ready
	jgc.updatePodOnceValid(pod.Name, jg)
	if fc.handle != nil {
		t.Errorf("expected handle to be nil: %v", fc.handle)
	}

	pod.Spec.Containers = []corev1.Container{{Name: "baz"}}
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Update(pod)
	// No-op: pod is being deleted
	jgc.updatePodOnceValid(pod.Name, jg)

	pod.DeletionTimestamp = nil
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Update(pod)
	handle := &pb.JanusdHandle{NodeName: podNodeName}
	stubCreateGuard(ctrl, fc, handle)
	// Successful call to CreateGuard
	jgc.updatePodOnceValid(pod.Name, jg)
	if !reflect.DeepEqual(fc.handle, handle) {
		t.Errorf("Handle does not match\nDiff:\n %s", diff.ObjectGoPrintDiff(fc.handle, handle))
	}
}

func TestGetHostURL(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	pod := newPod("bar", jg, corev1.PodRunning, true, false)
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)
	jgc := f.newJanusGuardController(nil, nil, nil)

	if hostURL, err := jgc.getHostURL(pod); err == nil {
		f.t.Errorf("expected hostURL to be in error state: %v", hostURL)
	}
	pod.Status.PodIP = podIP
	if hostURL, err := jgc.getHostURL(pod); err == nil {
		t.Errorf("expected hostURL to be in error state: %v", hostURL)
	}

	daemon := newDaemonPod("baz", jg)
	ep := newEndpoint(janusdService, daemon)
	f.kubeinformers.Core().V1().Endpoints().Informer().GetIndexer().Add(ep)
	if hostURL, err := jgc.getHostURL(pod); err == nil {
		t.Errorf("expected hostURL to be in error state: %v", hostURL)
	}
	ep.Subsets[0].Addresses[0].TargetRef.Name = pod.Name
	if hostURL, err := jgc.getHostURL(pod); err != nil {
		t.Errorf("expected hostURL to be valid: %v", hostURL)
	}
}

func TestGetHostURLFromSiblingPod(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	pod := newPod("bar", jg, corev1.PodRunning, true, false)
	f.podLister = append(f.podLister, pod)
	f.kubeobjects = append(f.kubeobjects, pod)
	jgc := f.newJanusGuardController(nil, nil, nil)

	if hostURL, err := jgc.getHostURLFromSiblingPod(pod); err == nil {
		t.Errorf("expected hostURL to be in error state: %v", hostURL)
	}
	pod.Status.PodIP = podIP
	if hostURL, err := jgc.getHostURLFromSiblingPod(pod); err == nil {
		t.Errorf("expected hostURL to be in error state: %v", hostURL)
	}

	daemon := newDaemonPod("baz", jg)
	ep := newEndpoint(janusdService, daemon)
	f.kubeinformers.Core().V1().Pods().Informer().GetIndexer().Add(daemon)
	f.kubeinformers.Core().V1().Endpoints().Informer().GetIndexer().Add(ep)
	if hostURL, err := jgc.getHostURLFromSiblingPod(pod); err != nil {
		t.Errorf("expected hostURL to be valid: %v", hostURL)
	}
}

func TestGetJanusGuardSubjects(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	jg.Spec.Subjects = []*janusv1alpha1.JanusGuardSubject{{
		Allow:  []string{"/foo"},
		Deny:   []string{"/bar"},
		Events: []string{"baz"},
	}}
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	jgc := f.newJanusGuardController(nil, nil, nil)

	subjects := jgc.getJanusGuardSubjects(jg)
	if got, want := len(subjects), 1; got != want {
		t.Errorf("unexpected guard subjects, expected %v, got %v", want, got)
	}
	expSubject := &pb.JanusGuardSubject{
		Allow: []string{"/foo"},
		Deny:  []string{"/bar"},
		Event: []string{"baz"},
	}
	if !reflect.DeepEqual(expSubject, subjects[0]) {
		t.Errorf("Subject does not match\nDiff:\n %s", diff.ObjectGoPrintDiff(expSubject, subjects[0]))
	}
}

func TestGuardStates(t *testing.T) {
	f := newFixture(t)
	jg := newJanusGuard("foo", jgMatchedLabel)
	f.jgLister = append(f.jgLister, jg)
	f.objects = append(f.objects, jg)
	pod := newPod("bar", jg, corev1.PodRunning, true, true)
	daemon := newDaemonPod("baz", jg)
	ep := newEndpoint(janusdService, daemon)
	f.podLister = append(f.podLister, pod, daemon)
	f.endpointsLister = append(f.endpointsLister, ep)
	f.kubeobjects = append(f.kubeobjects, pod, daemon, ep)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	fc := newMockJanusdClient(ctrl)
	stubGetGuardState(ctrl, fc, &pb.JanusdHandle{PodName: daemon.Name})
	jgc := f.newJanusGuardController(nil, nil, fc)

	guardStates, _ := jgc.getGuardStates()
	if got, want := len(guardStates), 1; got != want {
		t.Errorf("unexpected guard states, expected %v, got %v", want, got)
	}

	if jgc.isPodInGuardState(pod, guardStates) == true {
		t.Errorf("did not expect pod to be in guard states: %v", pod.Name)
	}
	if jgc.isPodInGuardState(daemon, guardStates) == false {
		t.Errorf("expected pod to be in guard states: %v", daemon.Name)
	}
}
