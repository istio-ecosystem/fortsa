/*
Copyright 2025.

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

package controller

import (
	"context"
	"time"

	"golang.org/x/time/rate"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/hercynium/istio-fortsa/internal/common"
	"github.com/hercynium/istio-fortsa/internal/config"
	"github.com/hercynium/istio-fortsa/internal/k8s"
)

// NamespaceReconciler reconciles a Namespace object
type NamespaceReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Config     config.FortsaConfig
	KubeClient *kubernetes.Clientset
}

type controllerSet map[string]bool

// Allow read-only access to Namespaces
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces/finalizers,verbs=get;list;watch

// Allow read-only access to MutatingWebhookConfigurations
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations/finalizers,verbs=get;list;watch

// Allow necessary access to Pods
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods/finalizers,verbs=get;list;watch

// Allow read-only access to everything
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Namespace object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.0/pkg/reconcile
func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var log = log.FromContext(ctx)

	// name of this namespace
	var nsName = req.Name

	// istio rev pods in this namespace should use
	var nsDesiredRev, err = r.getNamespaceDesiredRev(ctx, nsName)
	if err != nil {
		log.Error(err, "Failed to get istio revision associated with this namespace", "ns", nsName)
		return ctrl.Result{}, err
	}

	// get pods in the namespace
	var pods = &corev1.PodList{}
	err = r.List(ctx, pods, &client.ListOptions{
		Namespace: nsName,
	})
	if err != nil {
		log.Error(err, "Failed to get list of pods in this namespace", "ns", nsName)
		return ctrl.Result{}, err
	}

	// check each pod if it's using the desired revision of Istio
	var seenControllers = make(controllerSet)
	for _, pod := range pods.Items {
		var podIstioRev = pod.Annotations[common.IstioRevLabel]
		if podIstioRev != "" && podIstioRev != nsDesiredRev {
			log.Info("Outdated pod found", "ns", nsName, "nsRev", nsDesiredRev, "pod", pod.Name, "podRev", podIstioRev)
			err := r.RestartPodController(ctx, req, pod, seenControllers)
			if err != nil {
				log.Error(err, "Couldn't restart controller for pod", "ns", pod.Namespace, "pod", pod.Name)
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) RestartPodController(ctx context.Context, req ctrl.Request, pod corev1.Pod, seenControllers controllerSet) error {
	var log = log.FromContext(ctx)

	// find the controller of the pod
	pc, err := k8s.FindPodController(ctx, *r.KubeClient, pod)
	if err != nil {
		log.Info("Could not find controller for pod", "err", err, "ns", pod.Namespace, "pod", pod.Name)
		// not returning error, since it (pod or controller) probably was deleted
		return nil
	}

	if seenControllers[pc.GetName()] {
		log.Info("Alredy seen the controller for this pod",
			"ns", pod.Namespace, "pod", pod.Name,
			"podController", pc.GetName(), "podControllerKind", pc.GetKind())
		return nil
	}
	seenControllers[pc.GetName()] = true

	// make sure the controller is one we can restart
	switch pc.GetKind() {
	case "DaemonSet", "Deployment", "StatefulSet":
		break
	default:
		log.Info("Upsupported controller type for restart",
			"ns", pod.Namespace, "pod", pod.Name,
			"podController", pc.GetName(), "podControllerKind", pc.GetKind())
		return nil
	}

	// do the thing
	dryRun := r.Config.DryRun
	err = k8s.DoRolloutRestart(ctx, r.Client, pc, dryRun)
	if err != nil {
		log.Error(err, "Error doing rollout restart on controller for pod",
			"ns", pod.Namespace, "pod", pod.Name,
			"podController", pc.GetName(), "podControllerKind", pc.GetKind())
		return err
	}

	return nil
}

func (r *NamespaceReconciler) getNamespaceDesiredRev(ctx context.Context, nsName string) (string, error) {
	var _ = log.FromContext(ctx)

	var ns = &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: nsName}, ns, &client.GetOptions{})
	if err != nil {
		return "", err
	}

	// istio revision or tag this namespace is configured to use
	var nsIstioRevLabelValue = ns.Labels[common.IstioRevLabel]

	// get the webhooks that correspond to the label on the namespace
	var webhooks = &admissionregistrationv1.MutatingWebhookConfigurationList{}
	err = r.List(ctx, webhooks, &client.ListOptions{
		LabelSelector: labels.Set{common.IstioTagLabel: nsIstioRevLabelValue}.AsSelector(),
	})
	if err != nil {
		return "", err
	}

	// map webhook tags to istio revisions for easy lookup. { tag => rev }
	var tagMap = make(map[string]string)
	for _, webhook := range webhooks.Items {
		tagMap[webhook.Labels[common.IstioTagLabel]] = webhook.Labels[common.IstioRevLabel]
	}

	// the revision that corresponds to the tag indicated by the label on the namespace
	var nsDesiredRev = tagMap[ns.Labels[common.IstioRevLabel]]

	return nsDesiredRev, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// also watch changes to MutatingWebhookConfigurations, because changing the istio
	// webhooks means we need to reconcile namespaces.
	src := source.Kind(
		mgr.GetCache(),
		&admissionregistrationv1.MutatingWebhookConfiguration{},
		r.webhookEventHandlers(),
		onlyReconcileIstioWebhooks(),
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Named("namespace").
		WithEventFilter(onlyReconcileIstioRevLabeled()).
		WatchesRawSource(src).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             r.namespaceControllerRateLimiter(),
		}).
		Complete(r)
}

// filter namespace events we want to reconcile
func onlyReconcileIstioRevLabeled() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetLabels()[common.IstioRevLabel] != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// only reconcile if the value of the label changed
			var oldLabels = e.ObjectOld.GetLabels()
			var newLabels = e.ObjectNew.GetLabels()
			return oldLabels[common.IstioRevLabel] != newLabels[common.IstioRevLabel]
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// no namespace means no label to think about. Skip the event.
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetLabels()[common.IstioRevLabel] != ""
		},
	}
}

func (r *NamespaceReconciler) webhookEventHandlers() handler.TypedFuncs[*admissionregistrationv1.MutatingWebhookConfiguration, reconcile.Request] {
	return handler.TypedFuncs[*admissionregistrationv1.MutatingWebhookConfiguration, reconcile.Request]{
		CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[*admissionregistrationv1.MutatingWebhookConfiguration], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			var log = log.FromContext(ctx)
			log.Info("Reconciling Create for Webhook", "name", e.Object.Name)
			for _, nsRec := range r.reconcileWebhookConfig(ctx, e.Object) {
				q.Add(nsRec)
			}
		},
		UpdateFunc: func(ctx context.Context, e event.TypedUpdateEvent[*admissionregistrationv1.MutatingWebhookConfiguration], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			var log = log.FromContext(ctx)
			log.Info("Reconciling Update for Webhook", "name", e.ObjectNew.Name)
			for _, nsRec := range r.reconcileWebhookConfig(ctx, e.ObjectNew) {
				q.Add(nsRec)
			}
		},
		/*
			DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[*admissionregistrationv1.MutatingWebhookConfiguration], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      e.Object.Name,
					Namespace: e.Object.Namespace,
				}})
			},
			GenericFunc: func(ctx context.Context, e event.TypedGenericEvent[*admissionregistrationv1.MutatingWebhookConfiguration], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      e.Object.Name,
					Namespace: e.Object.Namespace,
				}})
			},
		*/

	}
}

// all istio webhooks should have this "app" label value
const webhookAppLabelValue = "sidecar-injector"

// the webhooks we're interested in...
func isIstioTaggedWebhook(o client.Object) bool {
	labels := o.GetLabels()
	return labels["app"] == webhookAppLabelValue && labels[common.IstioTagLabel] != ""
}

func (r *NamespaceReconciler) reconcileWebhookConfig(ctx context.Context,
	webhook *admissionregistrationv1.MutatingWebhookConfiguration) []reconcile.Request {
	var log = log.FromContext(ctx)

	var tag = webhook.Labels[common.IstioTagLabel] // canary, stable, default, etc...
	var rev = webhook.Labels[common.IstioRevLabel] // istiod instance revision

	if !isIstioTaggedWebhook(webhook) {
		return []reconcile.Request{}
	}

	log.Info("Istio Webhook Found", "webhookName", webhook.Name, "istioTag", tag, "istioRev", rev)

	// find namespaces that use this webhook's tag
	var nsList = &corev1.NamespaceList{}
	err := r.List(ctx, nsList, &client.ListOptions{
		LabelSelector: labels.Set{common.IstioRevLabel: tag}.AsSelector(),
	})
	if err != nil {
		log.Error(err, "Failed to get list of namespaces labeled for istio revision",
			"webhookName", webhook.Name, "istioTag", tag, "istioRev", rev)
		return []reconcile.Request{}
	}

	var nsRecs = []reconcile.Request{}
	for _, ns := range nsList.Items {
		log.Info("Enqueuing Namespace", "ns", ns.Name)
		rec := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: ns.Name, Namespace: ns.Namespace},
		}
		nsRecs = append(nsRecs, rec)
	}

	return nsRecs
}

func onlyReconcileIstioWebhooks() predicate.TypedPredicate[*admissionregistrationv1.MutatingWebhookConfiguration] {
	return predicate.TypedFuncs[*admissionregistrationv1.MutatingWebhookConfiguration]{
		CreateFunc: func(e event.TypedCreateEvent[*admissionregistrationv1.MutatingWebhookConfiguration]) bool {
			return isIstioTaggedWebhook(e.Object)
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*admissionregistrationv1.MutatingWebhookConfiguration]) bool {
			return isIstioTaggedWebhook(e.ObjectNew)
		},
		DeleteFunc: func(e event.TypedDeleteEvent[*admissionregistrationv1.MutatingWebhookConfiguration]) bool {
			return isIstioTaggedWebhook(e.Object)
		},
		GenericFunc: func(e event.TypedGenericEvent[*admissionregistrationv1.MutatingWebhookConfiguration]) bool {
			return isIstioTaggedWebhook(e.Object)
		},
	}
}

func (r *NamespaceReconciler) namespaceControllerRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	// TODO: figure out how to implement desired rate-limiting semantics here...
	// for example: only perform 5 restarts every minute, and no more than 5 active restarts at a time
	var restartsPerMinute = r.Config.RestartsPerMinute
	var activeRestartLimit = r.Config.ActiveRestartLimit

	limit := rate.Limit(1.0 / (60.0 / restartsPerMinute))
	limiter := rate.NewLimiter(limit, activeRestartLimit)
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 1000*time.Second),
		// This is only for retry speed and its only the overall factor (not per item)
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: limiter},
	)
}
