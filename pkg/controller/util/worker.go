/*
Copyright 2018 The Kubernetes Authors.

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

package util

import (
	"time"

	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/workqueue"
)

type ReconcileFunc func(qualifiedName QualifiedName) ReconciliationStatus

type ReconcileWorker interface {
	Enqueue(qualifiedName QualifiedName)
	EnqueueForClusterSync(qualifiedName QualifiedName)
	EnqueueForError(qualifiedName QualifiedName)
	EnqueueForRetry(qualifiedName QualifiedName)
	EnqueueObject(obj pkgruntime.Object)
	EnqueueWithDelay(qualifiedName QualifiedName, delay time.Duration)
	Run(stopChan <-chan struct{})
	SetDelay(retryDelay, clusterSyncDelay time.Duration)
}

type WorkerTiming struct {
	Interval         time.Duration
	RetryDelay       time.Duration
	ClusterSyncDelay time.Duration
	InitialBackoff   time.Duration
	MaxBackoff       time.Duration
}

type asyncWorker struct {
	name string

	reconcile ReconcileFunc

	timing WorkerTiming

	// For triggering reconciliation of a single resource. This is
	// used when there is an add/update/delete operation on a resource
	// in either the API of the cluster hosting KubeFed or in the API
	// of a member cluster.
	deliverer *DelayingDeliverer

	// Work queue allowing parallel processing of resources
	queue workqueue.Interface

	// Backoff manager
	backoff *flowcontrol.Backoff
}

func NewReconcileWorker(name string, reconcile ReconcileFunc, timing WorkerTiming) ReconcileWorker {
	if timing.Interval == 0 {
		timing.Interval = time.Second * 1
	}
	if timing.RetryDelay == 0 {
		timing.RetryDelay = time.Second * 10
	}
	if timing.InitialBackoff == 0 {
		timing.InitialBackoff = time.Second * 5
	}
	if timing.MaxBackoff == 0 {
		timing.MaxBackoff = time.Minute
	}
	return &asyncWorker{
		name:      name,
		reconcile: reconcile,
		timing:    timing,
		deliverer: NewDelayingDeliverer(),
		queue:     workqueue.NewNamed(name),
		backoff:   flowcontrol.NewBackOff(timing.InitialBackoff, timing.MaxBackoff),
	}
}

func (w *asyncWorker) Enqueue(qualifiedName QualifiedName) {
	w.deliver(qualifiedName, 0, false)
}

func (w *asyncWorker) EnqueueForError(qualifiedName QualifiedName) {
	w.deliver(qualifiedName, 0, true)
}

func (w *asyncWorker) EnqueueForRetry(qualifiedName QualifiedName) {
	w.deliver(qualifiedName, w.timing.RetryDelay, false)
}

func (w *asyncWorker) EnqueueForClusterSync(qualifiedName QualifiedName) {
	w.deliver(qualifiedName, w.timing.ClusterSyncDelay, false)
}

func (w *asyncWorker) EnqueueObject(obj pkgruntime.Object) {
	qualifiedName := NewQualifiedName(obj)
	w.Enqueue(qualifiedName)
}

func (w *asyncWorker) EnqueueWithDelay(qualifiedName QualifiedName, delay time.Duration) {
	w.deliver(qualifiedName, delay, false)
}

func (w *asyncWorker) Run(stopChan <-chan struct{}) {
	StartBackoffGC(w.backoff, stopChan)
	w.deliverer.StartWithHandler(func(item *DelayingDelivererItem) {
		qualifiedName, ok := item.Value.(*QualifiedName)
		if ok {
			w.queue.Add(*qualifiedName)
		}
	})
	go wait.Until(w.worker, w.timing.Interval, stopChan)

	// Ensure all goroutines are cleaned up when the stop channel closes
	go func() {
		<-stopChan
		w.queue.ShutDown()
		w.deliverer.Stop()
	}()
}

func (w *asyncWorker) SetDelay(retryDelay, clusterSyncDelay time.Duration) {
	w.timing.RetryDelay = retryDelay
	w.timing.ClusterSyncDelay = clusterSyncDelay
}

// deliver adds backoff to delay if this delivery is related to some
// failure. Resets backoff if there was no failure.
func (w *asyncWorker) deliver(qualifiedName QualifiedName, delay time.Duration, failed bool) {
	key := qualifiedName.String()
	if failed {
		w.backoff.Next(key, time.Now())
		delay += w.backoff.Get(key)
	} else {
		w.backoff.Reset(key)
	}
	w.deliverer.DeliverAfter(key, &qualifiedName, delay)
}

func (w *asyncWorker) worker() {
	for w.reconcileOnce() {
	}
}

func (w *asyncWorker) reconcileOnce() bool {
	obj, quit := w.queue.Get()
	if quit {
		return false
	}
	defer w.queue.Done(obj)

	qualifiedName, ok := obj.(QualifiedName)
	if !ok {
		return true
	}

	status := w.reconcile(qualifiedName)
	switch status {
	case StatusAllOK:
		break
	case StatusError:
		w.EnqueueForError(qualifiedName)
	case StatusNeedsRecheck:
		w.EnqueueForRetry(qualifiedName)
	case StatusNotSynced:
		w.EnqueueForClusterSync(qualifiedName)
	}
	return true
}
