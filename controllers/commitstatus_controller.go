/*
Copyright 2020 The Flux authors

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

package controllers

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	kuberecorder "k8s.io/client-go/tools/record"

	"github.com/fluxcd/notification-controller/api/v1beta2"
)

// This is already declared in the alert controller -- I'm not currently sure if we'll need to change this.
//var (
//	ProviderIndexKey = ".metadata.provider"
//)

// CommitStatusReconciler reconciles a CommitStatus object
type CommitStatusReconciler struct {
	client.Client
	helper.Metrics
	kuberecorder.EventRecorder

	Scheme *runtime.Scheme
}

type CommitStatusReconcilerOptions struct {
	MaxConcurrentReconciles int
}

func (r *CommitStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, CommitStatusReconcilerOptions{})
}

func (r *CommitStatusReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts CommitStatusReconcilerOptions) error {
	if err := mgr.GetFieldIndexer().IndexField(context.TODO(), &v1beta2.CommitStatus{}, ProviderIndexKey,
		func(o client.Object) []string {
			commitStatus := o.(*v1beta2.CommitStatus)
			return []string{
				fmt.Sprintf("%s/%s", commitStatus.GetNamespace(), commitStatus.Spec.ProviderRef.Name),
			}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta2.CommitStatus{}).
		WithEventFilter(predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{})).
		Watches(
			&source.Kind{Type: &v1beta2.Provider{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForProviderChange),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

// +kubebuilder:rbac:groups=notification.toolkit.fluxcd.io,resources=commitStatuss,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=notification.toolkit.fluxcd.io,resources=commitStatuss/status,verbs=get;update;patch

func (r *CommitStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	commitStatus := &v1beta2.CommitStatus{}
	if err := r.Get(ctx, req.NamespacedName, commitStatus); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// record suspension metrics
	r.RecordSuspend(ctx, commitStatus, commitStatus.Spec.Suspend)

	if commitStatus.Spec.Suspend {
		log.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(commitStatus, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		patchOpts := []patch.Option{
			patch.WithOwnedConditions{
				Conditions: []string{
					meta.ReadyCondition,
					meta.ReconcilingCondition,
					meta.StalledCondition,
				},
			},
		}

		if retErr == nil && (result.IsZero() || !result.Requeue) {
			conditions.Delete(commitStatus, meta.ReconcilingCondition)

			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})

			readyCondition := conditions.Get(commitStatus, meta.ReadyCondition)
			switch readyCondition.Status {
			case metav1.ConditionFalse:
				// As we are no longer reconciling and the end-state is not ready, the reconciliation has stalled
				conditions.MarkStalled(commitStatus, readyCondition.Reason, readyCondition.Message)
			case metav1.ConditionTrue:
				// As we are no longer reconciling and the end-state is ready, the reconciliation is no longer stalled
				conditions.Delete(commitStatus, meta.StalledCondition)
			}
		}

		if err := patchHelper.Patch(ctx, commitStatus, patchOpts...); err != nil {
			retErr = kerrors.NewAggregate([]error{retErr, err})
		}

		r.Metrics.RecordReadiness(ctx, commitStatus)
		r.Metrics.RecordDuration(ctx, commitStatus, start)
	}()

	return r.reconcile(ctx, commitStatus)
}

func (r *CommitStatusReconciler) reconcile(ctx context.Context, commitStatus *v1beta2.CommitStatus) (ctrl.Result, error) {
	// Mark the resource as under reconciliation
	conditions.MarkReconciling(commitStatus, meta.ProgressingReason, "")

	// validate commitStatus spec and provider
	if err := r.validate(ctx, commitStatus); err != nil {
		conditions.MarkFalse(commitStatus, meta.ReadyCondition, v1beta2.ValidationFailedReason, err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	conditions.MarkTrue(commitStatus, meta.ReadyCondition, meta.SucceededReason, v1beta2.InitializedReason)
	ctrl.LoggerFrom(ctx).Info("CommitStatus initialized")

	return ctrl.Result{}, nil
}

func (r *CommitStatusReconciler) validate(ctx context.Context, commitStatus *v1beta2.CommitStatus) error {
	provider := &v1beta2.Provider{}
	providerName := types.NamespacedName{Namespace: commitStatus.Namespace, Name: commitStatus.Spec.ProviderRef.Name}
	if err := r.Get(ctx, providerName, provider); err != nil {
		// log not found errors since they get filtered out
		ctrl.LoggerFrom(ctx).Error(err, "failed to get provider %s", providerName.String())
		return fmt.Errorf("failed to get provider '%s': %w", providerName.String(), err)
	}

	if !conditions.IsReady(provider) {
		return fmt.Errorf("provider %s is not ready", providerName.String())
	}

	return nil
}

func (r *CommitStatusReconciler) requestsForProviderChange(o client.Object) []reconcile.Request {
	provider, ok := o.(*v1beta2.Provider)
	if !ok {
		panic(fmt.Errorf("expected a provider, got %T", o))
	}

	ctx := context.Background()
	var list v1beta2.CommitStatusList
	if err := r.List(ctx, &list, client.MatchingFields{
		ProviderIndexKey: client.ObjectKeyFromObject(provider).String(),
	}); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
	}

	return reqs
}
