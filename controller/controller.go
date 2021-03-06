/*
Copyright 2016 Skippbox, Ltd.

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

/*
Modifications made
 1. Removed code to handle many types of k8s object such as deployments,
		 pods etc.
 2. Remove namespace from Event.
 3. Modified #processItem to cast all interfaces to secrets and releases
    and to handle various types of Helm events.
*/

package controller

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RoadieHQ/kubewise/handlers"
	"github.com/RoadieHQ/kubewise/kwrelease"
	"github.com/RoadieHQ/kubewise/utils"

	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const maxRetries = 5

var serverStartTime time.Time

// Event is a temporary, serializable reporesentation of a change in a secret or the creation
// or deletion of a secret. It can be placed on a queue and processed at a later point in
// the application where the secret is retrieved by a key.
type Event struct {
	key        string
	eventType  string
	secretType api_v1.SecretType
}

// Controller accepts notifications from the Kubernetes APIs and makes decisions based on the
// events that occur.
type Controller struct {
	clientset    kubernetes.Interface
	queue        workqueue.RateLimitingInterface
	informer     cache.SharedIndexInformer
	eventHandler handlers.Handler
}

// Start watches the Kubernetes secrets API. When notified, sends a message through a channel.
// Start is blocking. It runs until it receives a SIGTERM or SIGINT.
func Start(eventHandler handlers.Handler) {
	kubeClient := utils.GetClient()
	namespace := ""
	if value, ok := os.LookupEnv("KW_NAMESPACE"); ok {
		namespace = value
		log.Println("KubeWise operating in namespace", value, ". Operations in other namespaces will be ignored.")
	}

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
				return kubeClient.CoreV1().Secrets(namespace).List(options)
			},
			WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
				return kubeClient.CoreV1().Secrets(namespace).Watch(options)
			},
		},
		&api_v1.Secret{},
		0,
		cache.Indexers{},
	)

	c := newResourceController(kubeClient, eventHandler, informer)
	stopCh := make(chan struct{})
	defer close(stopCh)

	go c.run(stopCh)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	<-sigterm
}

func newResourceController(client kubernetes.Interface, eventHandler handlers.Handler, informer cache.SharedIndexInformer) *Controller {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	var newEvent Event
	var err error

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(secret interface{}) {
			newEvent.key, err = cache.MetaNamespaceKeyFunc(secret)
			newEvent.eventType = "create"
			newEvent.secretType = secret.(*api_v1.Secret).Type

			if err == nil {
				queue.Add(newEvent)
			}
		},

		UpdateFunc: func(secret, new interface{}) {
			newEvent.key, err = cache.MetaNamespaceKeyFunc(secret)
			newEvent.eventType = "update"
			newEvent.secretType = secret.(*api_v1.Secret).Type

			if err == nil {
				queue.Add(newEvent)
			}
		},

		DeleteFunc: func(secret interface{}) {
			newEvent.key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(secret)
			newEvent.eventType = "delete"
			newEvent.secretType = secret.(*api_v1.Secret).Type

			if err == nil {
				queue.Add(newEvent)
			}
		},
	})

	return &Controller{
		clientset:    client,
		informer:     informer,
		queue:        queue,
		eventHandler: eventHandler,
	}
}

func (c *Controller) run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	log.Println("Starting KubeWise controller")
	serverStartTime = time.Now().Local()

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	log.Println("KubeWise controller ready")

	wait.Until(c.runWorker, time.Second, stopCh)
}

// HasSynced is needed to satisfy the Controller interface.
func (c *Controller) HasSynced() bool {
	return c.informer.HasSynced()
}

// LastSyncResourceVersion is needed to satisfy the Controller interface.
func (c *Controller) LastSyncResourceVersion() string {
	return c.informer.LastSyncResourceVersion()
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
		// infinite loop
	}
}

func (c *Controller) processNextItem() bool {
	newEvent, quit := c.queue.Get()

	if quit {
		return false
	}
	defer c.queue.Done(newEvent)

	err := c.processItem(newEvent.(Event))

	if err == nil {
		c.queue.Forget(newEvent)
	} else if c.queue.NumRequeues(newEvent) < maxRetries {
		log.Printf("Error processing %s (will retry): %v", newEvent.(Event).key, err)
		c.queue.AddRateLimited(newEvent)
	} else {
		log.Printf("Error processing %s (giving up): %v", newEvent.(Event).key, err)
		c.queue.Forget(newEvent)
		utilruntime.HandleError(err)
	}

	return true
}

func (c *Controller) processItem(newEvent Event) error {
	object, _, err := c.informer.GetIndexer().GetByKey(newEvent.key)

	// GetByKey returns a nil object in the case where a Helm secret has been deleted. This means
	// we don't have access to the original secret at this point and can't inform the user about
	// which application has been successfully deleted.
	//
	// One approach to investigate in future would be to put the relevant details on the Event
	// and use that to provide the user information about what has been uninstalled.

	if err != nil {
		log.Fatalf("Error fetching secret with key %s from store: %v", newEvent.key, err)
		return err
	}

	// Uninstalling a Helm chart triggers a processItem but the secret has been deleted.
	// Without a nil check, we can see a panic when we type check the secret below.
	if object == nil {
		log.Println("Skipping nil secret", newEvent.eventType, "event for secret type:", newEvent.secretType)
		return nil
	}

	secret, ok := object.(*api_v1.Secret)

	if !ok {
		log.Println("Unable to cast 'object' (interface) as secret in", newEvent.eventType, "event:", object)
		return nil
	}

	if secret.Type != "helm.sh/release.v1" {
		log.Println("Skipping non-helm secret", newEvent.eventType, "event:", secret.Type)
		return nil
	}

	releaseEvent := &kwrelease.Event{
		SecretAction:         newEvent.eventType,
		CurrentReleaseSecret: secret,
	}
	err = releaseEvent.Init()

	if err != nil {
		return nil
	}

	// This event is the old release secret being marked as superseeded. There is no need to
	// inform the user of this action. It is internal bookkeeping.
	if releaseEvent.GetAction() == kwrelease.ActionPostReplaceSuperseded {
		return nil
	}

	switch releaseEvent.SecretAction {
	case "create":
		// "create" events are triggered for all the Helm secrets which are already in the cluster
		// when KubeWise starts up. Handling them normally would result in individual handler events
		// being triggered for each Helm chart which is already in the cluster. This could be a lot
		// of Slack messages being sent.
		//
		// Checking if the server started up less than zero seconds ago is a hacky way to prevent
		// this handler spam.
		if secret.ObjectMeta.CreationTimestamp.Sub(serverStartTime).Seconds() > 0 {
			c.eventHandler.HandleEvent(releaseEvent)
		}
		return nil

	case "update":
		c.eventHandler.HandleEvent(releaseEvent)
		return nil

	case "delete":
		c.eventHandler.HandleEvent(releaseEvent)
		return nil
	}

	return nil
}
