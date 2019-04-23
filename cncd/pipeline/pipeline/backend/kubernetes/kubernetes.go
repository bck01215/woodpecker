package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"time"

	"github.com/laszlocph/drone-oss-08/cncd/pipeline/pipeline/backend"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"

	// To authenticate to GCP K8s clusters
	"k8s.io/client-go/informers"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
)

type engine struct {
	logs         *bytes.Buffer
	kubeClient   kubernetes.Interface
	namespace    string
	storageClass string
	volumeSize   string
}

// New returns a new Kubernetes Engine.
func New(namespace1 string, storageClass1 string, size string) (backend.Engine, error) {
	var kubeClient kubernetes.Interface
	_, err := rest.InClusterConfig()
	if err != nil {
		kubeClient, err = getClientOutOfCluster()
	} else {
		kubeClient, err = getClient()
	}

	if err != nil {
		return nil, err
	}

	return &engine{
		logs:         new(bytes.Buffer),
		kubeClient:   kubeClient,
		namespace:    namespace1,
		storageClass: storageClass1,
		volumeSize:   size,
	}, nil
}

// Setup the pipeline environment.
func (e *engine) Setup(ctx context.Context, conf *backend.Config) error {
	e.logs.WriteString("Setting up Kubernetes primitives\n")

	for _, vol := range conf.Volumes {
		pvc := PersistentVolumeClaim(e.namespace, vol.Name, e.storageClass, e.volumeSize)
		_, err := e.kubeClient.CoreV1().PersistentVolumeClaims(e.namespace).Create(pvc)
		if err != nil {
			return err
		}
	}

	return nil
}

// Start the pipeline step.
func (e *engine) Exec(ctx context.Context, step *backend.Step) error {
	e.logs.WriteString("Creating pod\n")
	pod, err := Pod(e.namespace, step)
	if err != nil {
		return err
	}

	for _, n := range step.Networks {
		if len(n.Aliases) > 0 {
			svc := Service(e.namespace, n.Aliases[0], pod.Name, step.Ports)
			if svc == nil {
				continue
			}
			if _, err := e.kubeClient.CoreV1().Services(e.namespace).Create(svc); err != nil {
				return err
			}
		}
	}

	_, err = e.kubeClient.CoreV1().Pods(e.namespace).Create(pod)
	return err
}

// DEPRECATED
// Kill the pipeline step.
func (e *engine) Kill(context.Context, *backend.Step) error {
	return nil
}

// Wait for the pipeline step to complete and returns
// the completion results.
func (e *engine) Wait(ctx context.Context, step *backend.Step) (*backend.State, error) {
	podName := podName(step)

	finished := make(chan bool)

	var podUpdated = func(old interface{}, new interface{}) {
		pod := new.(*v1.Pod)
		if pod.Name == podName {
			if isImagePullBackOffState(pod) {
				finished <- true
			}

			switch pod.Status.Phase {
			case v1.PodSucceeded, v1.PodFailed, v1.PodUnknown:
				finished <- true
			}
		}
	}

	// TODO 5 seconds is against best practice, k3s didn't work otherwise
	si := informers.NewSharedInformerFactory(e.kubeClient, 5*time.Second)
	si.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: podUpdated,
		},
	)
	si.Start(wait.NeverStop)

	// TODO Cancel on ctx.Done
	<-finished

	pod, err := e.kubeClient.CoreV1().Pods(e.namespace).Get(podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if isImagePullBackOffState(pod) {
		return nil, fmt.Errorf("Could not pull image for pod %s", pod.Name)
	}

	bs := &backend.State{
		ExitCode:  int(pod.Status.ContainerStatuses[0].State.Terminated.ExitCode),
		Exited:    true,
		OOMKilled: false,
	}

	return bs, nil
}

// Tail the pipeline step logs.
func (e *engine) Tail(ctx context.Context, step *backend.Step) (io.ReadCloser, error) {
	podName := podName(step)

	up := make(chan bool)

	var podUpdated = func(old interface{}, new interface{}) {
		pod := new.(*v1.Pod)
		if pod.Name == podName {
			switch pod.Status.Phase {
			case v1.PodRunning, v1.PodSucceeded, v1.PodFailed:
				up <- true
			}
		}
	}

	// TODO 5 seconds is against best practice, k3s didn't work otherwise
	si := informers.NewSharedInformerFactory(e.kubeClient, 5*time.Second)
	si.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: podUpdated,
		},
	)
	si.Start(wait.NeverStop)

	<-up

	opts := &v1.PodLogOptions{
		Follow: true,
	}

	logs, err := e.kubeClient.CoreV1().RESTClient().Get().
		Namespace(e.namespace).
		Name(podName).
		Resource("pods").
		SubResource("log").
		VersionedParams(opts, scheme.ParameterCodec).
		Stream()
	if err != nil {
		return nil, err
	}
	rc, wc := io.Pipe()

	go func() {
		systemLogs := ioutil.NopCloser(bytes.NewReader(e.logs.Bytes()))
		io.Copy(wc, systemLogs)
		io.Copy(wc, logs)
		e.logs.Reset()
		logs.Close()
		wc.Close()
		rc.Close()
	}()
	return rc, nil

	// rc := ioutil.NopCloser(bytes.NewReader(e.logs.Bytes()))
	// e.logs.Reset()
	// return rc, nil
}

// Destroy the pipeline environment.
func (e *engine) Destroy(ctx context.Context, conf *backend.Config) error {
	var gracePeriodSeconds int64 = 0 // immediately
	dpb := metav1.DeletePropagationBackground

	deleteOpts := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
		PropagationPolicy:  &dpb,
	}

	for _, stage := range conf.Stages {
		for _, step := range stage.Steps {
			if err := e.kubeClient.CoreV1().Pods(e.namespace).Delete(podName(step), deleteOpts); err != nil {
				return err
			}

			for _, n := range step.Networks {
				svc := Service(e.namespace, n.Aliases[0], step.Alias, step.Ports)
				if svc == nil {
					continue
				}
				if err := e.kubeClient.CoreV1().Services(e.namespace).Delete(svc.Name, deleteOpts); err != nil {
					return err
				}
			}
		}
	}

	for _, vol := range conf.Volumes {
		pvc := PersistentVolumeClaim(e.namespace, vol.Name, e.storageClass, e.volumeSize)
		err := e.kubeClient.CoreV1().PersistentVolumeClaims(e.namespace).Delete(pvc.Name, deleteOpts)
		if err != nil {
			return err
		}
	}

	return nil
}

func dnsName(i string) string {
	return strings.Replace(i, "_", "-", -1)
}

func isImagePullBackOffState(pod *v1.Pod) bool {
	for _, containerState := range pod.Status.ContainerStatuses {
		if containerState.State.Waiting != nil {
			if containerState.State.Waiting.Reason == "ImagePullBackOff" {
				return true
			}
		}
	}

	return false
}