/*
Copyright 2023.

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

	netwrkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	"github.com/kannon-email/k8nnon/api/v1alpha1"
	corev1alpha1 "github.com/kannon-email/k8nnon/api/v1alpha1"
	"github.com/kannon-email/k8nnon/internal/dns/checker"
)

// DomainReconciler reconciles a Domain object
type DomainReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	DNSChecker checker.DNSChecker
}

//+kubebuilder:rbac:groups=core.k8s.kannon.email,resources=domains,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core.k8s.kannon.email,resources=domains/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core.k8s.kannon.email,resources=domains/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Domain object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *DomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	l.Info("reconciling domain", "domain", req.NamespacedName)

	domain := &corev1alpha1.Domain{}
	if err := r.Get(ctx, req.NamespacedName, domain); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	dnsStatus, err := r.checkDomainDNS(ctx, l, domain)
	if err != nil {
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	domain.Status = corev1alpha1.DomainStatus{
		DNS: dnsStatus,
	}

	if err := r.reconcileIngress(ctx, domain, l); err != nil {
		l.Error(err, "failed to reconcile ingress", "domain", domain)
		return ctrl.Result{}, err
	}

	if err := r.Status().Update(ctx, domain); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{
		RequeueAfter: computeReconcileInterval(domain),
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Domain{}).
		Owns(&netwrkingv1.Ingress{}).
		Complete(r)
}

func (r *DomainReconciler) reconcileIngress(ctx context.Context, domain *v1alpha1.Domain, l logr.Logger) error {
	ingress := &netwrkingv1.Ingress{}
	name := statsIngressName(domain)

	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: domain.Namespace}, ingress)
	if err == nil {
		return r.handleFoundIngress(ctx, ingress, domain, l)
	} else if !errors.IsNotFound(err) {
		return err
	}

	if !domain.Status.DNS.Stats {
		return nil
	}

	ingress, err = r.buildDesiredIngress(domain)
	if err != nil {
		return err
	}

	return r.Create(ctx, ingress)
}

func (r *DomainReconciler) handleFoundIngress(ctx context.Context, ingress *netwrkingv1.Ingress, domain *v1alpha1.Domain, l logr.Logger) error {
	if domain.Status.DNS.Stats {
		return nil
	}

	if ingress.DeletionTimestamp == nil {
		return r.Delete(ctx, ingress)
	}

	return nil
}

func (r *DomainReconciler) checkDomainDNS(ctx context.Context, l logr.Logger, domain *corev1alpha1.Domain) (corev1alpha1.DNSStatus, error) {
	l.Info("checking domain dns", "domain", domain.Spec.BaseDomain)
	dnsStatus := corev1alpha1.DNSStatus{}

	var err error

	if dnsStatus.DKIN, err = r.DNSChecker.CheckDomainDKim(ctx, domain); err != nil {
		l.Error(err, "failed to check dkim", "domain", domain)
		return corev1alpha1.DNSStatus{}, err
	}

	if dnsStatus.Stats, err = r.DNSChecker.CheckDomainStatsDNS(ctx, domain); err != nil {
		l.Error(err, "failed to check dns stats", "domain", domain)
		return corev1alpha1.DNSStatus{}, err
	}

	if dnsStatus.SFP, err = r.DNSChecker.CheckDomainSPF(ctx, domain); err != nil {
		l.Error(err, "failed to check dns spf", "domain", domain)
		return corev1alpha1.DNSStatus{}, err
	}

	l.Info("domain dns checked", "domain", dnsStatus)

	return dnsStatus, nil
}

func (r *DomainReconciler) buildDesiredIngress(domain *corev1alpha1.Domain) (*netwrkingv1.Ingress, error) {
	pathPrefix := netwrkingv1.PathTypePrefix
	name := statsIngressName(domain)

	ing := &netwrkingv1.Ingress{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: domain.Namespace,
		},
		Spec: netwrkingv1.IngressSpec{
			Rules: []netwrkingv1.IngressRule{
				{
					Host: domain.Spec.BaseDomain,
					IngressRuleValue: netwrkingv1.IngressRuleValue{
						HTTP: &netwrkingv1.HTTPIngressRuleValue{
							Paths: []netwrkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathPrefix,
									Backend: netwrkingv1.IngressBackend{
										Service: &netwrkingv1.IngressServiceBackend{
											Name: "k8nnon",
											Port: netwrkingv1.ServiceBackendPort{
												Number: 80,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(domain, ing, r.Scheme); err != nil {
		return ing, err
	}

	return ing, nil
}

func statsIngressName(domain *corev1alpha1.Domain) string {
	return fmt.Sprintf("%s-stats", domain.Name)
}

func computeReconcileInterval(domain *corev1alpha1.Domain) time.Duration {
	if domain.Status.DNS.Stats && domain.Status.DNS.SFP && domain.Status.DNS.DKIN {
		return 1 * time.Hour
	}

	return 1 * time.Minute
}
