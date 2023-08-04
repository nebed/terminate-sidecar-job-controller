/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"time"
	"bytes"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	podinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	podlisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/client-go/tools/remotecommand"
	set "github.com/deckarep/golang-set"
)

const controllerAgentName = "terminate-sidecar-job-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Foo is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Foo fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Foo"
	// MessageResourceSynced is the message used for an Event fired when a Foo
	// is synced successfully
	MessageResourceSynced = "Pod synced successfully"
)

// Controller is the controller implementation to manage pods
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	podsLister podlisters.PodLister
	podsSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new controller
func NewController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	podInformer podinformers.PodInformer) *Controller {
	logger := klog.FromContext(ctx)

	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:     kubeclientset,
		podsLister: podInformer.Lister(),
		podsSynced: podInformer.Informer().HasSynced,
		workqueue:         workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		recorder:          recorder,
	}

	logger.Info("Setting up event handlers")
	//Setup event handlers for when pods are created, changed or deleted
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*corev1.Pod)
			oldPod := old.(*corev1.Pod)
			if newPod.ResourceVersion == oldPod.ResourceVersion {
				// Periodic resync will send update events for all known Deployments.
				// Two different versions of the same Deployment will always have different RVs.
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleDeleteObject,
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)

	// Start the informer factories to begin populating the informer caches
	logger.Info("Starting Terminate Sidecar controller")

	// Wait for the caches to be synced before starting workers
	logger.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.podsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count", workers)
	// Launch two workers to process Foo resources
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logger.Info("Started workers")
	<-ctx.Done()
	logger.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		if err := c.syncHandler(ctx, key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		logger.Info("Successfully synced", "resourceName", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two.
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "resourceName", key)

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the pod with the name
	pod, err := c.podsLister.Pods(namespace).Get(name)

	if err != nil {
		return err
	}

	sidecars := set.NewSet()
	sidecars.Add("istio-proxy")
	allContainers := set.NewSet()
	runningContainers := set.NewSet()
	completedContainers := set.NewSet()

	for _, containerStatus := range pod.Status.ContainerStatuses {
		allContainers.Add(containerStatus.Name)

		if containerStatus.Ready {
			runningContainers.Add(containerStatus.Name)
		} else {
			terminated := containerStatus.State.Terminated
			if terminated != nil && (terminated.Reason == "Completed" || terminated.Reason == "Error") {
				completedContainers.Add(containerStatus.Name)
			}
		}
	}

	logger.Info("all", allContainers)
	logger.Info("running", runningContainers)
	logger.Info("completed", completedContainers)
	logger.Info("sidecars", sidecars)

	// If we have accounted for all of the containers, and the sidecar containers are the only
	// ones still running, issue them each a shutdown command
	if runningContainers.Union(completedContainers).Equal(allContainers) {
		logger.Info("  We have all the containers")
		if runningContainers.Equal(sidecars) {
			logger.Info("    Sending shutdown signal to containers: ", pod.Name, sidecars)
			c.sendShutdownSignal(ctx, pod, sidecars)
		}
	}

	c.recorder.Event(pod, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

// enqueuePod takes a Pod resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Pod.
func (c *Controller) enqueuePod(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the Pod resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that Pod resource to be processed. If the pod is not Owned
// ny a Job it will be skipped
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	logger := klog.FromContext(context.Background())
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		logger.V(4).Info("Recovered deleted object", "resourceName", object.GetName())
	}
	logger.V(4).Info("Processing object", "object", klog.KObj(object))
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a Job, we should not do anything more
		// with it.
		if ownerRef.Kind != "Job" {
			return
		}

		pod, err := c.podsLister.Pods(object.GetNamespace()).Get(object.GetName())

		if err != nil {
			logger.V(4).Info("Ignore orphaned object", "object", klog.KObj(object), "pod", ownerRef.Name)
			return
		}

		if pod.Status.Phase != "Running" {
			logger.V(4).Info("Pod is not running", "pod", pod.Name)
			return
		}

		c.enqueuePod(pod)
		return
	}
}

func (c *Controller) handleDeleteObject(obj interface{}) {
	return
}

// Send a shutdown signal to sidecar containers in the Pod
func (c *Controller) sendShutdownSignal(ctx context.Context, pod *corev1.Pod, containers set.Set) {

	// Multiple arguments must be provided as separate "command" parameters
	// The first one is added automatically.
	// Todo: Update requestFromConfig to handle this better
	logger := klog.FromContext(ctx)
	config, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
        return
	}
	req := c.kubeclientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		logger.Info("There was an error adding to scheme", err)
		return 
	}
	command := "kill -s TERM 1"
	// creates the connection

	for _, c := range containers.ToSlice() {
		// Create a request out of config and the query parameters
		parameterCodec := runtime.NewParameterCodec(scheme)
		req.VersionedParams(&corev1.PodExecOptions{
			Command:   []string{"sh", "-c", command},
			Container: c.(string),
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, parameterCodec)

        logger.Info("Initiating exec into pod to kill main process")
		exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			logger.Info("There was an error executing", err)
		}

		var stdout, stderr bytes.Buffer
		err = exec.Stream(remotecommand.StreamOptions{
			Stdin:  nil,
			Stdout: &stdout,
			Stderr: &stderr,
			Tty:    false,
		})

		if err != nil {
			logger.Info("There was an error executing the stream", err)
		}
	}
}
