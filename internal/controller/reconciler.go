// Package controller reconciles AISIX custom resources into the gateway's
// standalone resources file.
//
// The file is one document, so the whole namespace is reconciled at once:
// any change to a custom resource, a Secret, the gateway Deployment or its
// pods re-renders it. A changed document is written to the gateway's
// resources Secret and rolled out by stamping its hash on the Deployment's
// pod template, because the gateway reloads the file only on SIGHUP or at
// boot.
package controller

import (
	"bytes"
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/render"
	"github.com/api7/aisix-controller/internal/validate"
)

// FieldManager names the controller in managed fields and labels.
const FieldManager = "aisix-controller"

// Reconciler writes the resources file of one gateway.
type Reconciler struct {
	Client     client.Client
	Namespace  string
	Secret     string
	SecretKey  string
	Deployment string
	Validator  validate.Validator
	Status     StatusReader
	// Resync re-checks everything this often.
	Resync time.Duration
	// Poll re-reads the gateway pods this often while a change is pending.
	Poll time.Duration
}

func (r *Reconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := client.InNamespace(r.Namespace)

	var pks v1alpha1.ProviderKeyList
	var models v1alpha1.ModelList
	var keys v1alpha1.APIKeyList
	var secrets corev1.SecretList
	for _, l := range []client.ObjectList{&pks, &models, &keys, &secrets} {
		if err := r.Client.List(ctx, l, ns); err != nil {
			return ctrl.Result{}, err
		}
	}

	var objects []v1alpha1.Object
	kinds := map[v1alpha1.Object]v1alpha1.Kind{}
	for i := range pks.Items {
		objects = append(objects, &pks.Items[i])
		kinds[&pks.Items[i]] = v1alpha1.KindProviderKey
	}
	for i := range models.Items {
		objects = append(objects, &models.Items[i])
		kinds[&models.Items[i]] = v1alpha1.KindModel
	}
	for i := range keys.Items {
		objects = append(objects, &keys.Items[i])
		kinds[&keys.Items[i]] = v1alpha1.KindAPIKey
	}

	sources := make([]render.Source, 0, len(objects))
	decodeErrs := map[render.Key]string{}
	for _, o := range objects {
		fields, err := v1alpha1.SpecFields(o.SpecRaw())
		s := render.Source{
			Kind: kinds[o], Name: o.GetName(), Generation: o.GetGeneration(),
			Created: o.GetCreationTimestamp().Time, Fields: fields,
		}
		if err != nil {
			s.Fields = map[string]any{}
			decodeErrs[s.Key()] = fmt.Sprintf("spec: %v", err)
		}
		sources = append(sources, s)
	}

	secretData := render.Secrets{}
	var current []byte
	var target *corev1.Secret
	for i := range secrets.Items {
		s := &secrets.Items[i]
		secretData[s.Name] = s.Data
		if s.Name == r.Secret {
			target = s
			current = s.Data[r.SecretKey]
		}
	}

	rendered := render.All(sources, secretData)
	for i := range rendered {
		if msg, ok := decodeErrs[rendered[i].Source.Key()]; ok {
			rendered[i].Entry, rendered[i].Err = nil, msg
		}
	}
	outcome, err := render.Assemble(ctx, rendered, render.ServedEntries(current), r.Validator)
	if err != nil {
		return ctrl.Result{}, err
	}
	if outcome.Error != "" {
		logger.Info("resources file rejected; keeping the served version", "error", outcome.Error)
	}

	// A gateway needs a file to start, so seed an empty one.
	desired := outcome.Document
	if desired == nil && target == nil {
		desired = render.Document(nil)
	}
	served := current
	if desired != nil && (target == nil || !bytes.Equal(current, desired)) {
		if err := r.writeSecret(ctx, target, desired); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("resources file written", "hash", render.Hash(desired)[:12])
		served = desired
	}
	hash := render.Hash(served)

	gw, err := r.gateway(ctx, hash)
	if err != nil {
		return ctrl.Result{}, err
	}

	written := outcome.Document != nil && bytes.Equal(outcome.Document, served)
	pending := false
	for _, o := range objects {
		key := render.Key{Kind: kinds[o], Name: o.GetName()}
		d, known := outcome.Dispositions[key]
		st := desiredStatus(*o.ResourceStatus(), o.GetGeneration(), d, known, outcome.Error, written, gw)
		if d.State == render.Applied && !gw.Ready {
			pending = true
		}
		if err := r.patchStatus(ctx, o, st); err != nil {
			return ctrl.Result{}, err
		}
	}

	if pending {
		return ctrl.Result{RequeueAfter: r.Poll}, nil
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

func (r *Reconciler) writeSecret(ctx context.Context, existing *corev1.Secret, doc []byte) error {
	labels := map[string]string{"app.kubernetes.io/managed-by": FieldManager}
	annotations := map[string]string{v1alpha1.HashAnnotation: render.Hash(doc)}
	if existing == nil {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: r.Secret, Namespace: r.Namespace, Labels: labels, Annotations: annotations,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{r.SecretKey: doc},
		}
		if err := r.Client.Create(ctx, s, client.FieldOwner(FieldManager)); err != nil {
			return fmt.Errorf("creating Secret %s: %w", r.Secret, err)
		}
		return nil
	}
	s := existing.DeepCopy()
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	for k, v := range labels {
		s.Labels[k] = v
	}
	for k, v := range annotations {
		s.Annotations[k] = v
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data[r.SecretKey] = doc
	if err := r.Client.Update(ctx, s, client.FieldOwner(FieldManager)); err != nil {
		return fmt.Errorf("updating Secret %s: %w", r.Secret, err)
	}
	return nil
}

// gateway stamps the served hash on the Deployment and reports whether its
// pods serve it.
func (r *Reconciler) gateway(ctx context.Context, hash string) (Gateway, error) {
	var dep appsv1.Deployment
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.Deployment}, &dep)
	if apierrors.IsNotFound(err) {
		return evaluate(nil, nil, nil, nil, hash), nil
	}
	if err != nil {
		return Gateway{}, err
	}

	if !stamped(&dep, hash) {
		patch := client.MergeFrom(dep.DeepCopy())
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[v1alpha1.HashAnnotation] = hash
		if err := r.Client.Patch(ctx, &dep, patch, client.FieldOwner(FieldManager)); err != nil {
			return Gateway{}, fmt.Errorf("stamping Deployment %s: %w", r.Deployment, err)
		}
		log.FromContext(ctx).Info("gateway rollout started", "hash", hash[:12])
		return Gateway{Reason: "Pending", Message: "rollout started"}, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return Gateway{}, err
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(r.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return Gateway{}, err
	}
	ready := readyPods(pods.Items)
	statuses := map[string]PodStatus{}
	errs := map[string]error{}
	for _, p := range ready {
		s, err := r.Status.Read(ctx, p)
		if err != nil {
			errs[p.Name] = err
			continue
		}
		statuses[p.Name] = s
	}
	return evaluate(&dep, ready, statuses, errs, hash), nil
}

func (r *Reconciler) patchStatus(ctx context.Context, o v1alpha1.Object, st v1alpha1.ResourceStatus) error {
	if equality.Semantic.DeepEqual(*o.ResourceStatus(), st) {
		return nil
	}
	obj, ok := o.(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a client.Object", o)
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	*o.ResourceStatus() = st
	if err := r.Client.Status().Patch(ctx, obj, patch, client.FieldOwner(FieldManager)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("status of %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
	return nil
}
