package januscontroller

import (
	cryptotls "crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"

	janusv1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
	janusv1alpha1client "clustergarage.io/janus-controller/pkg/client/clientset/versioned/typed/januscontroller/v1alpha1"
	pb "github.com/clustergarage/janus-proto/golang"
)

var (
	// TLS specifies whether to connect to the JanusD server using TLS.
	TLS bool
	// TLSSkipVerify specifies not to verify the certificate presented by the
	// server.
	TLSSkipVerify bool
	// TLSCACert is the file containing trusted certificates for verifying the
	// server.
	TLSCACert string
	// TLSClientCert is the file containing the client certificate for
	// authenticating with the server.
	TLSClientCert string
	// TLSClientKey is the file containing the client private key for
	// authenticating with the server.
	TLSClientKey string
	// TLSServerName overrides the hostname used to verify the server
	// certficate.
	TLSServerName string

	annotationMux = &sync.RWMutex{}
)

// janusdConnection defines a JanusD gRPC server URL and a gRPC client to
// connect to in order to make add and remove watcher calls, as well as getting
// the current state of the daemon to keep the controller<-->daemon in sync.
type janusdConnection struct {
	hostURL string
	handle  *pb.JanusdHandle
	client  pb.JanusdClient
}

// BuildAndStoreDialOptions creates a grpc.DialOption object to be used with
// grpc.Dial to connect to a daemon server. This function allows both secure
// and insecure variants.
func BuildAndStoreDialOptions(tls, tlsSkipVerify bool, caCert, clientCert, clientKey, serverName string) (grpc.DialOption, error) {
	// Store passed-in configuration to later create janusdConnections.
	TLS = tls
	TLSSkipVerify = tlsSkipVerify
	TLSCACert = caCert
	TLSClientCert = clientCert
	TLSClientKey = clientKey
	TLSServerName = serverName

	if TLS {
		var tlsconfig cryptotls.Config
		if TLSClientCert != "" && TLSClientKey != "" {
			keypair, err := cryptotls.LoadX509KeyPair(TLSClientCert, TLSClientKey)
			if err != nil {
				return nil, fmt.Errorf("Failed to load TLS client cert/key pair: %v", err)
			}
			tlsconfig.Certificates = []cryptotls.Certificate{keypair}
		}

		if TLSSkipVerify {
			tlsconfig.InsecureSkipVerify = true
		} else if TLSCACert != "" {
			pem, err := ioutil.ReadFile(TLSCACert)
			if err != nil {
				return nil, fmt.Errorf("Failed to load root CA certificates from file %s: %v", TLSCACert, err)
			}
			cacertpool := x509.NewCertPool()
			if !cacertpool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("No root CA certificate parsed from file %s", TLSCACert)
			}
			tlsconfig.RootCAs = cacertpool
		}
		if TLSServerName != "" {
			tlsconfig.ServerName = TLSServerName
		}
		return grpc.WithTransportCredentials(credentials.NewTLS(&tlsconfig)), nil
	}
	return grpc.WithInsecure(), nil
}

// NewJanusdConnection creates a new janusdConnection type given a required
// hostURL and an optional gRPC client; if the client is not specified, this is
// created for you here.
func NewJanusdConnection(hostURL string, opts grpc.DialOption, client ...pb.JanusdClient) (*janusdConnection, error) {
	fc := &janusdConnection{hostURL: hostURL}
	if len(client) > 0 {
		fc.client = client[0]
	} else {
		conn, err := grpc.Dial(fc.hostURL, opts)
		if err != nil {
			return nil, fmt.Errorf("Could not connect: %v", err)
		}
		fc.client = pb.NewJanusdClient(conn)
	}
	return fc, nil
}

// AddJanusdWatcher sends a message to the JanusD daemon to create a new
// watcher.
func (fc *janusdConnection) AddJanusdWatcher(config *pb.JanusdConfig) (*pb.JanusdHandle, error) {
	glog.Infof("Sending CreateWatch call to JanusD daemon, host: %s, request: %#v)", fc.hostURL, config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer ctx.Done()

	response, err := fc.client.CreateWatch(ctx, config)
	glog.Infof("Received CreateWatch response: %#v", response)
	if err != nil || response.NodeName == "" {
		return nil, errors.NewConflict(schema.GroupResource{Resource: "nodes"},
			config.NodeName, fmt.Errorf(fmt.Sprintf("janusd::CreateWatch failed: %v", err)))
	}
	return response, nil
}

// RemoveJanusdWatcher sends a message to the JanusD daemon to remove an
// existing watcher.
func (fc *janusdConnection) RemoveJanusdWatcher(config *pb.JanusdConfig) error {
	glog.Infof("Sending DestroyWatch call to JanusD daemon, host: %s, request: %#v", fc.hostURL, config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer ctx.Done()

	_, err := fc.client.DestroyWatch(ctx, config)
	if err != nil {
		return errors.NewConflict(schema.GroupResource{Resource: "nodes"},
			config.NodeName, fmt.Errorf(fmt.Sprintf("janusd::DestroyWatch failed: %v", err)))
	}
	return nil
}

// GetWatchState sends a message to the JanusD daemon to return the current
// state of the watchers being watched via inotify.
func (fc *janusdConnection) GetWatchState() ([]*pb.JanusdHandle, error) {
	ctx := context.Background()
	defer ctx.Done()

	var watchers []*pb.JanusdHandle
	stream, err := fc.client.GetWatchState(ctx, &pb.Empty{})
	if err != nil {
		return nil, errors.NewConflict(schema.GroupResource{Resource: "nodes"},
			fc.hostURL, fmt.Errorf(fmt.Sprintf("janusd::GetWatchState failed: %v", err)))
	}
	for {
		watch, err := stream.Recv()
		if err == io.EOF || err != nil {
			break
		}
		watchers = append(watchers, watch)
	}
	return watchers, nil
}

// updatePodFunc defines an function signature to be passed into
// updatePodWithRetries.
type updatePodFunc func(pod *corev1.Pod) error

// updateJanusWatcherStatus updates the status of the specified JanusWatcher
// object.
func updateJanusWatcherStatus(c janusv1alpha1client.JanusWatcherInterface, jw *janusv1alpha1.JanusWatcher,
	newStatus janusv1alpha1.JanusWatcherStatus) (*janusv1alpha1.JanusWatcher, error) {

	// This is the steady state. It happens when the JanusWatcher doesn't have
	// any expectations, since we do a periodic relist every 30s. If the
	// generations differ but the subjects are the same, a caller might have
	// resized to the same subject count.
	if jw.Status.ObservablePods == newStatus.ObservablePods &&
		jw.Status.WatchedPods == newStatus.WatchedPods &&
		jw.Generation == jw.Status.ObservedGeneration {
		return jw, nil
	}
	// Save the generation number we acted on, otherwise we might wrongfully
	// indicate that we've seen a spec update when we retry.
	// @TODO: This can clobber an update if we allow multiple agents to write
	// to the same status.
	newStatus.ObservedGeneration = jw.Generation

	var getErr, updateErr error
	var updatedJW *janusv1alpha1.JanusWatcher
	for i, jw := 0, jw; ; i++ {
		glog.V(4).Infof(fmt.Sprintf("Updating status for %v: %s/%s, ", jw.Kind, jw.Namespace, jw.Name) +
			fmt.Sprintf("observed generation %d->%d, ", jw.Status.ObservedGeneration, jw.Generation) +
			fmt.Sprintf("observable pods %d->%d, ", jw.Status.ObservablePods, newStatus.ObservablePods) +
			fmt.Sprintf("watched pods %d->%d", jw.Status.WatchedPods, newStatus.WatchedPods))

		// If the CustomResourceSubresources feature gate is not enabled, we
		// must use Update instead of UpdateStatus to update the Status block
		// of the JanusWatcher resource.
		// UpdateStatus will not allow changes to the Spec of the resource,
		// which is ideal for ensuring nothing other than resource status has
		// been updated.
		jw.Status = newStatus
		updatedJW, updateErr = c.UpdateStatus(jw)
		if updateErr == nil {
			return updatedJW, nil
		}
		// Stop retrying if we exceed statusUpdateRetries - the janus watcher
		// will be requeued with a rate limit.
		if i >= statusUpdateRetries {
			break
		}
		// Update the JanusWatcher with the latest resource version for the next
		// poll.
		if jw, getErr = c.Get(jw.Name, metav1.GetOptions{}); getErr != nil {
			// If the GET fails we can't trust status.ObservablePods anymore.
			// This error is bound to be more interesting than the update
			// failure.
			return nil, getErr
		}
	}

	return nil, updateErr
}

// calculateStatus creates a new status from a given JanusWatcher and
// filteredPods array.
func calculateStatus(jw *janusv1alpha1.JanusWatcher, filteredPods []*corev1.Pod, manageJanusWatchersErr error) janusv1alpha1.JanusWatcherStatus {
	newStatus := jw.Status
	newStatus.ObservablePods = int32(len(filteredPods))

	// Count the number of pods that have labels matching the labels of the pod
	// template of the janus watcher, the matching pods may have more labels
	// than are in the template. Because the label of podTemplateSpec is a
	// superset of the selector of the janus watcher, so the possible matching
	// pods must be part of the filteredPods.
	watchedPods := 0
	for _, pod := range filteredPods {
		if _, found := pod.GetAnnotations()[JanusWatcherAnnotationKey]; found {
			watchedPods++
		}
	}
	newStatus.WatchedPods = int32(watchedPods)
	return newStatus
}

// updateAnnotations takes an array of annotations to remove or add and an
// object to apply this update to; this will modify the object's Annotations
// map by way of an Accessor.
func updateAnnotations(removeAnnotations []string, newAnnotations map[string]string, obj runtime.Object) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	annotations := accessor.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	for key, value := range newAnnotations {
		annotations[key] = value
	}
	for _, annotation := range removeAnnotations {
		delete(annotations, annotation)
	}
	annotationMux.Lock()
	accessor.SetAnnotations(annotations)
	annotationMux.Unlock()

	return nil
}

// updatePodWithRetries updates a pod with given applyUpdate function.
func updatePodWithRetries(podClient coreclient.PodInterface, podLister corelisters.PodLister,
	namespace, name string, applyUpdate updatePodFunc) (*corev1.Pod, error) {

	var pod *corev1.Pod
	retryErr := retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		pod, err = podLister.Pods(namespace).Get(name)
		if err != nil {
			return err
		}
		pod = pod.DeepCopy()
		// Apply the update, then attempt to push it to the apiserver.
		if applyErr := applyUpdate(pod); applyErr != nil {
			return applyErr
		}
		pod, err = podClient.Update(pod)
		return err
	})

	// Ignore the precondition violated error, this pod is already updated with
	// the desired label.
	if retryErr == errorsutil.ErrPreconditionViolated {
		glog.Infof("Pod %s/%s precondition doesn't hold, skip updating it.", namespace, name)
		retryErr = nil
	}

	return pod, retryErr
}

// getPodContainerIDs returns a list of container IDs given a pod.
func getPodContainerIDs(pod *corev1.Pod) []string {
	var cids []string
	if len(pod.Status.ContainerStatuses) == 0 {
		return cids
	}
	for _, ctr := range pod.Status.ContainerStatuses {
		cids = append(cids, ctr.ContainerID)
	}
	return cids
}
