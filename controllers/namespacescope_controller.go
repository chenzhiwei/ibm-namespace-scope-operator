//
// Copyright 2020 IBM Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package controllers

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "github.com/IBM/ibm-namespace-scope-operator/api/v1"
	util "github.com/IBM/ibm-namespace-scope-operator/controllers/common"
)

var ctx context.Context

// var validatedNs []string

// NamespaceScopeReconciler reconciles a NamespaceScope object
type NamespaceScopeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=operator.ibm.com,resources=namespacescopes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.ibm.com,resources=namespacescopes/status,verbs=get;update;patch

func (r *NamespaceScopeReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx = context.Background()

	// Fetch the NamespaceScope instance
	instance := &operatorv1.NamespaceScope{}

	if err := r.Client.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	klog.Infof("Reconciling NamespaceScope: %s", req.NamespacedName)
	// // Get all validated namespace list
	// if err := r.GetValidatedNamespaces(instance); err != nil {
	// 	return ctrl.Result{}, err
	// }
	// if len(validatedNs) == 0 {
	// 	klog.Infof("Finished reconciling NamespaceScope: %s", req.NamespacedName)
	// 	return ctrl.Result{}, nil
	// }

	if err := r.InitConfigMap(instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.PushRbacToNamespace(instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.DeleteRbacFromUnmanagedNamespace(instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.UpdateConfigMap(instance); err != nil {
		return ctrl.Result{}, err
	}

	klog.Infof("Finished reconciling NamespaceScope: %s", req.NamespacedName)
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

func (r *NamespaceScopeReconciler) InitConfigMap(instance *operatorv1.NamespaceScope) error {
	cm := &corev1.ConfigMap{}
	cmName := "namespace-scope"
	cmNamespace := instance.Namespace

	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: cmNamespace}, cm); err != nil {
		// If ConfigMap does not exist, create it
		if errors.IsNotFound(err) {
			cm.Name = cmName
			cm.Namespace = cmNamespace
			cm.Data = make(map[string]string)
			cm.Data["namespaces"] = instance.Namespace

			if err := r.Create(ctx, cm); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	return nil
}

func (r *NamespaceScopeReconciler) UpdateConfigMap(instance *operatorv1.NamespaceScope) error {
	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{Name: "namespace-scope", Namespace: instance.Namespace}
	if err := r.Get(ctx, cmKey, cm); err != nil {
		return err
	}

	// If NamespaceMembers changed, update ConfigMap
	if strings.Join(instance.Spec.NamespaceMembers, ",") != cm.Data["namespaces"] {
		cm.Data["namespaces"] = strings.Join(instance.Spec.NamespaceMembers, ",")
		if err := r.Update(ctx, cm); err != nil {
			return err
		}
	}
	return nil
}

func (r *NamespaceScopeReconciler) DeleteRbacFromUnmanagedNamespace(instance *operatorv1.NamespaceScope) error {
	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{Name: "namespace-scope", Namespace: instance.Namespace}
	if err := r.Get(ctx, cmKey, cm); err != nil {
		return err
	}

	var nsInCm []string
	if cm.Data["namespaces"] != "" {
		nsInCm = strings.Split(cm.Data["namespaces"], ",")
	}
	nsInCr := instance.Spec.NamespaceMembers
	unmanagedNss := util.GetListDifference(nsInCm, nsInCr)
	for _, toNs := range unmanagedNss {
		if err := r.DeleteRoleBinding(instance.Namespace, toNs); err != nil {
			return err
		}
		if err := r.DeleteRole(instance.Namespace, toNs); err != nil {
			return err
		}
	}

	return nil
}

func (r *NamespaceScopeReconciler) PushRbacToNamespace(instance *operatorv1.NamespaceScope) error {
	fromNs := instance.Namespace
	saNames, err := r.GetServiceAccountFromNamespace(fromNs)
	if err != nil {
		return err
	}

	for _, toNs := range instance.Spec.NamespaceMembers {
		restart := false
		if err := r.CreateRole(fromNs, toNs); err == nil {
			restart = true
		} else if !errors.IsAlreadyExists(err) {
			return err
		}
		if err := r.CreateUpdateRoleBinding(saNames, fromNs, toNs); err == nil {
			restart = true
		} else if !errors.IsAlreadyExists(err) {
			return err
		}
		if restart {
			klog.Infof("Restarting pods in namespace %s with matching labels: %v", toNs, instance.Spec.RestartLabels)
			// if err := r.RestartPods(instance.Spec.RestartLabels, toNs); err != nil {
			// 	return err
			// }
		}
	}

	return nil
}

func (r *NamespaceScopeReconciler) GetServiceAccountFromNamespace(namespace string) ([]string, error) {
	sas := &corev1.ServiceAccountList{}
	opts := []client.ListOption{
		client.InNamespace(namespace),
	}
	if err := r.List(ctx, sas, opts...); err != nil {
		return nil, err
	}
	var saNames []string
	for _, sa := range sas.Items {
		if sa.Name == "default" || sa.Name == "deployer" || sa.Name == "builder" || sa.Name == "ibm-common-service-operator-leader-election-role" || sa.Name == "leader-election-role" {
			continue
		}
		saNames = append(saNames, sa.Name)
	}
	return saNames, nil
}

func (r *NamespaceScopeReconciler) CreateRole(fromNs, toNs string) error {
	name := "ibm-namespace-scope-operator-managed-role"
	namespace := toNs
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"projectedfrom": fromNs,
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"*"},
				APIGroups: []string{"*"},
				Resources: []string{"*"},
			},
		},
	}
	err := r.Create(ctx, role)
	if err == nil {
		klog.Infof("Create role %s in namespace %s", name, namespace)
	}
	return err
}

func (r *NamespaceScopeReconciler) DeleteRole(fromNs, toNs string) error {
	opts := []client.DeleteAllOfOption{
		client.MatchingLabels(map[string]string{"projectedfrom": fromNs}),
		client.InNamespace(toNs),
	}
	err := r.DeleteAllOf(ctx, &rbacv1.Role{}, opts...)
	if err == nil {
		klog.Infof("Delete role with label %s from namespace %s", "projectedfrom: "+fromNs, toNs)
	}
	return err
}

func (r *NamespaceScopeReconciler) CreateUpdateRoleBinding(saNames []string, fromNs, toNs string) error {
	name := "ibm-namespace-scope-operator-managed-rolebinding"
	namespace := toNs
	subjects := []rbacv1.Subject{}
	for _, saName := range saNames {
		subject := rbacv1.Subject{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: fromNs,
		}
		subjects = append(subjects, subject)
	}
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"projectedfrom": fromNs,
			},
		},
		Subjects: subjects,
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     "ibm-namespace-scope-operator-managed-role",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}

	err := r.Create(ctx, roleBinding)
	if err == nil {
		klog.Infof("Create rolebinding %s in namespace %s", name, namespace)
	} else {
		if errors.IsAlreadyExists(err) {
			if err := r.Update(ctx, roleBinding); err != nil {
				return err
			}
			klog.Infof("Update rolebinding %s in namespace %s", name, namespace)
		}
	}
	return err
}

func (r *NamespaceScopeReconciler) DeleteRoleBinding(fromNs, toNs string) error {
	opts := []client.DeleteAllOfOption{
		client.MatchingLabels(map[string]string{"projectedfrom": fromNs}),
		client.InNamespace(toNs),
	}
	err := r.DeleteAllOf(ctx, &rbacv1.RoleBinding{}, opts...)
	if err == nil {
		klog.Infof("Delete rolebinding with label %s from namespace %s", "projectedfrom: "+fromNs, toNs)
	}
	return err
}

// func (r *NamespaceScopeReconciler) GetValidatedNamespaces(instance *operatorv1.NamespaceScope) error {
// 	validatedNs = []string{}
// 	for _, nsMem := range instance.Spec.NamespaceMembers {
// 		ns := &corev1.Namespace{}
// 		key := types.NamespacedName{Name: nsMem}
// 		if err := r.Get(ctx, key, ns); err != nil {
// 			if errors.IsNotFound(err) {
// 				klog.Infof("Namespace %s does not exist and will be ignored", nsMem)
// 				continue
// 			} else {
// 				return err
// 			}
// 		}
// 		validatedNs = append(validatedNs, nsMem)
// 	}
// 	klog.Infof("Validate namespaces: %s", validatedNs)
// 	return nil
// }

// Restart pods in specific namespace with the matching labels
func (r *NamespaceScopeReconciler) RestartPods(labels map[string]string, namespace string) error {
	klog.Infof("Restarting pods in namespace %s with matching labels: %v", namespace, labels)
	opts := []client.DeleteAllOfOption{
		client.MatchingLabels(labels),
		client.InNamespace(namespace),
	}
	if err := r.DeleteAllOf(ctx, &corev1.Pod{}, opts...); err != nil {
		return err
	}
	return nil
}

func (r *NamespaceScopeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&operatorv1.NamespaceScope{}).
		Complete(r)
}
