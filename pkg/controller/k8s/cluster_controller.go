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

package k8s

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/everoute/everoute/pkg/constants"
)

// PodReconciler watch pod and sync to endpoint
type SecretReconciler struct {
	Client     client.Client
	Scheme     *runtime.Scheme
	clusterSet sets.String
}

// Reconcile receive endpoint from work queue, synchronize the endpoint status
func (r *SecretReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	if !strings.HasPrefix(req.Name, "ecp-cluster-") {
		return ctrl.Result{}, nil
	}

	klog.Infof("SecretReconciler received secret %s reconcile", req.NamespacedName)

	secret := corev1.Secret{}
	// delete endpoint if pod is not found
	if err := r.Client.Get(ctx, req.NamespacedName, &secret); err != nil && errors.IsNotFound(err) {
		klog.Infof("Delete Secret %s", req.Name)

		return ctrl.Result{}, nil
	}

	if r.clusterSet.Has(secret.Name) {
		return ctrl.Result{}, nil
	}
	r.clusterSet.Insert(secret.Name)

	klog.Info(string(secret.Data["kubeconfig"]))
	WriteFile(string(secret.Data["kubeconfig"]), "/root/"+secret.Name)

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: "/root/" + secret.Name},
		&clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{
				Server: "",
			},
			CurrentContext: "",
		}).ClientConfig()
	if err != nil {
		klog.Error(err)
		return ctrl.Result{}, nil
	}
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(constants.ControllerRuntimeQPS, constants.ControllerRuntimeBurst)
	sch := runtime.NewScheme()
	corev1.AddToScheme(sch)
	networkingv1.AddToScheme(sch)
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: sch,
	})
	if err != nil {
		klog.Error(err)
		return ctrl.Result{}, nil
	}

	// pod controller
	if err = (&PodReconciler{
		erClient: r.Client,
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.Fatalf("unable to create pod controller: %s", err.Error())
	}
	klog.Info("start pod controller")

	// networkPolicy controller
	if err = (&NetworkPolicyReconciler{
		erClient: r.Client,
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.Fatalf("unable to create networkPolicy controller: %s", err.Error())
	}
	klog.Info("start networkPolicy controller")

	klog.Info("starting manager")

	go func() {
		if err := mgr.Start(make(<-chan struct{})); err != nil {
			klog.Fatalf("error while running manager: %s", err.Error())
		}
	}()

	return ctrl.Result{}, nil
}

func WriteFile(input string, fileName string) {
	file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		klog.Errorf("open file failed %s", err)
	}
	defer file.Close()

	write := bufio.NewWriter(file)
	write.WriteString(input)
	write.Flush()
}

// SetupWithManager create and add Endpoint Controller to the manager.
func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("can't setup with nil manager")
	}

	r.clusterSet = sets.NewString()

	c, err := controller.New("cluster-controller", mgr, controller.Options{
		MaxConcurrentReconciles: constants.DefaultMaxConcurrentReconciles,
		Reconciler:              r,
	})
	if err != nil {
		return err
	}

	if err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}

	return nil
}
