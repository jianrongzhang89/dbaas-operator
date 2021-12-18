/*
Copyright 2021.

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

	appv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/RHEcosystemAppEng/dbaas-operator/api/v1alpha1"
)

// DBaaSInstanceReconciler reconciles a DBaaSInstance object
type DBaaSInstanceReconciler struct {
	*DBaaSReconciler
}

//+kubebuilder:rbac:groups=dbaas.redhat.com,resources=*,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=dbaas.redhat.com,resources=*/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=dbaas.redhat.com,resources=*/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *DBaaSInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, recErr error) {
	logger := ctrl.LoggerFrom(ctx, "DBaaS Connection", req.NamespacedName)

	var instance v1alpha1.DBaaSInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted since request queued, child objects getting GC'd, no requeue
			logger.Info("DBaaS Connection resource not found, has been deleted")
			result, recErr = ctrl.Result{}, err
			return
		}
		logger.Error(err, "Error fetching DBaaS Connection for reconcile")
		result, recErr = ctrl.Result{}, err
		return
	}

	var dbaasCond metav1.Condition
	// This update will make sure the status is always updated in case of any errors or successful result
	defer func(inst *v1alpha1.DBaaSInstance, cond *metav1.Condition) {
		apimeta.SetStatusCondition(&inst.Status.Conditions, *cond)
		if err := r.Client.Status().Update(ctx, inst); err != nil {
			if errors.IsConflict(err) {
				logger.V(1).Info("Connection modified, retry syncing spec")
				// Re-queue and preserve existing recErr
				result = ctrl.Result{Requeue: true}
				return
			}
			logger.Error(err, "Could not update connection status")
			if recErr == nil {
				// There is no existing recErr. Set it to the status update error
				recErr = err
			}
		}
	}(&instance, &dbaasCond)

	var inventory v1alpha1.DBaaSInventory
	if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Spec.InventoryRef.Namespace, Name: instance.Spec.InventoryRef.Name}, &inventory); err != nil {
		logger.Error(err, "Error fetching DBaaS Inventory resource reference for DBaaS Connection", "DBaaS Inventory", instance.Spec.InventoryRef)
		result, recErr = ctrl.Result{}, err
		return
	}

	provider, err := r.getDBaaSProvider(inventory.Spec.ProviderRef.Name, ctx)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "Requested DBaaS Provider is not configured in this environment", "DBaaS Provider", inventory.Spec.ProviderRef)
			result, recErr = ctrl.Result{}, err
			return
		}
		logger.Error(err, "Error reading configured DBaaS Provider", "DBaaS Provider", inventory.Spec.ProviderRef)
		result, recErr = ctrl.Result{}, err
		return
	}
	logger.Info("Found DBaaS Provider", "DBaaS Provider", inventory.Spec.ProviderRef)

	providerInstance := r.createProviderObject(&instance, provider.Spec.InstanceKind)
	if res, err := r.reconcileProviderObject(providerInstance, r.providerObjectMutateFn(&instance, providerInstance, instance.Spec.DeepCopy()), ctx); err != nil {
		if errors.IsConflict(err) {
			logger.Info("Provider Instance modified, retry syncing spec")
			result, recErr = ctrl.Result{Requeue: true}, nil
			return
		}
		logger.Error(err, "Error reconciling Provider Instance resource")
		result, recErr = ctrl.Result{}, err
		return
	} else {
		logger.Info("Provider Instance resource reconciled", "result", res)
	}

	var DBaaSProviderInstance v1alpha1.DBaaSProviderInstance
	if err := r.parseProviderObject(providerInstance, &DBaaSProviderInstance); err != nil {
		logger.Error(err, "Error parsing the Provider Connection resource")
		result, recErr = ctrl.Result{}, nil
		return
	}
	dbaasCond = *mergeInstanceStatus(&instance, &DBaaSProviderInstance)
	result, recErr = ctrl.Result{}, nil
	return
}

// SetupWithManager sets up the controller with the Manager.
func (r *DBaaSInstanceReconciler) SetupWithManager(mgr ctrl.Manager) (controller.Controller, error) {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DBaaSInstance{}).
		Build(r)
}

func (r *DBaaSInstanceReconciler) deploymentMutateFn(connection *v1alpha1.DBaaSInstance, deployment *appv1.Deployment) controllerutil.MutateFn {
	return func() error {
		deployment.ObjectMeta.Labels = map[string]string{
			"managed-by":      "dbaas-operator",
			"owner":           connection.Name,
			"owner.kind":      connection.Kind,
			"owner.namespace": connection.Namespace,
		}
		deployment.Spec = appv1.DeploymentSpec{
			Replicas: pointer.Int32Ptr(0),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": "bind-deploy",
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": "bind-deploy",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:            "bind-deploy",
							Image:           "quay.io/ecosystem-appeng/busybox",
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         []string{"sh", "-c", "echo The app is running! && sleep 3600"},
						},
					},
				},
			},
		}
		deployment.OwnerReferences = nil
		if err := ctrl.SetControllerReference(connection, deployment, r.Scheme); err != nil {
			return err
		}
		return nil
	}
}

// mergeInstanceStatus: merge the status from DBaaSProviderConnection into the current DBaaSInstance status
func mergeInstanceStatus(inst *v1alpha1.DBaaSInstance, providerInst *v1alpha1.DBaaSProviderInstance) *metav1.Condition {
	cond := apimeta.FindStatusCondition(inst.Status.Conditions, v1alpha1.DBaaSInstanceReadyType)
	providerInst.Status.DeepCopyInto(&inst.Status)
	if cond != nil {
		inst.Status.Conditions = append(inst.Status.Conditions, *cond)
	}
	// Update connection status condition (type: DBaaSInstanceReadyType) based on the provider status
	specSync := apimeta.FindStatusCondition(providerInst.Status.Conditions, v1alpha1.DBaaSInstanceProviderSyncType)
	if cond != nil && specSync != nil && specSync.Status == metav1.ConditionTrue {
		return &metav1.Condition{Type: v1alpha1.DBaaSInstanceReadyType, Status: metav1.ConditionTrue, Reason: v1alpha1.Ready, Message: v1alpha1.MsgProviderCRStatusSyncDone}
	}
	return &metav1.Condition{Type: v1alpha1.DBaaSInstanceReadyType, Status: metav1.ConditionFalse, Reason: v1alpha1.ProviderReconcileInprogress, Message: v1alpha1.MsgProviderCRReconcileInProgress}
}
