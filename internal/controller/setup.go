package controller

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

// Settle is how long events coalesce before one reconcile, so a burst of
// edits rolls the gateway once.
const Settle = time.Second

// delayed enqueues the single work item after Settle.
type delayed struct{ key reconcile.Request }

type queue = workqueue.TypedRateLimitingInterface[reconcile.Request]

func (d delayed) Create(_ context.Context, _ event.CreateEvent, q queue) { q.AddAfter(d.key, Settle) }
func (d delayed) Update(_ context.Context, _ event.UpdateEvent, q queue) { q.AddAfter(d.key, Settle) }
func (d delayed) Delete(_ context.Context, _ event.DeleteEvent, q queue) { q.AddAfter(d.key, Settle) }
func (d delayed) Generic(_ context.Context, _ event.GenericEvent, q queue) {
	q.AddAfter(d.key, Settle)
}

// SetupWithManager watches everything the resources file depends on.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	key := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: r.Secret}}
	h := delayed{key}
	generation := builder.WithPredicates(predicate.GenerationChangedPredicate{})
	inNamespace := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == r.Namespace
	}))

	// Reconcile once at start, even in an empty namespace.
	kick := make(chan event.GenericEvent, 1)
	kick <- event.GenericEvent{Object: &corev1.Secret{}}

	return ctrl.NewControllerManagedBy(mgr).
		Named("aisix-resources").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Watches(&v1alpha1.ProviderKey{}, h, generation, inNamespace).
		Watches(&v1alpha1.Model{}, h, generation, inNamespace).
		Watches(&v1alpha1.APIKey{}, h, generation, inNamespace).
		Watches(&corev1.Secret{}, h, inNamespace).
		Watches(&appsv1.Deployment{}, h, inNamespace).
		Watches(&corev1.Pod{}, h, inNamespace).
		WatchesRawSource(source.Channel(kick, handler.EventHandler(h))).
		Complete(r)
}
