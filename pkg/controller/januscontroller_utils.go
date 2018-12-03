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
// connect to in order to make add and remove guard calls, as well as getting
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

// AddJanusdGuard sends a message to the JanusD daemon to create a new guard.
func (fc *janusdConnection) AddJanusdGuard(config *pb.JanusdConfig) (*pb.JanusdHandle, error) {
	glog.Infof("Sending CreateGuard call to JanusD daemon, host: %s, request: %#v)", fc.hostURL, config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer ctx.Done()

	response, err := fc.client.CreateGuard(ctx, config)
	glog.Infof("Received CreateGuard response: %#v", response)
	if err != nil || response.NodeName == "" {
		return nil, errors.NewConflict(schema.GroupResource{Resource: "nodes"},
			config.NodeName, fmt.Errorf(fmt.Sprintf("janusd::CreateGuard failed: %v", err)))
	}
	return response, nil
}

// RemoveJanusdGuard sends a message to the JanusD daemon to remove an existing
// guard.
func (fc *janusdConnection) RemoveJanusdGuard(config *pb.JanusdConfig) error {
	glog.Infof("Sending DestroyGuard call to JanusD daemon, host: %s, request: %#v", fc.hostURL, config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer ctx.Done()

	_, err := fc.client.DestroyGuard(ctx, config)
	if err != nil {
		return errors.NewConflict(schema.GroupResource{Resource: "nodes"},
			config.NodeName, fmt.Errorf(fmt.Sprintf("janusd::DestroyGuard failed: %v", err)))
	}
	return nil
}

// GetGuardState sends a message to the JanusD daemon to return the current
// state of the guards being marked via fanotify.
func (fc *janusdConnection) GetGuardState() ([]*pb.JanusdHandle, error) {
	ctx := context.Background()
	defer ctx.Done()

	var guards []*pb.JanusdHandle
	stream, err := fc.client.GetGuardState(ctx, &pb.Empty{})
	if err != nil {
		return nil, errors.NewConflict(schema.GroupResource{Resource: "nodes"},
			fc.hostURL, fmt.Errorf(fmt.Sprintf("janusd::GetGuardState failed: %v", err)))
	}
	for {
		guard, err := stream.Recv()
		if err == io.EOF || err != nil {
			break
		}
		guards = append(guards, guard)
	}
	return guards, nil
}

// updatePodFunc defines an function signature to be passed into
// updatePodWithRetries.
type updatePodFunc func(pod *corev1.Pod) error

// updateJanusGuardStatus updates the status of the specified JanusGuard
// object.
func updateJanusGuardStatus(c janusv1alpha1client.JanusGuardInterface, jg *janusv1alpha1.JanusGuard,
	newStatus janusv1alpha1.JanusGuardStatus) (*janusv1alpha1.JanusGuard, error) {

	// This is the steady state. It happens when the JanusGuard doesn't have
	// any expectations, since we do a periodic relist every 30s. If the
	// generations differ but the subjects are the same, a caller might have
	// resized to the same subject count.
	if jg.Status.ObservablePods == newStatus.ObservablePods &&
		jg.Status.GuardedPods == newStatus.GuardedPods &&
		jg.Generation == jg.Status.ObservedGeneration {
		return jg, nil
	}
	// Save the generation number we acted on, otherwise we might wrongfully
	// indicate that we've seen a spec update when we retry.
	// @TODO: This can clobber an update if we allow multiple agents to write
	// to the same status.
	newStatus.ObservedGeneration = jg.Generation

	var getErr, updateErr error
	var updatedJG *janusv1alpha1.JanusGuard
	for i, jg := 0, jg; ; i++ {
		glog.V(4).Infof(fmt.Sprintf("Updating status for %v: %s/%s, ", jg.Kind, jg.Namespace, jg.Name) +
			fmt.Sprintf("observed generation %d->%d, ", jg.Status.ObservedGeneration, jg.Generation) +
			fmt.Sprintf("observable pods %d->%d, ", jg.Status.ObservablePods, newStatus.ObservablePods) +
			fmt.Sprintf("guarded pods %d->%d", jg.Status.GuardedPods, newStatus.GuardedPods))

		// If the CustomResourceSubresources feature gate is not enabled, we
		// must use Update instead of UpdateStatus to update the Status block
		// of the JanusGuard resource.
		// UpdateStatus will not allow changes to the Spec of the resource,
		// which is ideal for ensuring nothing other than resource status has
		// been updated.
		jg.Status = newStatus
		updatedJG, updateErr = c.UpdateStatus(jg)
		if updateErr == nil {
			return updatedJG, nil
		}
		// Stop retrying if we exceed statusUpdateRetries - the janus guard
		// will be requeued with a rate limit.
		if i >= statusUpdateRetries {
			break
		}
		// Update the JanusGuard with the latest resource version for the next
		// poll.
		if jg, getErr = c.Get(jg.Name, metav1.GetOptions{}); getErr != nil {
			// If the GET fails we can't trust status.ObservablePods anymore.
			// This error is bound to be more interesting than the update
			// failure.
			return nil, getErr
		}
	}

	return nil, updateErr
}

// calculateStatus creates a new status from a given JanusGuard and
// filteredPods array.
func calculateStatus(jg *janusv1alpha1.JanusGuard, filteredPods []*corev1.Pod, manageJanusGuardErr error) janusv1alpha1.JanusGuardStatus {
	newStatus := jg.Status
	newStatus.ObservablePods = int32(len(filteredPods))

	// Count the number of pods that have labels matching the labels of the pod
	// template of the janus guard, the matching pods may have more labels than
	// are in the template. Because the label of podTemplateSpec is a superset
	// of the selector of the janus guard, so the possible matching pods must
	// be part of the filteredPods.
	guardedPods := 0
	for _, pod := range filteredPods {
		if _, found := pod.GetAnnotations()[JanusGuardAnnotationKey]; found {
			guardedPods++
		}
	}
	newStatus.GuardedPods = int32(guardedPods)
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
