package operatortests

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	k8sv1 "k8s.io/api/core/v1"
	k8scrdclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	k8sclient "k8s.io/client-go/kubernetes/typed/core/v1"
	k8srestapi "k8s.io/client-go/rest"
	k8sclientcmd "k8s.io/client-go/tools/clientcmd"

	"github.com/nats-io/nats-operator/pkg/apis/nats/v1alpha2"
	"github.com/nats-io/nats-operator/pkg/client"
	natsclient "github.com/nats-io/nats-operator/pkg/client/clientset/versioned"
	natsalphav2client "github.com/nats-io/nats-operator/pkg/client/clientset/versioned/typed/nats/v1alpha2"
	"github.com/nats-io/nats-operator/pkg/controller"
	contextutil "github.com/nats-io/nats-operator/pkg/util/context"
	kubernetesutil "github.com/nats-io/nats-operator/pkg/util/kubernetes"
	testutil "github.com/nats-io/nats-operator/test/util"
)

func TestRegisterCRD(t *testing.T) {
	c, err := newController()
	if err != nil {
		t.Fatal(err)
	}

	// Create a context with a timeout of 10 seconds.
	// This should provide enough time for the CRDs to be registered, and is small enough for the test not to take long.
	ctx := contextutil.WithTimeout(10 * time.Second)

	// Run the controller for NatsCluster resources in the foreground.
	// It will stop when the context times out, and the test will proceed to verify that the CRDs have been are created.
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}

	cl, err := newKubeClients()
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the CRDs to become ready.
	// This should have already happened within the 10 seconds that the controller was running, but it's OK to explicitly wait.
	if err := kubernetesutil.WaitCRDs(cl.kcrdc); err != nil {
		t.Fatal(err)
	}

	// Confirm that the resource has been created.
	result, err := cl.kcrdc.ApiextensionsV1beta1().
		CustomResourceDefinitions().
		Get("natsclusters.nats.io", k8smetav1.GetOptions{})
	if err != nil {
		t.Errorf("Failed registering cluster: %s", err)
	}

	got := result.Spec.Names
	expected := "natsclusters"
	if got.Plural != expected {
		t.Errorf("got: %s, expected: %s", got.Plural, expected)
	}
	expected = "natscluster"
	if got.Singular != expected {
		t.Errorf("got: %s, expected: %s", got.Plural, expected)
	}
	if len(got.ShortNames) < 1 {
		t.Errorf("expected shortnames for the CRD: %+v", got.ShortNames)
	}
	expected = "nats"
	if got.ShortNames[0] != expected {
		t.Errorf("got: %s, expected: %s", got.ShortNames[0], expected)
	}
}

func TestCreateConfigSecret(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runController(ctx, t)

	cl, err := newKubeClients()
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the CRDs to become ready.
	if err := kubernetesutil.WaitCRDs(cl.kcrdc); err != nil {
		t.Fatal(err)
	}

	name := "test-nats-cluster-1"
	namespace := "default"
	var size = 3
	cluster := &v1alpha2.NatsCluster{
		TypeMeta: k8smetav1.TypeMeta{
			Kind:       v1alpha2.CRDResourceKind,
			APIVersion: v1alpha2.SchemeGroupVersion.String(),
		},
		ObjectMeta: k8smetav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha2.ClusterSpec{
			Size:    size,
			Version: "1.1.0",
		},
	}
	_, err = cl.ncli.Create(ctx, cluster)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the cluster to reach the desired size.
	err = testutil.WaitForNatsClusterCondition(cl.ocli, cluster, func(event watch.Event) (bool, error) {
		newCluster := event.Object.(*v1alpha2.NatsCluster)
		return newCluster.Status.Size == size, nil
	})
	if err != nil {
		t.Errorf("failed to wait for cluster size: %v", err)
	}

	// List all pods belonging to the NatsCluster resource.
	pods, err := cl.kc.Pods(cluster.Namespace).List(kubernetesutil.ClusterListOpt(cluster.Name))
	if err != nil {
		t.Errorf("failed to list pods for cluster: %v", err)
	}

	cm, err := cl.kc.Secrets(namespace).Get(name, k8smetav1.GetOptions{})
	if err != nil {
		t.Errorf("Config map error: %v", err)
	}
	conf, ok := cm.Data["nats.conf"]
	if !ok {
		t.Error("Config map was missing")
	}
	for _, pod := range pods.Items {
		if !strings.Contains(string(conf), pod.Name) {
			t.Errorf("Could not find pod %q in config", pod.Name)
		}
	}
}

func TestReplacePresentConfigSecret(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runController(ctx, t)

	cl, err := newKubeClients()
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the CRDs to become ready.
	if err := kubernetesutil.WaitCRDs(cl.kcrdc); err != nil {
		t.Fatal(err)
	}

	name := "test-nats-cluster-2"
	namespace := "default"

	// Create a secret with the same name, that will
	// be replaced with a new one by the operator.
	cm := &k8sv1.Secret{
		ObjectMeta: k8smetav1.ObjectMeta{
			Name: name,
		},
		Data: map[string][]byte{
			"nats.conf": []byte("port: 4222"),
		},
	}
	_, err = cl.kc.Secrets(namespace).Create(cm)
	if err != nil {
		t.Fatal(err)
	}

	var size = 3
	cluster := &v1alpha2.NatsCluster{
		TypeMeta: k8smetav1.TypeMeta{
			Kind:       v1alpha2.CRDResourceKind,
			APIVersion: v1alpha2.SchemeGroupVersion.String(),
		},
		ObjectMeta: k8smetav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha2.ClusterSpec{
			Size:    size,
			Version: "1.1.0",
		},
	}
	_, err = cl.ncli.Create(ctx, cluster)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the cluster to reach the desired size.
	err = testutil.WaitForNatsClusterCondition(cl.ocli, cluster, func(event watch.Event) (bool, error) {
		newCluster := event.Object.(*v1alpha2.NatsCluster)
		return newCluster.Status.Size == size, nil
	})
	if err != nil {
		t.Errorf("failed to wait for cluster size: %v", err)
	}

	// List all pods belonging to the NatsCluster resource.
	pods, err := cl.kc.Pods(cluster.Namespace).List(kubernetesutil.ClusterListOpt(cluster.Name))
	if err != nil {
		t.Errorf("failed to list pods for cluster: %v", err)
	}

	cm, err = cl.kc.Secrets(namespace).Get(name, k8smetav1.GetOptions{})
	if err != nil {
		t.Errorf("Config map error: %v", err)
	}
	conf, ok := cm.Data["nats.conf"]
	if !ok {
		t.Error("Config map was missing")
	}
	for _, pod := range pods.Items {
		if !strings.Contains(string(conf), pod.Name) {
			t.Errorf("Could not find pod %q in config", pod.Name)
		}
	}
}

type clients struct {
	kc      k8sclient.CoreV1Interface
	kcrdc   k8scrdclient.Interface
	restcli *k8srestapi.RESTClient
	config  *k8srestapi.Config
	ncli    client.NatsClusterCR
	ocli    *natsalphav2client.NatsV1alpha2Client

	// kubeClient is an interface for a client the the Kubernetes base APIs.
	kubeClient kubernetes.Interface
	// natsClient is an interface for a client for our API.
	natsClient natsclient.Interface
}

func newKubeClients() (*clients, error) {
	var err error
	var cfg *k8srestapi.Config
	if kubeconfig := os.Getenv("KUBERNETES_CONFIG_FILE"); kubeconfig != "" {
		cfg, err = k8sclientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	// Create a client for the Kubernetes base APIs.
	kubeClient := kubernetesutil.MustNewKubeClientFromConfig(cfg)
	// Create a client for our API.
	natsClient := kubernetesutil.MustNewNatsClientFromConfig(cfg)

	kcrdc, err := k8scrdclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	restcli, _, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	ncli, err := client.NewCRClient(cfg)
	if err != nil {
		return nil, err
	}

	cl := &clients{
		// Use the already existing client for the Kubernetes base APIs.
		// TODO Remove and use kubeClient alone everywhere in the test suite.
		kc:      kubeClient.CoreV1(),
		kcrdc:   kcrdc,
		restcli: restcli,
		ncli:    ncli,
		// Use the already existing client for our API.
		// TODO Remove and use natsClient alone everywhere in the test suite.
		ocli:   natsClient.NatsV1alpha2().(*natsalphav2client.NatsV1alpha2Client),
		config: cfg,

		kubeClient: kubeClient,
		natsClient: natsClient,
	}
	return cl, nil
}

func newController() (*controller.Controller, error) {
	cl, err := newKubeClients()
	if err != nil {
		return nil, err
	}

	// NOTE: Eventually use a namespace at random under a
	// delete propagation policy for deleting the namespace.
	config := controller.Config{
		Namespace:   "default",
		KubeCli:     cl.kubeClient,
		KubeExtCli:  cl.kcrdc,
		OperatorCli: cl.natsClient,
	}

	// Initialize the controller for NatsCluster resources.
	c := controller.NewNatsClusterController(config)

	return c, nil
}

func runController(ctx context.Context, t *testing.T) {
	// Run the operator controller in the background.
	c, err := newController()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		err := c.Run(ctx)
		if err != nil && err != context.Canceled {
			t.Fatal(err)
		}
	}()
}
