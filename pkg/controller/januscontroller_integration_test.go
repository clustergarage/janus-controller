// +build integration

package januscontroller

/*
import (
	"net/http/httptest"
	"testing"
	"time"

	janusv1alpha1 "clustergarage.io/janus-controller/pkg/apis/januscontroller/v1alpha1"
	janusclientset "clustergarage.io/janus-controller/pkg/client/clientset/versioned"
	informers "clustergarage.io/janus-controller/pkg/client/informers/externalversions"
	kubeinformers "k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/integration/framework"
)

func awcSetup(t *testing.T) (*httptest.Server, framework.CloseFunc, *janusv1alpha1.JanusGuardController,
	kubeinformers.SharedInformerFactory, informers.SharedInformerFactory, clientset.Interface) {

	masterConfig := framework.NewIntegrationTestMasterConfig()
	_, s, closeFn := framework.RunAMaster(masterConfig)

	config := restclient.Config{Host: s.URL}
	clientSet, err := clientset.NewForConfig(&config)
	if err != nil {
		t.Fatalf("Error in create clientset: %v", err)
	}
	resyncPeriod := 12 * time.Hour
	kubeinformers = kubeinformers.NewSharedInformerFactory(clientset.NewForConfigOrDie(restclient.AddUserAgent(&config, "kube-informers")), resyncPeriod)
	janusinformers := informers.NewSharedInformerFactory(janusclientset.NewForConfigOrDie(restclient.AddUserAgent(&config, "janus-informers")), resyncPeriod)

	awc := NewJanusGuardController(
		clientset.NewForConfigOrDie(restclient.AddUserAgent(&config, "kube-controller")),
		janusclientset.NewForConfigOrDie(restclient.AddUserAgent(&config, "janus-controller")),
		janusinformers.januscontroller().V1alpha1().JanusGuards(),
		kubeinformers.Core().V1().Pods(),
		kubeinformers.Core().V1().Endpoints(),
		nil)

	if err != nil {
		t.Fatalf("Failed to create januswatcher controller")
	}
	return s, closeFn, awc, kubeinformers, janusinformers, clientSet
}
*/
