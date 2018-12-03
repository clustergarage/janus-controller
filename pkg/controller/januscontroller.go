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

	// JanusWatcherAnnotationKey value to annotate a pod being watched by a
	// JanusD daemon.
	JanusWatcherAnnotationKey = "clustergarage.io/janus-watcher"

	// SuccessSynced is used as part of the Event 'reason' when a JanusWatcher
	// is synced.
	SuccessSynced = "Synced"
	// SuccessAdded is used as part of the Event 'reason' when a JanusWatcher
	// is synced.
	SuccessAdded = "Added"
	// SuccessRemoved is used as part of the Event 'reason' when a JanusWatcher
	// is synced.
	SuccessRemoved = "Removed"
	// MessageResourceAdded is the message used for an Event fired when a
	// JanusWatcher is synced added.
	MessageResourceAdded = "Added JanusD watcher on %v"
	// MessageResourceRemoved is the message used for an Event fired when a
	// JanusWatcher is synced removed.
	MessageResourceRemoved = "Removed JanusD watcher on %v"
	// MessageResourceSynced is the message used for an Event fired when a
	// JanusWatcher is synced successfully.
	MessageResourceSynced = "JanusWatcher synced successfully"

	// statusUpdateRetries is the number of times we retry updating a
	// JanusWatcher's status.
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

// JanusWatcherController is the controller implementation for JanusWatcher
// resources.
type JanusWatcherController struct {
	// GroupVersionKind indicates the controller type.
	// Different instances of this struct may handle different GVKs.
	schema.GroupVersionKind

	// kubeclientset is a standard Kubernetes clientset.
	kubeclientset kubernetes.Interface
	// janusclientset is a clientset for our own API group.
	janusclientset clientset.Interface

	// Allow injection of syncJanusWatcher.
	syncHandler func(key string) error
	// backoff is the backoff definition for RetryOnConflict.
	backoff wait.Backoff

	// A TTLCache of pod creates/deletes each jw expects to see.
	expectations *controller.UIDTrackingControllerExpectations

	// A store of JanusWatchers, populated by the shared informer passed to
	// NewJanusWatcherController.
	jwLister listers.JanusWatcherLister
	// jwListerSynced returns true if the pod store has been synced at least
	// once. Added as a member to the struct to allow injection for testing.
	jwListerSynced cache.InformerSynced

	// A store of pods, populated by the shared informer passed to
	// NewJanusWatcherController.
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod store has been synced at least
	// once. Added as a member to the struct to allow injection for testing.
	podListerSynced cache.InformerSynced

	// A store of endpoints, populated by the shared informer passed to
	// NewJanusWatcherController.
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
	// watching.
	janusdConnections sync.Map
	// janusdURL is used to connect to the JanusD gRPC server if daemon is
	// out-of-cluster.
	janusdURL string
}

// NewJanusWatcherController returns a new JanusWatcher controller.
func NewJanusWatcherController(kubeclientset kubernetes.Interface, janusclientset clientset.Interface,
	jwInformer informers.JanusWatcherInformer, podInformer coreinformers.PodInformer,
	endpointsInformer coreinformers.EndpointsInformer, janusdConnection *janusdConnection) *JanusWatcherController {

	// Create event broadcaster.
	// Add januscontroller types to the default Kubernetes Scheme so Events can
	// be logged for januscontroller types.
	janusscheme.AddToScheme(scheme.Scheme)
	glog.Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: januscontrollerAgentName})

	jwc := &JanusWatcherController{
		GroupVersionKind:      appsv1.SchemeGroupVersion.WithKind("JanusWatcher"),
		kubeclientset:         kubeclientset,
		janusclientset:        janusclientset,
		expectations:          controller.NewUIDTrackingControllerExpectations(controller.NewControllerExpectations()),
		jwLister:              jwInformer.Lister(),
		jwListerSynced:        jwInformer.Informer().HasSynced,
		podLister:             podInformer.Lister(),
		podListerSynced:       podInformer.Informer().HasSynced,
		endpointsLister:       endpointsInformer.Lister(),
		endpointsListerSynced: endpointsInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "JanusWatchers"),
		recorder:              recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when JanusWatcher resources change.
	jwInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    jwc.enqueueJanusWatcher,
		UpdateFunc: jwc.updateJanusWatcher,
		DeleteFunc: jwc.enqueueJanusWatcher,
	})

	// Set up an event handler for when Pod resources change.
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: jwc.addPod,
		// This invokes the JanusWatcher for every pod change, eg: host
		// assignment. Though this might seem like overkill the most frequent
		// pod update is status, and the associated JanusWatcher will only list
		// from local storage, so it should be okay.
		UpdateFunc: jwc.updatePod,
		DeleteFunc: jwc.deletePod,
	})

	jwc.syncHandler = jwc.syncJanusWatcher
	jwc.backoff = wait.Backoff{
		Steps:    10,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
	}

	// If specifying a janusdConnection for a daemon that is located
	// out-of-cluster, initialize the janusd connection here, because we will
	// not receive an addPod event where it is normally initialized.
	jwc.janusdConnections = sync.Map{}
	if janusdConnection != nil {
		jwc.janusdConnections.Store(janusdConnection.hostURL, janusdConnection)
		jwc.janusdURL = janusdConnection.hostURL
	}

	return jwc
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (jwc *JanusWatcherController) Run(workers int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer jwc.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches.
	glog.Info("Starting JanusWatcher controller")
	defer glog.Info("Shutting down JanusWatcher controller")

	// Wait for the caches to be synced before starting workers.
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, jwc.podListerSynced, jwc.endpointsListerSynced, jwc.jwListerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process JanusWatcher resources.
	for i := 0; i < workers; i++ {
		go wait.Until(jwc.runWorker, time.Second, stopCh)
	}
	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// updateJanusWatcher is a callback for when a JanusWatcher is updated.
func (jwc *JanusWatcherController) updateJanusWatcher(old, new interface{}) {
	oldJW := old.(*janusv1alpha1.JanusWatcher)
	newJW := new.(*janusv1alpha1.JanusWatcher)

	subjectsChanged := !reflect.DeepEqual(newJW.Spec.Subjects, oldJW.Spec.Subjects)

	if subjectsChanged {
		// Add new JanusWatcher definitions.
		selector, err := metav1.LabelSelectorAsSelector(newJW.Spec.Selector)
		if err != nil {
			return
		}
		if selectedPods, err := jwc.podLister.Pods(newJW.Namespace).List(selector); err == nil {
			for _, pod := range selectedPods {
				if !podutil.IsPodReady(pod) {
					continue
				}
				go jwc.updatePodOnceValid(pod.Name, newJW)
			}
		}
	}

	jwc.enqueueJanusWatcher(newJW)
}

// addPod is called when a pod is created, enqueue the JanusWatcher that
// manages it and update its expectations.
func (jwc *JanusWatcherController) addPod(obj interface{}) {
	pod := obj.(*corev1.Pod)

	if pod.DeletionTimestamp != nil {
		// On a restart of the controller manager, it's possible a new pod
		// shows up in a state that is already pending deletion. Prevent the
		// pod from being a creation observation.
		jwc.deletePod(pod)
		return
	}

	// If it has a JanusWatcher annotation that's all that matters.
	if jwName, found := pod.Annotations[JanusWatcherAnnotationKey]; found {
		jw, err := jwc.jwLister.JanusWatchers(pod.Namespace).Get(jwName)
		if err != nil {
			return
		}
		jwKey, err := controller.KeyFunc(jw)
		if err != nil {
			return
		}
		glog.V(4).Infof("Pod %s created: %#v.", pod.Name, pod)
		jwc.expectations.CreationObserved(jwKey)
		jwc.enqueueJanusWatcher(jw)
		return
	}

	// If this pod is a JanusD pod, we need to first initialize the connection
	// to the gRPC server run on the daemon. Then a check is done on any pods
	// running on the same node as the daemon, if they match our nodeSelector
	// then immediately enqueue the JanusWatcher for additions.
	if label, _ := pod.Labels["daemon"]; label == "janusd" {
		var hostURL string
		// Run this function with a retry, to make sure we get a connection to
		// the daemon pod. If we exhaust all attempts, process error
		// accordingly.
		if retryErr := retry.RetryOnConflict(jwc.backoff, func() (err error) {
			po, err := jwc.podLister.Pods(janusNamespace).Get(pod.Name)
			if err != nil {
				err = errorsutil.NewConflict(schema.GroupResource{Resource: "pods"},
					po.Name, errors.New("could not find pod"))
				return err
			}
			hostURL, err = jwc.getHostURL(po)
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
			jwc.janusdConnections.Store(hostURL, conn)
			return err
		}); retryErr != nil {
			return
		}

		allPods, err := jwc.podLister.List(labels.Everything())
		if err != nil {
			return
		}
		for _, po := range allPods {
			if po.Spec.NodeName != pod.Spec.NodeName {
				continue
			}
			jws := jwc.getPodJanusWatchers(po)
			if len(jws) == 0 {
				continue
			}

			glog.V(4).Infof("Unannotated pod %s found: %#v.", po.Name, po)
			for _, jw := range jws {
				jwc.enqueueJanusWatcher(jw)
			}
		}
		return
	}

	// Get a list of all matching JanusWatchers and sync them. Do not observe
	// creation because no controller should be waiting for an orphan.
	jws := jwc.getPodJanusWatchers(pod)
	if len(jws) == 0 {
		return
	}

	glog.V(4).Infof("Unannotated pod %s found: %#v.", pod.Name, pod)
	for _, jw := range jws {
		jwc.enqueueJanusWatcher(jw)
	}
}

// updatePod is called when a pod is updated. Figure out what JanusWatcher(s)
// manage it and wake them up. If the labels of the pod have changed we need to
// Jwaken both the old and new JanusWatcher. old and new must be *corev1.Pod
// types.
func (jwc *JanusWatcherController) updatePod(old, new interface{}) {
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
		// for modification of the deletion timestamp and expect an jw to
		// create more watchers asap, not wait until the kubelet actually
		// deletes the pod. This is different from the Phase of a pod changing,
		// because an jw never initiates a phase change, and so is never asleep
		// waiting for the same.
		jwc.deletePod(newPod)
		if labelChanged {
			// We don't need to check the oldPod.DeletionTimestamp because
			// DeletionTimestamp cannot be unset.
			jwc.deletePod(oldPod)
		}
		return
	}

	jws := jwc.getPodJanusWatchers(newPod)
	for _, jw := range jws {
		jwc.enqueueJanusWatcher(jw)
	}
}

// deletePod is called when a pod is deleted. Enqueue the JanusWatcher that
// watches the pod and update its expectations. obj could be an *v1.Pod, or a
// DeletionFinalStateUnknown marker item.
func (jwc *JanusWatcherController) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which
	// contains the deleted key/value. Note that this value might be stale. If
	// the pod changed labels the new JanusWatcher will not be woken up until
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

	if jwName, found := pod.Annotations[JanusWatcherAnnotationKey]; found {
		jw, err := jwc.jwLister.JanusWatchers(pod.Namespace).Get(jwName)
		if err != nil {
			return
		}
		jwKey, err := controller.KeyFunc(jw)
		if err != nil {
			return
		}
		glog.V(4).Infof("Annotated pod %s/%s deleted through %v, timestamp %+v: %#v.",
			pod.Namespace, pod.Name, runtime.GetCaller(), pod.DeletionTimestamp, pod)
		jwc.expectations.DeletionObserved(jwKey, controller.PodKey(pod))
		jwc.enqueueJanusWatcher(jw)
	}

	// If this pod is a JanusD pod, we need to first destroy the connection to
	// the gRPC server run on the daemon. Then remove relevant JanusWatcher
	// annotations from pods on the same node.
	if label, _ := pod.Labels["daemon"]; label == "janusd" {
		hostURL, err := jwc.getHostURL(pod)
		if err != nil {
			return
		}
		// Destroy connections to gRPC server on daemon.
		jwc.janusdConnections.Delete(hostURL)

		allPods, err := jwc.podLister.List(labels.Everything())
		if err != nil {
			return
		}
		for _, po := range allPods {
			if po.Spec.NodeName != pod.Spec.NodeName {
				continue
			}
			//delete(po.Annotations, JanusWatcherAnnotationKey)
			updateAnnotations([]string{JanusWatcherAnnotationKey}, nil, po)
		}
		glog.V(4).Infof("Daemon pod %s/%s deleted through %v, timestamp %+v: %#v.",
			pod.Namespace, pod.Name, runtime.GetCaller(), pod.DeletionTimestamp, pod)
	}
}

// enqueueJanusWatcher takes a JanusWatcher resource and converts it into a
// namespace/name string which is then put onto the workqueue. This method
// should not be passed resources of any type other than JanusWatcher.
func (jwc *JanusWatcherController) enqueueJanusWatcher(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		runtime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	jwc.workqueue.AddRateLimited(key)
}

// enqueueJanusWatcherAfter performs the same functionality as
// enqueueJanusWatcher, except it is enqueued in `after` duration of time.
func (jwc *JanusWatcherController) enqueueJanusWatcherAfter(obj interface{}, after time.Duration) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		runtime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	jwc.workqueue.AddAfter(key, after)
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (jwc *JanusWatcherController) runWorker() {
	for jwc.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (jwc *JanusWatcherController) processNextWorkItem() bool {
	obj, shutdown := jwc.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer jwc.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished processing
		// this item. We also must remember to call Forget if we do not want
		// this work item being re-queued. For example, we do not call Forget
		// if a transient error occurs, instead the item is put back on the
		// workqueue and attempted again after a back-off period.
		defer jwc.workqueue.Done(obj)
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
			jwc.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("Expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// JanusWatcher resource to be synced.
		if err := jwc.syncHandler(key); err != nil {
			return fmt.Errorf("Error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not get
		// queued again until another change happens.
		jwc.workqueue.Forget(obj)
		glog.V(4).Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		jwc.workqueue.AddRateLimited(obj)
		return true
	}
	return true
}

// syncJanusWatcher compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the JanusWatcher
// resource with the current status of the resource.
func (jwc *JanusWatcherController) syncJanusWatcher(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing %v %q (%v)", jwc.Kind, key, time.Since(startTime))
	}()

	// Convert the namespace/name string into a distinct namespace and name.
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the JanusWatcher resource with this namespace/name.
	jw, err := jwc.jwLister.JanusWatchers(namespace).Get(name)
	// The JanusWatcher resource may no longer exist, in which case we stop
	// processing.
	if errorsutil.IsNotFound(err) {
		// @TODO: cleanup: delete annotations from any pods that have them
		runtime.HandleError(fmt.Errorf("%v '%s' in work queue no longer exists", jwc.Kind, key))
		jwc.expectations.DeleteExpectations(key)
		return nil
	}
	if err != nil {
		return err
	}

	jwNeedsSync := jwc.expectations.SatisfiedExpectations(key)

	// Get the diff between all pods and pods that match the JanusWatch
	// selector.
	var rmPods []*corev1.Pod
	var addPods []*corev1.Pod

	selector, err := metav1.LabelSelectorAsSelector(jw.Spec.Selector)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Error converting pod selector to selector: %v", err))
		return nil
	}
	selectedPods, err := jwc.podLister.Pods(jw.Namespace).List(selector)
	if err != nil {
		return err
	}

	// @TODO: Only get pods with annotation: JanusWatcherAnnotationKey.
	allPods, err := jwc.podLister.Pods(jw.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}

	// Get current watch state from JanusD daemon.
	watchStates, err := jwc.getWatchStates()
	if err != nil {
		return err
	}

	for _, pod := range selectedPods {
		if pod.DeletionTimestamp != nil ||
			!podutil.IsPodReady(pod) {
			continue
		}
		if wsFound := jwc.isPodInWatchState(pod, watchStates); !wsFound {
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
		if wsFound := jwc.isPodInWatchState(pod, watchStates); !wsFound {
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
			if value, found := pod.Annotations[JanusWatcherAnnotationKey]; found && value == jw.Name {
				rmPods = append(rmPods, pod)
				continue
			}
		}
	}

	var manageSubjectsErr error
	if (jwNeedsSync && jw.DeletionTimestamp == nil) ||
		len(rmPods) > 0 ||
		len(addPods) > 0 {
		manageSubjectsErr = jwc.manageObserverPods(rmPods, addPods, jw)
	}

	jw = jw.DeepCopy()
	newStatus := calculateStatus(jw, selectedPods, manageSubjectsErr)

	// Always updates status as pods come up or die.
	updatedJW, err := updateJanusWatcherStatus(jwc.janusclientset.JanuscontrollerV1alpha1().JanusWatchers(jw.Namespace), jw, newStatus)
	if err != nil {
		// Multiple things could lead to this update failing. Requeuing the
		// janus watcher ensures. Returning an error causes a requeue without
		// forcing a hotloop.
		return err
	}

	// Resync the JanusWatcher after MinReadySeconds as a last line of defense
	// to guard against clock-skew.
	if manageSubjectsErr == nil &&
		minReadySeconds > 0 &&
		updatedJW.Status.WatchedPods != int32(len(selectedPods)) {
		jwc.enqueueJanusWatcherAfter(updatedJW, time.Duration(minReadySeconds)*time.Second)
	}
	return manageSubjectsErr
}

// manageObserverPods checks and updates observers for the given JanusWatcher.
// It will requeue the JanusWatcher in case of an error while creating/deleting
// pods.
func (jwc *JanusWatcherController) manageObserverPods(rmPods []*corev1.Pod, addPods []*corev1.Pod, jw *janusv1alpha1.JanusWatcher) error {
	jwKey, err := controller.KeyFunc(jw)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Couldn't get key for %v %#v: %v", jwc.Kind, jw, err))
		return nil
	}

	if len(rmPods) > 0 {
		jwc.expectations.ExpectDeletions(jwKey, getPodKeys(rmPods))
		glog.Infof("Too many watchers for %v %s/%s, deleting %d", jwc.Kind, jw.Namespace, jw.Name, len(rmPods))
	}
	if len(addPods) > 0 {
		jwc.expectations.ExpectCreations(jwKey, len(addPods))
		glog.Infof("Too few watchers for %v %s/%s, creating %d", jwc.Kind, jw.Namespace, jw.Name, len(addPods))
	}

	var podsToUpdate []*corev1.Pod

	for _, pod := range rmPods {
		if _, found := pod.Annotations[JanusWatcherAnnotationKey]; found {
			cids := getPodContainerIDs(pod)
			if len(cids) > 0 {
				hostURL, err := jwc.getHostURLFromSiblingPod(pod)
				if err != nil {
					return err
				}
				fc, err := jwc.getJanusdConnection(hostURL)
				if err != nil {
					return err
				}
				if fc.handle == nil {
					return fmt.Errorf("janusd connection has no handle %#v", fc)
				}

				if err := fc.RemoveJanusdWatcher(&pb.JanusdConfig{
					NodeName: pod.Spec.NodeName,
					PodName:  pod.Name,
					Pid:      fc.handle.Pid,
				}); err != nil {
					return err
				}

				jwc.expectations.DeletionObserved(jwKey, controller.PodKey(pod))
				jwc.recorder.Eventf(jw, corev1.EventTypeNormal, SuccessRemoved, MessageResourceRemoved, pod.Spec.NodeName)
			}
		}

		err := updateAnnotations([]string{JanusWatcherAnnotationKey}, nil, pod)
		if err != nil {
			return err
		}
		podsToUpdate = append(podsToUpdate, pod)
	}

	for _, pod := range addPods {
		go jwc.updatePodOnceValid(pod.Name, jw)

		err := updateAnnotations(nil, map[string]string{JanusWatcherAnnotationKey: jw.Name}, pod)
		if err != nil {
			return err
		}
		podsToUpdate = append(podsToUpdate, pod)
		// Once updatePodOnceValid is called, put it in an update queue so we
		// don't start another RetryOnConflict while one is already in effect.
		updatePodQueue.Store(pod.Name, true)
	}

	for _, pod := range podsToUpdate {
		updatePodWithRetries(jwc.kubeclientset.CoreV1().Pods(pod.Namespace), jwc.podLister,
			jw.Namespace, pod.Name, func(po *corev1.Pod) error {
				po.Annotations = pod.Annotations
				return nil
			})
	}

	return nil
}

// getPodJanusWatchers returns a list of JanusWatchers matching the given pod.
func (jwc *JanusWatcherController) getPodJanusWatchers(pod *corev1.Pod) []*janusv1alpha1.JanusWatcher {
	if len(pod.Labels) == 0 {
		glog.V(4).Infof("no JanusWatchers found for pod %v because it has no labels", pod.Name)
		return nil
	}

	list, err := jwc.jwLister.JanusWatchers(pod.Namespace).List(labels.Everything())
	if err != nil {
		return nil
	}

	var jws []*janusv1alpha1.JanusWatcher
	for _, jw := range list {
		if jw.Namespace != pod.Namespace {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(jw.Spec.Selector)
		if err != nil {
			runtime.HandleError(fmt.Errorf("invalid selector: %v", err))
			return nil
		}
		// If a JanusWatcher with a nil or empty selector creeps in, it should
		// match nothing, not everything.
		if selector.Empty() ||
			!selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		jws = append(jws, jw)
	}

	if len(jws) == 0 {
		glog.V(4).Infof("could not find JanusWatcher for pod %s in namespace %s with labels: %v", pod.Name, pod.Namespace, pod.Labels)
		return nil
	}
	if len(jws) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than
		//one item in this list nevertheless constitutes user error.
		runtime.HandleError(fmt.Errorf("user error; more than one %v is selecting pods with labels: %+v", jwc.Kind, pod.Labels))
	}
	return jws
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
// JanusD gRPC server. Then it retries adding a new JanusD watcher by calling
// the gRPC server CreateWatch function; on success it updates the appropriate
// JanusWatcher annotations so we can mark it now "watched".
func (jwc *JanusWatcherController) updatePodOnceValid(podName string, jw *janusv1alpha1.JanusWatcher) {
	var cids []string
	var nodeName, hostURL string

	// Run this function with a retry, to make sure we get a connection to the
	// daemon pod. If we exhaust all attempts, process error accordingly.
	if retryErr := retry.RetryOnConflict(jwc.backoff, func() (err error) {
		// Be sure to clear all slice elements first in case of a retry.
		cids = cids[:0]

		pod, err := jwc.podLister.Pods(jw.Namespace).Get(podName)
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
		hostURL, hostErr = jwc.getHostURLFromSiblingPod(pod)
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
	if retryErr := retry.RetryOnConflict(jwc.backoff, func() (err error) {
		pod, err := jwc.podLister.Pods(jw.Namespace).Get(podName)
		if err != nil {
			return err
		}
		if pod.DeletionTimestamp != nil {
			return fmt.Errorf("pod is being deleted %v", pod.Name)
		}

		fc, err := jwc.getJanusdConnection(hostURL)
		if err != nil {
			return errorsutil.NewConflict(schema.GroupResource{Resource: "nodes"},
				nodeName, errors.New("failed to get janusd connection"))
		}
		if handle, err := fc.AddJanusdWatcher(&pb.JanusdConfig{
			Name:     jw.Name,
			NodeName: nodeName,
			PodName:  pod.Name,
			Cid:      cids,
			Subject:  jwc.getJanusWatcherSubjects(jw),
		}); err == nil {
			fc.handle = handle
		}
		return err
	}); retryErr != nil {
		updatePodWithRetries(jwc.kubeclientset.CoreV1().Pods(jw.Namespace), jwc.podLister,
			jw.Namespace, podName, func(po *corev1.Pod) error {
				pod, err := jwc.podLister.Pods(jw.Namespace).Get(podName)
				if err != nil {
					return err
				}
				po.Annotations = pod.Annotations
				return nil
			})
	}

	go jwc.removePodFromUpdateQueue(podName)

	jwKey, err := controller.KeyFunc(jw)
	if err != nil {
		return
	}
	jwc.expectations.CreationObserved(jwKey)
	jwc.recorder.Eventf(jw, corev1.EventTypeNormal, SuccessAdded, MessageResourceAdded, nodeName)
}

// getHostURL constructs a URL from the pod's hostIP and hard-coded JanusD
// port.  The pod specified in this function is assumed to be a daemon pod.
// If janusdURL was specified to the controller, to connect to an
// out-of-cluster daemon, use this instead.
func (jwc *JanusWatcherController) getHostURL(pod *corev1.Pod) (string, error) {
	if jwc.janusdURL != "" {
		return jwc.janusdURL, nil
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("cannot locate janusd pod on node %v", pod.Spec.NodeName)
	}

	endpoint, err := jwc.endpointsLister.Endpoints(janusNamespace).Get(janusdService)
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
func (jwc *JanusWatcherController) getHostURLFromSiblingPod(pod *corev1.Pod) (string, error) {
	if jwc.janusdURL != "" {
		return jwc.janusdURL, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: janusdSelector})
	if err != nil {
		return "", err
	}
	daemonPods, err := jwc.podLister.Pods(janusNamespace).List(selector)
	if err != nil {
		return "", err
	}
	for _, daemonPod := range daemonPods {
		if daemonPod.Spec.NodeName == pod.Spec.NodeName {
			hostURL, err := jwc.getHostURL(daemonPod)
			if err != nil {
				return "", err
			}
			return hostURL, nil
		}
	}

	return "", fmt.Errorf("cannot locate janusd pod on node %v", pod.Spec.NodeName)
}

// getJanusWatcherSubjects is a helper function that given a JanusWatcher,
// constructs a list of Subjects to be used when creating a new JanusD watcher.
func (jwc *JanusWatcherController) getJanusWatcherSubjects(jw *janusv1alpha1.JanusWatcher) []*pb.JanusWatcherSubject {
	var subjects []*pb.JanusWatcherSubject
	for _, s := range jw.Spec.Subjects {
		subjects = append(subjects, &pb.JanusWatcherSubject{
			Allow: s.Allow,
			Deny:  s.Deny,
			Event: s.Events,
		})
	}
	return subjects
}

// getJanusdConnection returns a janusdConnection object given a hostURL.
func (jwc *JanusWatcherController) getJanusdConnection(hostURL string) (*janusdConnection, error) {
	if fc, ok := jwc.janusdConnections.Load(hostURL); ok == true {
		return fc.(*janusdConnection), nil
	}
	return nil, fmt.Errorf("could not connect to janusd at hostURL %v", hostURL)
}

// GetWatchStates is a helper function that gets all the current watch states
// from every JanusD pod running in the clutser. This is used in the
// syncHandler to run exactly once each sync.
func (jwc *JanusWatcherController) getWatchStates() ([][]*pb.JanusdHandle, error) {
	var watchStates [][]*pb.JanusdHandle

	// If specifying a janusdURL for a daemon that is located out-of-cluster,
	// we assume a single JanusD pod in the cluster; get the watch state of
	// this daemon pod only.
	if jwc.janusdURL != "" {
		fc, err := jwc.getJanusdConnection(jwc.janusdURL)
		if err != nil {
			return nil, err
		}
		ws, err := fc.GetWatchState()
		if err != nil {
			return nil, err
		}
		watchStates = append(watchStates, ws)
		return watchStates, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: janusdSelector})
	if err != nil {
		return nil, err
	}
	daemonPods, err := jwc.podLister.Pods(janusNamespace).List(selector)
	if err != nil {
		return nil, err
	}
	for _, pod := range daemonPods {
		hostURL, err := jwc.getHostURL(pod)
		if err != nil {
			continue
		}
		fc, err := jwc.getJanusdConnection(hostURL)
		if err != nil {
			return nil, err
		}
		ws, err := fc.GetWatchState()
		if err != nil {
			continue
		}
		watchStates = append(watchStates, ws)
	}
	return watchStates, nil
}

// isPodInWatchState is a helper function that given a pod and list of watch
// states, find if pod name appears anywhere in the list.
func (jwc *JanusWatcherController) isPodInWatchState(pod *corev1.Pod, watchStates [][]*pb.JanusdHandle) bool {
	var found bool
	for _, watchState := range watchStates {
		for _, ws := range watchState {
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
func (jwc *JanusWatcherController) removePodFromUpdateQueue(podName string) {
	updatePodQueue.Delete(podName)
}
