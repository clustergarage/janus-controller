package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	clientset "clustergarage.io/janus-controller/pkg/client/clientset/versioned"
	informers "clustergarage.io/janus-controller/pkg/client/informers/externalversions"
	januscontroller "clustergarage.io/janus-controller/pkg/controller"
	"clustergarage.io/janus-controller/pkg/signals"
)

const (
	// StatusInvalidArguments indicates specified invalid arguments.
	StatusInvalidArguments = 1
)

var (
	masterURL     string
	kubeconfig    string
	janusdURL     string
	tls           bool
	tlsSkipVerify bool
	tlsCACert     string
	tlsClientCert string
	tlsClientKey  string
	tlsServerName string
	healthPort    uint
)

func getKubernetesClient() (kubernetes.Interface, clientset.Interface) {
	config, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}
	kubeclientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}
	janusclientset, err := clientset.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error building janus clientset: %s", err.Error())
	}
	return kubeclientset, janusclientset
}

func serveHealthCheck() {
	server := grpc.NewServer()
	grpc_health_v1.RegisterHealthServer(server, health.NewServer())
	address, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", healthPort))
	if err != nil {
		log.Fatalf("Error dialing health check server port: %v", err)
	}
	if err := server.Serve(address); err != nil {
		log.Fatalf("Error serving health check server: %v", err)
	}
}

func init() {
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&janusdURL, "janusd", "", "The address of the JanusD server. Only required if daemon is running out-of-cluster.")
	flag.BoolVar(&tls, "tls", false, "Connect to the JanusD server using TLS. (default: false)")
	flag.BoolVar(&tlsSkipVerify, "tls-skip-verify", false, "Do not verify the certificate presented by the server. (default: false)")
	flag.StringVar(&tlsCACert, "tls-ca-cert", "", "The file containing trusted certificates for verifying the server. (with -tls, optional)")
	flag.StringVar(&tlsClientCert, "tls-client-cert", "", "The file containing the client certificate for authenticating with the server. (with -tls, optional)")
	flag.StringVar(&tlsClientKey, "tls-client-key", "", "The file containing the client private key for authenticating with the server. (with -tls)")
	flag.StringVar(&tlsServerName, "tls-server-name", "", "Override the hostname used to verify the server certificate. (with -tls)")
	flag.UintVar(&healthPort, "health", 5000, "The port to use for setting up the health check that will be used to monitor the controller.")

	argError := func(s string, v ...interface{}) {
		log.Printf("error: "+s, v...)
		os.Exit(StatusInvalidArguments)
	}

	if !tls {
		if tlsSkipVerify {
			argError("Specified -tls-skip-verify without specifying -tls.")
		}
		if tlsCACert != "" {
			argError("Specified -tls-ca-cert without specifying -tls.")
		}
		if tlsClientCert != "" {
			argError("Specified -tls-client-cert without specifying -tls.")
		}
		if tlsServerName != "" {
			argError("Specified -tls-server-name without specifying -tls.")
		}
	}
	if tlsClientCert != "" && tlsClientKey == "" {
		argError("Specified -tls-client-cert without specifying -tls-client-key.")
	}
	if tlsClientCert == "" && tlsClientKey != "" {
		argError("Specified -tls-client-key without specifying -tls-client-cert.")
	}
	if tlsSkipVerify {
		if tlsCACert != "" {
			argError("Cannot specify -tls-ca-cert with -tls-skip-verify (CA cert would not be used).")
		}
		if tlsServerName != "" {
			argError("Cannot specify -tls-server-name with -tls-skip-verify (server name would not be used).")
		}
	}
}

func main() {
	// Override --stderrthreshold default value.
	if stderrThreshold := flag.Lookup("stderrthreshold"); stderrThreshold != nil {
		stderrThreshold.DefValue = "INFO"
		stderrThreshold.Value.Set("INFO")
	}
	flag.Parse()
	defer glog.Flush()

	// Set up signals so we handle the first shutdown signal gracefully.
	stopCh := signals.SetupSignalHandler()

	kubeclientset, janusclientset := getKubernetesClient()
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeclientset, time.Second*30)
	janusInformerFactory := informers.NewSharedInformerFactory(janusclientset, time.Second*30)

	opts, err := januscontroller.BuildAndStoreDialOptions(tls, tlsSkipVerify, tlsCACert, tlsClientCert, tlsClientKey, tlsServerName)
	if err != nil {
		log.Fatalf("Error creating dial options: %s", err.Error())
	}
	janusdConnection, err := januscontroller.NewJanusdConnection(janusdURL, opts)
	if err != nil {
		log.Fatalf("Error creating connection to JanusD server: %s", err.Error())
	}

	controller := januscontroller.NewJanusGuardController(kubeclientset, janusclientset,
		janusInformerFactory.Januscontroller().V1alpha1().JanusGuards(),
		kubeInformerFactory.Core().V1().Pods(),
		kubeInformerFactory.Core().V1().Endpoints(),
		janusdConnection)

	go kubeInformerFactory.Start(stopCh)
	go janusInformerFactory.Start(stopCh)
	go serveHealthCheck()

	if err := controller.Run(2, stopCh); err != nil {
		log.Fatalf("Error running controller: %s", err.Error())
	}
}
