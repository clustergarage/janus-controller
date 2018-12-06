package januscontroller

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	errorsutil "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"

	janusv1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
	clientset "clustergarage.io/janus-controller/pkg/client/clientset/versioned"
	janusscheme "clustergarage.io/janus-controller/pkg/client/clientset/versioned/scheme"
	informers "clustergarage.io/janus-controller/pkg/client/informers/externalversions/januscontroller/v1alpha1"
	listers "clustergarage.io/janus-controller/pkg/client/listers/januscontroller/v1alpha1"
	pb "github.com/clustergarage/janus-proto/golang"
)

const (
	januscontrollerAgentName = "janus-controller"
	janusNamespace           = "janus"
	janusdService            = "janusd-svc"
	janusdSvcPortName        = "grpc"

	// JanusGuardAnnotationKey value to annotate a pod being guarded by a
	// JanusD daemon.
	JanusGuardAnnotationKey = "clustergarage.io/janus-guard"

	// SuccessSynced is used as part of the Event 'reason' when a JanusGuard
	// is synced.
	SuccessSynced = "Synced"
	// SuccessAdded is used as part of the Event 'reason' when a JanusGuard
	// is synced.
	SuccessAdded = "Added"
	// SuccessRemoved is used as part of the Event 'reason' when a JanusGuard
	// is synced.
	SuccessRemoved = "Removed"
	// MessageResourceAdded is the message used for an Event fired when a
	// JanusGuard is synced added.
	MessageResourceAdded = "Added JanusD guard on %v"
	// MessageResourceRemoved is the message used for an Event fired when a
	// JanusGuard is synced removed.
	MessageResourceRemoved = "Removed JanusD guard on %v"
	// MessageResourceSynced is the message used for an Event fired when a
	// JanusGuard is synced successfully.
	MessageResourceSynced = "JanusGuard synced successfully"

	// statusUpdateRetries is the number of times we retry updating a
	// JanusGuard's status.
	statusUpdateRetries = 1
	// minReadySeconds
	minReadySeconds = 10
)

var (
	janusdSelector = map[string]string{"daemon": "janusd"}

	// updatePodQueue stores a local queue of pod updates that will ensure pods
	// aren't being updated more than once at a single time. For example: if we
	// get an addPod event for a daemon, which checks any pods that need an
	// update, and another addPod event comes through for the pod that's
	// already being updated, it won't queue again until it finishes processing
	// and removes itself from the queue.
	updatePodQueue sync.Map
)

// JanusGuardController is the controller implementation for JanusGuard
// resources.
type JanusGuardController struct {
	// GroupVersionKind indicates the controller type.
	// Different instances of this struct may handle different GVKs.
	schema.GroupVersionKind

	// kubeclientset is a standard Kubernetes clientset.
	kubeclientset kubernetes.Interface
	// janusclientset is a clientset for our own API group.
	janusclientset clientset.Interface

	// Allow injection of syncJanusGuard.
	syncHandler func(key string) error
	// backoff is the backoff definition for RetryOnConflict.
	backoff wait.Backoff

	// A TTLCache of pod creates/deletes each jg expects to see.
	expectations *controller.UIDTrackingControllerExpectations

	// A store of JanusGuards, populated by the shared informer passed to
	// NewJanusGuardController.
	jgLister listers.JanusGuardLister
	// jgListerSynced returns true if the pod store has been synced at least
	// once. Added as a member to the struct to allow injection for testing.
	jgListerSynced cache.InformerSynced

	// A store of pods, populated by the shared informer passed to
	// NewJanusGuardController.
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod store has been synced at least
	// once. Added as a member to the struct to allow injection for testing.
	podListerSynced cache.InformerSynced

	// A store of endpoints, populated by the shared informer passed to
	// NewJanusGuardController.
	endpointsLister corelisters.EndpointsLister
	// endpointListerSynced returns true if the endpoints store has been synced
	// at least once. Added as a member to the struct to allow injection for
	// testing.
	endpointsListerSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder

	// janusdConnections is a collection of connections we have open to the
	// JanusD server which wrap important functions to add, remove, and get a
	// current up-to-date state of what the daemon thinks it should be
	// guarding.
	janusdConnections sync.Map
	// janusdURL is used to connect to the JanusD gRPC server if daemon is
	// out-of-cluster.
	janusdURL string
}

// NewJanusGuardController returns a new JanusGuard controller.
func NewJanusGuardController(kubeclientset kubernetes.Interface, janusclientset clientset.Interface,
	jgInformer informers.JanusGuardInformer, podInformer coreinformers.PodInformer,
	endpointsInformer coreinformers.EndpointsInformer, janusdConnection *janusdConnection) *JanusGuardController {

	// Create event broadcaster.
	// Add januscontroller types to the default Kubernetes Scheme so Events can
	// be logged for januscontroller types.
	janusscheme.AddToScheme(scheme.Scheme)
	glog.Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: januscontrollerAgentName})

	jgc := &JanusGuardController{
		GroupVersionKind:      appsv1.SchemeGroupVersion.WithKind("JanusGuard"),
		kubeclientset:         kubeclientset,
		janusclientset:        janusclientset,
		expectations:          controller.NewUIDTrackingControllerExpectations(controller.NewControllerExpectations()),
		jgLister:              jgInformer.Lister(),
		jgListerSynced:        jgInformer.Informer().HasSynced,
		podLister:             podInformer.Lister(),
		podListerSynced:       podInformer.Informer().HasSynced,
		endpointsLister:       endpointsInformer.Lister(),
		endpointsListerSynced: endpointsInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "JanusGuards"),
		recorder:              recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when JanusGuard resources change.
	jgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    jgc.enqueueJanusGuard,
		UpdateFunc: jgc.updateJanusGuard,
		DeleteFunc: jgc.enqueueJanusGuard,
	})

	// Set up an event handler for when Pod resources change.
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: jgc.addPod,
		// This invokes the JanusGuard for every pod change, eg: host
		// assignment. Though this might seem like overkill the most frequent
		// pod update is status, and the associated JanusGuard will only list
		// from local storage, so it should be okay.
		UpdateFunc: jgc.updatePod,
		DeleteFunc: jgc.deletePod,
	})

	jgc.syncHandler = jgc.syncJanusGuard
	jgc.backoff = wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
	}

	// If specifying a janusdConnection for a daemon that is located
	// out-of-cluster, initialize the janusd connection here, because we will
	// not receive an addPod event where it is normally initialized.
	jgc.janusdConnections = sync.Map{}
	if janusdConnection != nil {
		jgc.janusdConnections.Store(janusdConnection.hostURL, janusdConnection)
		jgc.janusdURL = janusdConnection.hostURL
	}

	return jgc
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (jgc *JanusGuardController) Run(workers int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer jgc.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches.
	glog.Info("Starting JanusGuard controller")
	defer glog.Info("Shutting down JanusGuard controller")

	// Wait for the caches to be synced before starting workers.
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, jgc.podListerSynced, jgc.endpointsListerSynced, jgc.jgListerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process JanusGuard resources.
	for i := 0; i < workers; i++ {
		go wait.Until(jgc.runWorker, time.Second, stopCh)
	}
	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// updateJanusGuard is a callback for when a JanusGuard is updated.
func (jgc *JanusGuardController) updateJanusGuard(old, new interface{}) {
	oldJG := old.(*janusv1alpha1.JanusGuard)
	newJG := new.(*janusv1alpha1.JanusGuard)

	logFormatChanged := !reflect.DeepEqual(newJG.Spec.LogFormat, oldJG.Spec.LogFormat)
	subjectsChanged := !reflect.DeepEqual(newJG.Spec.Subjects, oldJG.Spec.Subjects)

	if logFormatChanged || subjectsChanged {
		// Add new JanusGuard definitions.
		selector, err := metav1.LabelSelectorAsSelector(newJG.Spec.Selector)
		if err != nil {
			return
		}
		if selectedPods, err := jgc.podLister.Pods(newJG.Namespace).List(selector); err == nil {
			for _, pod := range selectedPods {
				if !podutil.IsPodReady(pod) {
					continue
				}
				go jgc.updatePodOnceValid(pod.Name, newJG)
			}
		}
	}

	jgc.enqueueJanusGuard(newJG)
}

// addPod is called when a pod is created, enqueue the JanusGuard that
// manages it and update its expectations.
func (jgc *JanusGuardController) addPod(obj interface{}) {
	pod := obj.(*corev1.Pod)

	if pod.DeletionTimestamp != nil {
		// On a restart of the controller manager, it's possible a new pod
		// shows up in a state that is already pending deletion. Prevent the
		// pod from being a creation observation.
		jgc.deletePod(pod)
		return
	}

	// If it has a JanusGuard annotation that's all that matters.
	if jgName, found := pod.Annotations[JanusGuardAnnotationKey]; found {
		jg, err := jgc.jgLister.JanusGuards(pod.Namespace).Get(jgName)
		if err != nil {
			return
		}
		jgKey, err := controller.KeyFunc(jg)
		if err != nil {
			return
		}
		glog.V(4).Infof("Pod %s created: %#v.", pod.Name, pod)
		jgc.expectations.CreationObserved(jgKey)
		jgc.enqueueJanusGuard(jg)
		return
	}

	// If this pod is a JanusD pod, we need to first initialize the connection
	// to the gRPC server run on the daemon. Then a check is done on any pods
	// running on the same node as the daemon, if they match our nodeSelector
	// then immediately enqueue the JanusGuard for additions.
	if label, _ := pod.Labels["daemon"]; label == "janusd" {
		var hostURL string
		// Run this function with a retry, to make sure we get a connection to
		// the daemon pod. If we exhaust all attempts, process error
		// accordingly.
		if retryErr := retry.RetryOnConflict(jgc.backoff, func() (err error) {
			po, err := jgc.podLister.Pods(janusNamespace).Get(pod.Name)
			if err != nil {
				err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
					po.Name, errors.New("could not find pod"))
				return err
			}
			hostURL, err = jgc.getHostURL(po)
			if err != nil {
				err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
					po.Name, errors.New("pod host is not available"))
				return err
			}
			// Initialize connection to gRPC server on daemon.
			opts, err := BuildAndStoreDialOptions(TLS, TLSSkipVerify, TLSCACert, TLSClientCert, TLSClientKey, TLSServerName)
			if err != nil {
				return err
			}
			conn, err := NewJanusdConnection(hostURL, opts)
			if err != nil {
				return err
			}
			jgc.janusdConnections.Store(hostURL, conn)
			return err
		}); retryErr != nil {
			return
		}

		allPods, err := jgc.podLister.List(labels.Everything())
		if err != nil {
			return
		}
		for _, po := range allPods {
			if po.Spec.NodeName != pod.Spec.NodeName {
				continue
			}
			jgs := jgc.getPodJanusGuards(po)
			if len(jgs) == 0 {
				continue
			}

			glog.V(4).Infof("Unannotated pod %s found: %#v.", po.Name, po)
			for _, jg := range jgs {
				jgc.enqueueJanusGuard(jg)
			}
		}
		return
	}

	// Get a list of all matching JanusGuards and sync them. Do not observe
	// creation because no controller should be waiting for an orphan.
	jgs := jgc.getPodJanusGuards(pod)
	if len(jgs) == 0 {
		return
	}

	glog.V(4).Infof("Unannotated pod %s found: %#v.", pod.Name, pod)
	for _, jg := range jgs {
		jgc.enqueueJanusGuard(jg)
	}
}

// updatePod is called when a pod is updated. Figure out what JanusGuard(s)
// manage it and wake them up. If the labels of the pod have changed we need to
// jgaken both the old and new JanusGuard. old and new must be *corev1.Pod
// types.
func (jgc *JanusGuardController) updatePod(old, new interface{}) {
	newPod := new.(*corev1.Pod)
	oldPod := old.(*corev1.Pod)

	if newPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pod will always have different
		// ResourceVersions.
		return
	}

	labelChanged := !reflect.DeepEqual(newPod.Labels, oldPod.Labels)
	if newPod.DeletionTimestamp != nil {
		// When a pod is deleted gracefully it's deletion timestamp is first
		// modified to reflect a grace period, and after such time has passed,
		// the kubelet actually deletes it from the store. We receive an update
		// for modification of the deletion timestamp and expect an jg to
		// create more guards asap, not wait until the kubelet actually
		// deletes the pod. This is different from the Phase of a pod changing,
		// because an jg never initiates a phase change, and so is never asleep
		// waiting for the same.
		jgc.deletePod(newPod)
		if labelChanged {
			// We don't need to check the oldPod.DeletionTimestamp because
			// DeletionTimestamp cannot be unset.
			jgc.deletePod(oldPod)
		}
		return
	}

	jgs := jgc.getPodJanusGuards(newPod)
	for _, jg := range jgs {
		jgc.enqueueJanusGuard(jg)
	}
}

// deletePod is called when a pod is deleted. Enqueue the JanusGuard that
// guards the pod and update its expectations. obj could be an *v1.Pod, or a
// DeletionFinalStateUnknown marker item.
func (jgc *JanusGuardController) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which
	// contains the deleted key/value. Note that this value might be stale. If
	// the pod changed labels the new JanusGuard will not be woken up until
	// the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("couldn't get object from tombstone %+v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			runtime.HandleError(fmt.Errorf("tombstone contained object that is not a pod %#v", obj))
			return
		}
	}

	if jgName, found := pod.Annotations[JanusGuardAnnotationKey]; found {
		jg, err := jgc.jgLister.JanusGuards(pod.Namespace).Get(jgName)
		if err != nil {
			return
		}
		jgKey, err := controller.KeyFunc(jg)
		if err != nil {
			return
		}
		glog.V(4).Infof("Annotated pod %s/%s deleted through %v, timestamp %+v: %#v.",
			pod.Namespace, pod.Name, runtime.GetCaller(), pod.DeletionTimestamp, pod)
		jgc.expectations.DeletionObserved(jgKey, controller.PodKey(pod))
		jgc.enqueueJanusGuard(jg)
	}

	// If this pod is a JanusD pod, we need to first destroy the connection to
	// the gRPC server run on the daemon. Then remove relevant JanusGuard
	// annotations from pods on the same node.
	if label, _ := pod.Labels["daemon"]; label == "janusd" {
		hostURL, err := jgc.getHostURL(pod)
		if err != nil {
			return
		}
		// Destroy connections to gRPC server on daemon.
		jgc.janusdConnections.Delete(hostURL)

		allPods, err := jgc.podLister.List(labels.Everything())
		if err != nil {
			return
		}
		for _, po := range allPods {
			if po.Spec.NodeName != pod.Spec.NodeName {
				continue
			}
			//delete(po.Annotations, JanusGuardAnnotationKey)
			updateAnnotations([]string{JanusGuardAnnotationKey}, nil, po)
		}
		glog.V(4).Infof("Daemon pod %s/%s deleted through %v, timestamp %+v: %#v.",
			pod.Namespace, pod.Name, runtime.GetCaller(), pod.DeletionTimestamp, pod)
	}
}

// enqueueJanusGuard takes a JanusGuard resource and converts it into a
// namespace/name string which is then put onto the workqueue. This method
// should not be passed resources of any type other than JanusGuard.
func (jgc *JanusGuardController) enqueueJanusGuard(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		runtime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	jgc.workqueue.AddRateLimited(key)
}

// enqueueJanusGuardAfter performs the same functionality as
// enqueueJanusGuard, except it is enqueued in `after` duration of time.
func (jgc *JanusGuardController) enqueueJanusGuardAfter(obj interface{}, after time.Duration) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		runtime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	jgc.workqueue.AddAfter(key, after)
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (jgc *JanusGuardController) runWorker() {
	for jgc.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (jgc *JanusGuardController) processNextWorkItem() bool {
	obj, shutdown := jgc.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer jgc.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished processing
		// this item. We also must remember to call Forget if we do not want
		// this work item being re-queued. For example, we do not call Forget
		// if a transient error occurs, instead the item is put back on the
		// workqueue and attempted again after a back-off period.
		defer jgc.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the form
		// namespace/name. We do this as the delayed nature of the workqueue
		// means the items in the informer cache may actually be more up to
		// date that when the item was initially put onto the workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call Forget
			// here else we'd go into a loop of attempting to process a work
			// item that is invalid.
			jgc.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("Expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// JanusGuard resource to be synced.
		if err := jgc.syncHandler(key); err != nil {
			return fmt.Errorf("Error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not get
		// queued again until another change happens.
		jgc.workqueue.Forget(obj)
		glog.V(4).Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		jgc.workqueue.AddRateLimited(obj)
		return true
	}
	return true
}

// syncJanusGuard compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the JanusGuard
// resource with the current status of the resource.
func (jgc *JanusGuardController) syncJanusGuard(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing %v %q (%v)", jgc.Kind, key, time.Since(startTime))
	}()

	// Convert the namespace/name string into a distinct namespace and name.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the JanusGuard resource with this namespace/name.
	jg, err := jgc.jgLister.JanusGuards(namespace).Get(name)
	// The JanusGuard resource may no longer exist, in which case we stop
	// processing.
	if errorsutil.IsNotFound(err) {
		// @TODO: cleanup: delete annotations from any pods that have them
		runtime.HandleError(fmt.Errorf("%v '%s' in work queue no longer exists", jgc.Kind, key))
		jgc.expectations.DeleteExpectations(key)
		return nil
	}
	if err != nil {
		return err
	}

	jgNeedsSync := jgc.expectations.SatisfiedExpectations(key)

	// Get the diff between all pods and pods that match the JanusGuard
	// selector.
	var rmPods []*corev1.Pod
	var addPods []*corev1.Pod

	selector, err := metav1.LabelSelectorAsSelector(jg.Spec.Selector)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Error converting pod selector to selector: %v", err))
		return nil
	}
	selectedPods, err := jgc.podLister.Pods(jg.Namespace).List(selector)
	if err != nil {
		return err
	}

	// @TODO: Only get pods with annotation: JanusGuardAnnotationKey.
	allPods, err := jgc.podLister.Pods(jg.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}

	// Get current guard state from JanusD daemon.
	guardStates, err := jgc.getGuardStates()
	if err != nil {
		return err
	}

	for _, pod := range selectedPods {
		if pod.DeletionTimestamp != nil ||
			!podutil.IsPodReady(pod) {
			continue
		}
		if wsFound := jgc.isPodInGuardState(pod, guardStates); !wsFound {
			var found bool
			updatePodQueue.Range(func(k, v interface{}) bool {
				// Check if pod is already in updatePodQueue.
				if pod.Name == k {
					found = true
					return false
				}
				return true
			})
			if !found {
				addPods = append(addPods, pod)
				continue
			}
		}
	}

	for _, pod := range allPods {
		if wsFound := jgc.isPodInGuardState(pod, guardStates); !wsFound {
			continue
		}

		var selFound bool
		for _, po := range selectedPods {
			if pod.Name == po.Name {
				selFound = true
				break
			}
		}
		if pod.DeletionTimestamp != nil || !selFound {
			if value, found := pod.Annotations[JanusGuardAnnotationKey]; found && value == jg.Name {
				rmPods = append(rmPods, pod)
				continue
			}
		}
	}

	var manageSubjectsErr error
	if (jgNeedsSync && jg.DeletionTimestamp == nil) ||
		len(rmPods) > 0 ||
		len(addPods) > 0 {
		manageSubjectsErr = jgc.manageObserverPods(rmPods, addPods, jg)
	}

	jg = jg.DeepCopy()
	newStatus := calculateStatus(jg, selectedPods, manageSubjectsErr)

	// Always updates status as pods come up or die.
	updatedjg, err := updateJanusGuardStatus(jgc.janusclientset.JanuscontrollerV1alpha1().JanusGuards(jg.Namespace), jg, newStatus)
	if err != nil {
		// Multiple things could lead to this update failing. Requeuing the
		// janus guard ensures. Returning an error causes a requeue without
		// forcing a hotloop.
		return err
	}

	// Resync the JanusGuard after MinReadySeconds as a last line of defense
	// to guard against clock-skew.
	if manageSubjectsErr == nil &&
		minReadySeconds > 0 &&
		updatedjg.Status.GuardedPods != int32(len(selectedPods)) {
		jgc.enqueueJanusGuardAfter(updatedjg, time.Duration(minReadySeconds)*time.Second)
	}
	return manageSubjectsErr
}

// manageObserverPods checks and updates observers for the given JanusGuard.
// It will requeue the JanusGuard in case of an error while creating/deleting
// pods.
func (jgc *JanusGuardController) manageObserverPods(rmPods []*corev1.Pod, addPods []*corev1.Pod, jg *janusv1alpha1.JanusGuard) error {
	jgKey, err := controller.KeyFunc(jg)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Couldn't get key for %v %#v: %v", jgc.Kind, jg, err))
		return nil
	}

	if len(rmPods) > 0 {
		jgc.expectations.ExpectDeletions(jgKey, getPodKeys(rmPods))
		glog.Infof("Too many guards for %v %s/%s, deleting %d", jgc.Kind, jg.Namespace, jg.Name, len(rmPods))
	}
	if len(addPods) > 0 {
		jgc.expectations.ExpectCreations(jgKey, len(addPods))
		glog.Infof("Too few guards for %v %s/%s, creating %d", jgc.Kind, jg.Namespace, jg.Name, len(addPods))
	}

	var podsToUpdate []*corev1.Pod

	for _, pod := range rmPods {
		if _, found := pod.Annotations[JanusGuardAnnotationKey]; found {
			cids := getPodContainerIDs(pod)
			if len(cids) > 0 {
				hostURL, err := jgc.getHostURLFromSiblingPod(pod)
				if err != nil {
					return err
				}
				fc, err := jgc.getJanusdConnection(hostURL)
				if err != nil {
					return err
				}
				if fc.handle == nil {
					return fmt.Errorf("janusd connection has no handle %#v", fc)
				}

				if err := fc.RemoveJanusdGuard(&pb.JanusdConfig{
					NodeName: pod.Spec.NodeName,
					PodName:  pod.Name,
					Pid:      fc.handle.Pid,
				}); err != nil {
					return err
				}

				jgc.expectations.DeletionObserved(jgKey, controller.PodKey(pod))
				jgc.recorder.Eventf(jg, corev1.EventTypeNormal, SuccessRemoved, MessageResourceRemoved, pod.Spec.NodeName)
			}
		}

		err := updateAnnotations([]string{JanusGuardAnnotationKey}, nil, pod)
		if err != nil {
			return err
		}
		podsToUpdate = append(podsToUpdate, pod)
	}

	for _, pod := range addPods {
		go jgc.updatePodOnceValid(pod.Name, jg)

		err := updateAnnotations(nil, map[string]string{JanusGuardAnnotationKey: jg.Name}, pod)
		if err != nil {
			return err
		}
		podsToUpdate = append(podsToUpdate, pod)
		// Once updatePodOnceValid is called, put it in an update queue so we
		// don't start another RetryOnConflict while one is already in effect.
		updatePodQueue.Store(pod.Name, true)
	}

	for _, pod := range podsToUpdate {
		updatePodWithRetries(jgc.kubeclientset.CoreV1().Pods(pod.Namespace), jgc.podLister,
			jg.Namespace, pod.Name, func(po *corev1.Pod) error {
				po.Annotations = pod.Annotations
				return nil
			})
	}

	return nil
}

// getPodJanusGuards returns a list of JanusGuards matching the given pod.
func (jgc *JanusGuardController) getPodJanusGuards(pod *corev1.Pod) []*janusv1alpha1.JanusGuard {
	if len(pod.Labels) == 0 {
		glog.V(4).Infof("no JanusGuards found for pod %v because it has no labels", pod.Name)
		return nil
	}

	list, err := jgc.jgLister.JanusGuards(pod.Namespace).List(labels.Everything())
	if err != nil {
		return nil
	}

	var jgs []*janusv1alpha1.JanusGuard
	for _, jg := range list {
		if jg.Namespace != pod.Namespace {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(jg.Spec.Selector)
		if err != nil {
			runtime.HandleError(fmt.Errorf("invalid selector: %v", err))
			return nil
		}
		// If a JanusGuard with a nil or empty selector creeps in, it should
		// match nothing, not everything.
		if selector.Empty() ||
			!selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		jgs = append(jgs, jg)
	}

	if len(jgs) == 0 {
		glog.V(4).Infof("could not find JanusGuard for pod %s in namespace %s with labels: %v", pod.Name, pod.Namespace, pod.Labels)
		return nil
	}
	if len(jgs) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than
		//one item in this list nevertheless constitutes user error.
		runtime.HandleError(fmt.Errorf("user error; more than one %v is selecting pods with labels: %+v", jgc.Kind, pod.Labels))
	}
	return jgs
}

// getPodKeys returns a list of pod key strings from array of pod objects.
func getPodKeys(pods []*corev1.Pod) []string {
	podKeys := make([]string, 0, len(pods))
	for _, pod := range pods {
		podKeys = append(podKeys, controller.PodKey(pod))
	}
	return podKeys
}

// updatePodOnceValid first retries getting the pod hostURL to connect to the
// JanusD gRPC server. Then it retries adding a new JanusD guard by calling
// the gRPC server CreateGuard function; on success it updates the appropriate
// JanusGuard annotations so we can mark it now "guarded".
func (jgc *JanusGuardController) updatePodOnceValid(podName string, jg *janusv1alpha1.JanusGuard) {
	var cids []string
	var nodeName, hostURL string

	// Run this function with a retry, to make sure we get a connection to the
	// daemon pod. If we exhaust all attempts, process error accordingly.
	if retryErr := retry.RetryOnConflict(jgc.backoff, func() (err error) {
		// Be sure to clear all slice elements first in case of a retry.
		cids = cids[:0]

		pod, err := jgc.podLister.Pods(jg.Namespace).Get(podName)
		if err != nil {
			return err
		}
		if pod.Spec.NodeName == "" || pod.Status.HostIP == "" {
			err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
				pod.Name, errors.New("host name/ip not available"))
			return err
		}
		nodeName = pod.Spec.NodeName

		for _, ctr := range pod.Status.ContainerStatuses {
			if ctr.ContainerID == "" {
				err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
					pod.Name, errors.New("pod container id not available"))
				return err
			}
			cids = append(cids, ctr.ContainerID)
		}
		if len(pod.Spec.Containers) != len(cids) {
			err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
				pod.Name, errors.New("available pod container count does not match ready"))
			return err
		}

		var hostErr error
		hostURL, hostErr = jgc.getHostURLFromSiblingPod(pod)
		if hostErr != nil || hostURL == "" {
			err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
				pod.Name, errors.New("pod host is not available"))
		}
		return err
	}); retryErr != nil {
		return
	}

	// Run this function with a retry, to make sure we get a successful
	// response from the daemon. If we exhaust all attempts, process error
	// accordingly.
	if retryErr := retry.RetryOnConflict(jgc.backoff, func() (err error) {
		pod, err := jgc.podLister.Pods(jg.Namespace).Get(podName)
		if err != nil {
			return err
		}
		if pod.DeletionTimestamp != nil {
			return fmt.Errorf("pod is being deleted %v", pod.Name)
		}

		fc, err := jgc.getJanusdConnection(hostURL)
		if err != nil {
			return errorsutil.NewConflict(schema.GroupResource{Resource: "nodes"},
				nodeName, errors.New("failed to get janusd connection"))
		}
		if handle, err := fc.AddJanusdGuard(&pb.JanusdConfig{
			Name:      jg.Name,
			NodeName:  nodeName,
			PodName:   pod.Name,
			Cid:       cids,
			Subject:   jgc.getJanusGuardSubjects(jg),
			LogFormat: jg.Spec.LogFormat,
		}); err == nil {
			fc.handle = handle
		}
		return err
	}); retryErr != nil {
		updatePodWithRetries(jgc.kubeclientset.CoreV1().Pods(jg.Namespace), jgc.podLister,
			jg.Namespace, podName, func(po *corev1.Pod) error {
				pod, err := jgc.podLister.Pods(jg.Namespace).Get(podName)
				if err != nil {
					return err
				}
				po.Annotations = pod.Annotations
				return nil
			})
	}

	go jgc.removePodFromUpdateQueue(podName)

	jgKey, err := controller.KeyFunc(jg)
	if err != nil {
		return
	}
	jgc.expectations.CreationObserved(jgKey)
	jgc.recorder.Eventf(jg, corev1.EventTypeNormal, SuccessAdded, MessageResourceAdded, nodeName)
}

// getHostURL constructs a URL from the pod's hostIP and hard-coded JanusD
// port.  The pod specified in this function is assumed to be a daemon pod.
// If janusdURL was specified to the controller, to connect to an
// out-of-cluster daemon, use this instead.
func (jgc *JanusGuardController) getHostURL(pod *corev1.Pod) (string, error) {
	if jgc.janusdURL != "" {
		return jgc.janusdURL, nil
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("cannot locate janusd pod on node %v", pod.Spec.NodeName)
	}

	endpoint, err := jgc.endpointsLister.Endpoints(janusNamespace).Get(janusdService)
	if err != nil {
		return "", err
	}

	var epIP string
	var epPort int32
	for _, subset := range endpoint.Subsets {
		for _, addr := range subset.Addresses {
			if addr.TargetRef.Name == pod.Name {
				epIP = addr.IP
			}
		}
		for _, port := range subset.Ports {
			if port.Name == janusdSvcPortName {
				epPort = port.Port
			}
		}
	}
	if epIP == "" || epPort == 0 {
		return "", fmt.Errorf("cannot locate endpoint IP or port on node %v", pod.Spec.NodeName)
	}
	return fmt.Sprintf("%s:%d", epIP, epPort), nil
}

// getHostURLFromSiblingPod constructs a URL from a daemon pod running on the
// same host; it uses the daemon pod's hostIP and hard-coded JanusD port. The
// pod specified in this function is assumed to be a non-daemon pod. If
// janusdURL was specified to the controller, to connect to an out-of-cluster
// daemon, use this instead.
func (jgc *JanusGuardController) getHostURLFromSiblingPod(pod *corev1.Pod) (string, error) {
	if jgc.janusdURL != "" {
		return jgc.janusdURL, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: janusdSelector})
	if err != nil {
		return "", err
	}
	daemonPods, err := jgc.podLister.Pods(janusNamespace).List(selector)
	if err != nil {
		return "", err
	}
	for _, daemonPod := range daemonPods {
		if daemonPod.Spec.NodeName == pod.Spec.NodeName {
			hostURL, err := jgc.getHostURL(daemonPod)
			if err != nil {
				return "", err
			}
			return hostURL, nil
		}
	}

	return "", fmt.Errorf("cannot locate janusd pod on node %v", pod.Spec.NodeName)
}

// getJanusGuardSubjects is a helper function that given a JanusGuard,
// constructs a list of Subjects to be used when creating a new JanusD guard.
func (jgc *JanusGuardController) getJanusGuardSubjects(jg *janusv1alpha1.JanusGuard) []*pb.JanusGuardSubject {
	var subjects []*pb.JanusGuardSubject
	for _, s := range jg.Spec.Subjects {
		subjects = append(subjects, &pb.JanusGuardSubject{
			Allow: s.Allow,
			Deny:  s.Deny,
			Event: s.Events,
			Tags:  s.Tags,
		})
	}
	return subjects
}

// getJanusdConnection returns a janusdConnection object given a hostURL.
func (jgc *JanusGuardController) getJanusdConnection(hostURL string) (*janusdConnection, error) {
	if fc, ok := jgc.janusdConnections.Load(hostURL); ok == true {
		return fc.(*janusdConnection), nil
	}
	return nil, fmt.Errorf("could not connect to janusd at hostURL %v", hostURL)
}

// GetGuardStates is a helper function that gets all the current guard states
// from every JanusD pod running in the clutser. This is used in the
// syncHandler to run exactly once each sync.
func (jgc *JanusGuardController) getGuardStates() ([][]*pb.JanusdHandle, error) {
	var guardStates [][]*pb.JanusdHandle

	// If specifying a janusdURL for a daemon that is located out-of-cluster,
	// we assume a single JanusD pod in the cluster; get the guard state of
	// this daemon pod only.
	if jgc.janusdURL != "" {
		fc, err := jgc.getJanusdConnection(jgc.janusdURL)
		if err != nil {
			return nil, err
		}
		ws, err := fc.GetGuardState()
		if err != nil {
			return nil, err
		}
		guardStates = append(guardStates, ws)
		return guardStates, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: janusdSelector})
	if err != nil {
		return nil, err
	}
	daemonPods, err := jgc.podLister.Pods(janusNamespace).List(selector)
	if err != nil {
		return nil, err
	}
	for _, pod := range daemonPods {
		hostURL, err := jgc.getHostURL(pod)
		if err != nil {
			continue
		}
		fc, err := jgc.getJanusdConnection(hostURL)
		if err != nil {
			return nil, err
		}
		ws, err := fc.GetGuardState()
		if err != nil {
			continue
		}
		guardStates = append(guardStates, ws)
	}
	return guardStates, nil
}

// isPodInGuardState is a helper function that given a pod and list of guard
// states, find if pod name appears anywhere in the list.
func (jgc *JanusGuardController) isPodInGuardState(pod *corev1.Pod, guardStates [][]*pb.JanusdHandle) bool {
	var found bool
	for _, guardState := range guardStates {
		for _, ws := range guardState {
			if pod.Name == ws.PodName {
				found = true
				break
			}
		}
	}
	return found
}

// removePodFromUpdateQueue is a helper function that given a pod name, remove
// it from the update pod queue in order to be marked for a new update in the
// future.
func (jgc *JanusGuardController) removePodFromUpdateQueue(podName string) {
	updatePodQueue.Delete(podName)
}
