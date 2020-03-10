// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infrastructure

import (
	"context"
	"fmt"

	extensionscontroller "github.com/gardener/gardener-extensions/pkg/controller"
	"github.com/gardener/gardener-extensions/pkg/util"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardencorev1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
)

const (
	// EventInfrastructureReconciliation an event reason to describe infrastructure reconciliation.
	EventInfrastructureReconciliation string = "InfrastructureReconciliation"
	// EventInfrastructureDeleton an event reason to describe infrastructure deletion.
	EventInfrastructureDeleton string = "InfrastructureDeleton"
	// EventInfrastructureMigration an event reason to describe infrastructure migration.
	EventInfrastructureMigration string = "InfrastructureMigration"
)

type reconciler struct {
	logger   logr.Logger
	actuator Actuator

	ctx      context.Context
	client   client.Client
	recorder record.EventRecorder
}

// NewReconciler creates a new reconcile.Reconciler that reconciles
// infrastructure resources of Gardener's `extensions.gardener.cloud` API group.
func NewReconciler(mgr manager.Manager, actuator Actuator) reconcile.Reconciler {
	return extensionscontroller.OperationAnnotationWrapper(
		&extensionsv1alpha1.Infrastructure{},
		&reconciler{
			logger:   log.Log.WithName(ControllerName),
			actuator: actuator,
			recorder: mgr.GetEventRecorderFor(ControllerName),
		},
	)
}

func (r *reconciler) InjectFunc(f inject.Func) error {
	return f(r.actuator)
}

func (r *reconciler) InjectClient(client client.Client) error {
	r.client = client
	return nil
}

func (r *reconciler) InjectStopChannel(stopCh <-chan struct{}) error {
	r.ctx = util.ContextFromStopChannel(stopCh)
	return nil
}

func (r *reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	infrastructure := &extensionsv1alpha1.Infrastructure{}
	if err := r.client.Get(r.ctx, request.NamespacedName, infrastructure); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	cluster, err := extensionscontroller.GetCluster(r.ctx, r.client, infrastructure.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}

	operationType := gardencorev1beta1helper.ComputeOperationType(infrastructure.ObjectMeta, infrastructure.Status.LastOperation)

	switch {
	case isInfraMigrated(infrastructure):
		return reconcile.Result{}, nil
	case operationType == gardencorev1beta1.LastOperationTypeMigrate:
		return r.migrate(infrastructure, cluster)
	case infrastructure.DeletionTimestamp != nil:
		return r.delete(infrastructure, cluster)
	case infrastructure.Annotations[gardencorev1beta1constants.GardenerOperation] == gardencorev1beta1constants.GardenerOperationRestore:
		return r.restore(infrastructure, cluster, operationType)
	default:
		return r.reconcile(infrastructure, cluster, operationType)
	}
}

func (r *reconciler) reconcile(infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster, operationType gardencorev1beta1.LastOperationType) (reconcile.Result, error) {
	if err := extensionscontroller.EnsureFinalizer(r.ctx, r.client, FinalizerName, infrastructure); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.updateStatusProcessing(r.ctx, infrastructure, operationType, "Reconciling the infrastructure"); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the reconciliation of infrastructure", "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureReconciliation, "Reconciling the infrastructure")
	if err := r.actuator.Reconcile(r.ctx, infrastructure, cluster); err != nil {
		msg := "Error reconciling infrastructure"
		r.recorder.Event(infrastructure, corev1.EventTypeWarning, EventInfrastructureReconciliation, fmt.Sprintf("%s: %+v", msg, err))
		utilruntime.HandleError(r.updateStatusError(r.ctx, extensionscontroller.ReconcileErrCauseOrErr(err), infrastructure, operationType, msg))
		r.logger.Error(err, msg, "infrastructure", infrastructure.Name)
		return extensionscontroller.ReconcileErr(err)
	}

	msg := "Successfully reconciled infrastructure"
	r.logger.Info(msg, "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureReconciliation, msg)
	if err := r.updateStatusSuccess(r.ctx, infrastructure, operationType, msg); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) delete(infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster) (reconcile.Result, error) {
	hasFinalizer, err := extensionscontroller.HasFinalizer(infrastructure, FinalizerName)
	if err != nil {
		r.logger.Error(err, "Could not instantiate finalizer deletion")
		return reconcile.Result{}, err
	}
	if !hasFinalizer {
		r.logger.Info("Deleting infrastructure causes a no-op as there is no finalizer.", "infrastructure", infrastructure.Name)
		return reconcile.Result{}, nil
	}

	if err := r.updateStatusProcessing(r.ctx, infrastructure, gardencorev1beta1.LastOperationTypeDelete, "Deleting the infrastructure"); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the deletion of infrastructure", "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureDeleton, "Deleting the infrastructure")
	if err := r.actuator.Delete(r.ctx, infrastructure, cluster); err != nil {
		msg := "Error deleting infrastructure"
		r.recorder.Eventf(infrastructure, corev1.EventTypeWarning, EventInfrastructureDeleton, fmt.Sprintf("%s: %+v", msg, err))
		utilruntime.HandleError(r.updateStatusError(r.ctx, extensionscontroller.ReconcileErrCauseOrErr(err), infrastructure, gardencorev1beta1.LastOperationTypeDelete, msg))
		r.logger.Error(err, msg, "infrastructure", infrastructure.Name)
		return extensionscontroller.ReconcileErr(err)
	}

	msg := "Successfully deleted infrastructure"
	r.logger.Info(msg, "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureDeleton, msg)
	if err := r.updateStatusSuccess(r.ctx, infrastructure, gardencorev1beta1.LastOperationTypeDelete, msg); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Removing finalizer.", "infrastructure", infrastructure.Name)
	if err := extensionscontroller.DeleteFinalizer(r.ctx, r.client, FinalizerName, infrastructure); err != nil {
		r.logger.Error(err, "Error removing finalizer from Infrastructure", "infrastructure", infrastructure.Name)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) migrate(infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster) (reconcile.Result, error) {
	if err := r.updateStatusProcessing(r.ctx, infrastructure, gardencorev1beta1.LastOperationTypeMigrate, "Starting Migration of the Infrastructure"); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the migration of infrastructure", "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureMigration, "Migrating the infrastructure")
	if err := r.actuator.Migrate(r.ctx, infrastructure, cluster); err != nil {
		msg := "Error migrating infrastructure"
		r.recorder.Eventf(infrastructure, corev1.EventTypeWarning, EventInfrastructureMigration, fmt.Sprintf("%s: %+v", msg, err))
		utilruntime.HandleError(r.updateStatusError(r.ctx, extensionscontroller.ReconcileErrCauseOrErr(err), infrastructure, gardencorev1beta1.LastOperationTypeMigrate, msg))
		r.logger.Error(err, msg, "infrastructure", infrastructure.Name)
		return extensionscontroller.ReconcileErr(err)
	}

	msg := "Successfully migrate infrastructure"
	r.logger.Info(msg, "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureMigration, msg)
	if err := r.updateStatusSuccess(r.ctx, infrastructure, gardencorev1beta1.LastOperationTypeMigrate, "Successfully migrate Infrastructure"); err != nil {
		msg := "Error migrating infrastructure"
		r.recorder.Eventf(infrastructure, corev1.EventTypeWarning, EventInfrastructureMigration, fmt.Sprintf("%s: %+v", msg, err))
		utilruntime.HandleError(r.updateStatusError(r.ctx, extensionscontroller.ReconcileErrCauseOrErr(err), infrastructure, gardencorev1beta1.LastOperationTypeMigrate, msg))
		r.logger.Error(err, msg, "infrastructure", infrastructure.Name)
		return reconcile.Result{}, err
	}

	r.logger.Info("Removing finalizer.", "infrastructure", fmt.Sprintf("%s/%s", infrastructure.Namespace, infrastructure.Name))
	if err := extensionscontroller.DeleteFinalizer(r.ctx, r.client, FinalizerName, infrastructure); err != nil {
		r.logger.Error(err, "Error removing finalizer from Infrastructure", "worker", fmt.Sprintf("%s/%s", infrastructure.Namespace, infrastructure.Name))
		return reconcile.Result{}, err
	}

	// remove operation annotation 'migrate'
	if err := removeAnnotation(r.ctx, r.client, infrastructure, gardencorev1beta1constants.GardenerOperation); err != nil {
		r.logger.Error(err, "Error removing annotation from Infrastructure", "annotation", fmt.Sprintf("%s/%s", gardencorev1beta1constants.GardenerOperation, gardencorev1beta1constants.GardenerOperationMigrate), "infrastructure", fmt.Sprintf("%s/%s", infrastructure.Namespace, infrastructure.Name))
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) restore(infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster, operationType gardencorev1beta1.LastOperationType) (reconcile.Result, error) {
	if err := r.updateStatusProcessing(r.ctx, infrastructure, operationType, "Restoring the worker"); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the restoration of infrastructure", "infrastructure", infrastructure.Name)
	r.recorder.Event(infrastructure, corev1.EventTypeNormal, EventInfrastructureMigration, "Restoring the infrastructure")
	if err := r.actuator.Restore(r.ctx, infrastructure, cluster); err != nil {
		r.updateStatusError(r.ctx, extensionscontroller.ReconcileErrCauseOrErr(err), infrastructure, operationType, "Error restoring worker")
		return extensionscontroller.ReconcileErr(err)
	}

	// remove operation annotation 'restore'
	if err := removeAnnotation(r.ctx, r.client, infrastructure, gardencorev1beta1constants.GardenerOperation); err != nil {
		r.logger.Error(err, "Error removing annotation from Infrastructure", "annotation", fmt.Sprintf("%s/%s", gardencorev1beta1constants.GardenerOperation, gardencorev1beta1constants.GardenerOperationRestore), "infrastructure", fmt.Sprintf("%s/%s", infrastructure.Namespace, infrastructure.Name))
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) updateStatusProcessing(ctx context.Context, infrastructure *extensionsv1alpha1.Infrastructure, lastOperationType gardencorev1beta1.LastOperationType, description string) error {
	return extensionscontroller.TryUpdateStatus(ctx, retry.DefaultBackoff, r.client, infrastructure, func() error {
		infrastructure.Status.LastOperation = extensionscontroller.LastOperation(lastOperationType, gardencorev1beta1.LastOperationStateProcessing, 1, description)
		return nil
	})
}

func (r *reconciler) updateStatusError(ctx context.Context, err error, infrastructure *extensionsv1alpha1.Infrastructure, lastOperationType gardencorev1beta1.LastOperationType, description string) error {
	return extensionscontroller.TryUpdateStatus(ctx, retry.DefaultBackoff, r.client, infrastructure, func() error {
		infrastructure.Status.ObservedGeneration = infrastructure.Generation
		infrastructure.Status.LastOperation, infrastructure.Status.LastError = extensionscontroller.ReconcileError(lastOperationType, gardencorev1beta1helper.FormatLastErrDescription(fmt.Errorf("%s: %v", description, err)), 50, gardencorev1beta1helper.ExtractErrorCodes(err)...)
		return nil
	})
}

func (r *reconciler) updateStatusSuccess(ctx context.Context, infrastructure *extensionsv1alpha1.Infrastructure, lastOperationType gardencorev1beta1.LastOperationType, description string) error {
	return extensionscontroller.TryUpdateStatus(ctx, retry.DefaultBackoff, r.client, infrastructure, func() error {
		infrastructure.Status.ObservedGeneration = infrastructure.Generation
		infrastructure.Status.LastOperation, infrastructure.Status.LastError = extensionscontroller.ReconcileSucceeded(lastOperationType, description)
		return nil
	})
}

func removeAnnotation(ctx context.Context, c client.Client, infrastructure *extensionsv1alpha1.Infrastructure, annotation string) error {
	withOpAnnotation := infrastructure.DeepCopyObject()
	delete(infrastructure.Annotations, annotation)
	return c.Patch(ctx, infrastructure, client.MergeFrom(withOpAnnotation))
}

func isInfraMigrated(infrastructure *extensionsv1alpha1.Infrastructure) bool {
	return infrastructure.Status.LastOperation != nil &&
		infrastructure.Status.LastOperation.GetType() == gardencorev1beta1.LastOperationTypeMigrate &&
		infrastructure.Status.LastOperation.GetState() == gardencorev1beta1.LastOperationStateSucceeded
}
