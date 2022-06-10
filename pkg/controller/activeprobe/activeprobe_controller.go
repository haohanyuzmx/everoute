/*
Copyright 2021 The Everoute Authors.

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

package activeprobe

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	activeprobev1alph1 "github.com/everoute/everoute/pkg/apis/activeprobe/v1alpha1"
	securityv1alpha1 "github.com/everoute/everoute/pkg/apis/security/v1alpha1"
	"github.com/everoute/everoute/pkg/constants"
	ctrltypes "github.com/everoute/everoute/pkg/controller/types"
)

const (
	controllerName        = "activeprobe-controller"
	externalIDIndex       = "externalIDIndex"
	endpointExternalIDKey = "iface-id"
	// Min and max data plane tag for activeprobes. minTagNum is 7 (0b000111), maxTagNum is 59 (0b111011).
	// As per RFC2474, 16 different DSCP values are we reserved for Experimental or Local Use, which we use as the 16 possible data plane tag values.
	// tagStep is 4 (0b100) to keep last 2 bits at 0b11.
	tagStep   uint8 = 0b100
	minTagNum uint8 = 0b1*tagStep + 0b11
	maxTagNum uint8 = 0b1110*tagStep + 0b11
)

type Reconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	syncQueue               workqueue.RateLimitingInterface
	RunningActiveprobeMutex sync.Mutex
	RunningActiveprobe      map[uint8]string // tag->activeProbeName if ap.Status.State is Running
}

// SetupWithManager create and add Endpoint Controller to the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("can't setup with nil manager")
	}

	r.RunningActiveprobe = make(map[uint8]string)

	c, err := controller.New(controllerName, mgr, controller.Options{
		MaxConcurrentReconciles: constants.DefaultMaxConcurrentReconciles,
		Reconciler:              r,
	})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &activeprobev1alph1.ActiveProbe{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var err error
	ctx := context.Background()
	ap := activeprobev1alph1.ActiveProbe{}
	if err = r.Client.Get(ctx, req.NamespacedName, &ap); err != nil {
		klog.Errorf("unable to fetch activeprobe %s: %s", req.Name, err.Error())
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch ap.Status.State {
	case "":
		err = r.runActiveProbe(&ap)
	case activeprobev1alph1.ActiveProbeRunning:
		err = r.checkActiveProbeStatus(&ap)
	case activeprobev1alph1.ActiveProbeFailed:
		r.deallocateTagForAP(&ap)
	default:
	}

	return ctrl.Result{}, err
}

func (r *Reconciler) AddEndpointInfo(ap *activeprobev1alph1.ActiveProbe) error {
	srcEpExternalIDValue := ap.Spec.Source.Endpoint
	srcEndpointID := ctrltypes.ExternalID{
		Name:  endpointExternalIDKey,
		Value: srcEpExternalIDValue,
	}
	dstEpExternalIDValue := ap.Spec.Destination.Endpoint
	dstEndpointID := ctrltypes.ExternalID{
		Name:  endpointExternalIDKey,
		Value: dstEpExternalIDValue,
	}

	var epList = securityv1alpha1.EndpointList{}
	err := r.Client.List(context.Background(), &epList)
	if err != nil {
		klog.Errorf("list endpoint: %s", err)
		return nil
	}
	for _, ep := range epList.Items {
		if ep.Spec.Reference.ExternalIDName == srcEndpointID.Name && ep.Spec.Reference.ExternalIDValue == srcEndpointID.Value {
			ap.Spec.Source.IP = ep.Status.IPs[0].String()
			ap.Spec.Source.MAC = ep.Status.MacAddress
			ap.Spec.Source.AgentName = ep.Status.Agents[0]
			ap.Spec.Source.BridgeName = ep.Status.BridgeName
			ap.Spec.Source.Ofport = ep.Status.Ofport
		}
		if ep.Spec.Reference.ExternalIDName == dstEndpointID.Name && ep.Spec.Reference.ExternalIDValue == dstEndpointID.Value {
			ap.Spec.Destination.IP = ep.Status.IPs[0].String()
			ap.Spec.Destination.MAC = ep.Status.MacAddress
			ap.Spec.Destination.AgentName = ep.Status.Agents[0]
			ap.Spec.Destination.BridgeName = ep.Status.BridgeName
			ap.Spec.Destination.Ofport = ep.Status.Ofport
		}
	}
	return nil
}

func (r *Reconciler) allocateTag(name string) (uint8, error) {
	r.RunningActiveprobeMutex.Lock()
	defer r.RunningActiveprobeMutex.Unlock()

	for _, n := range r.RunningActiveprobe {
		if n == name {
			return 0, nil
		}
	}
	for i := minTagNum; i <= maxTagNum; i += tagStep {
		if _, ok := r.RunningActiveprobe[i]; !ok {
			r.RunningActiveprobe[i] = name
			return i, nil
		}
	}
	return 0, fmt.Errorf("number of on-going ActiveProve operations already reached the upper limit: %d", maxTagNum)
}

func (r *Reconciler) deallocateTagForAP(ap *activeprobev1alph1.ActiveProbe) {
	if ap.Status.Tag != 0 {
		r.deallocateTag(ap.Name, ap.Status.Tag)
	}
}

func (r *Reconciler) deallocateTag(name string, tag uint8) {
	r.RunningActiveprobeMutex.Lock()
	defer r.RunningActiveprobeMutex.Unlock()
	if existingActiveProbeName, ok := r.RunningActiveprobe[tag]; ok {
		if name == existingActiveProbeName {
			delete(r.RunningActiveprobe, tag)
		}
	}
}

func (r *Reconciler) validateActiveProbe(ap *activeprobev1alph1.ActiveProbe) error {
	return nil
}

func (r *Reconciler) updateActiveProbeStatus(ap *activeprobev1alph1.ActiveProbe,
	state activeprobev1alph1.ActiveProbeState, reason string, tag uint8) error {
	update := ap.DeepCopy()
	update.Status.State = state
	update.Status.Tag = tag
	if reason != "" {
		update.Status.Reason = reason
	}
	err := r.Client.Update(context.TODO(), update, &client.UpdateOptions{})
	err = r.Client.Status().Update(context.TODO(), update, &client.UpdateOptions{})
	return err
}

/* TODO */
func (r *Reconciler) runActiveProbe(ap *activeprobev1alph1.ActiveProbe) error {
	if err := r.validateActiveProbe(ap); err != nil {
		klog.Errorf("Invalid ActiveProbe request %v", ap)
		return r.updateActiveProbeStatus(ap, activeprobev1alph1.ActiveProbeFailed, fmt.Sprintf("Invalid ActiveProbe request, err: %+v", err), 0)
	}

	// Allocate data plane tag.
	tag, err := r.allocateTag(ap.Name)
	if err != nil {
		return err
	}
	if tag == 0 {
		return nil
	}

	err = r.AddEndpointInfo(ap)
	if err != nil {
		return err
	}

	err = r.updateActiveProbeStatus(ap, activeprobev1alph1.ActiveProbeRunning, "", tag)
	if err != nil {
		r.deallocateTag(ap.Name, tag)
	}
	return err
}

/* TODO */
func (r *Reconciler) checkActiveProbeStatus(ap *activeprobev1alph1.ActiveProbe) error {
	return nil
}