package stub

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	ingressv1alpha1 "github.com/openshift/cluster-ingress-operator/pkg/apis/ingress/v1alpha1"
	"github.com/openshift/cluster-ingress-operator/pkg/manifests"
	"github.com/openshift/cluster-ingress-operator/pkg/util"
	"github.com/openshift/cluster-ingress-operator/pkg/util/slice"

	"github.com/operator-framework/operator-sdk/pkg/sdk"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

const (
	// installerConfigNamespace is the namespace containing the installer config.
	installerConfigNamespace = "kube-system"

	// clusterConfigResource is the resource containing the installer config.
	clusterConfigResource = "cluster-config-v1"

	// ClusterIngressFinalizer is applied to all ClusterIngress resources before
	// they are considered for processing; this ensures the operator has a chance
	// to handle all states.
	// TODO: Make this generic and not tied to the "default" ingress.
	ClusterIngressFinalizer = "ingress.openshift.io/default-cluster-ingress"
)

type Handler struct {
	InstallConfig   *util.InstallConfig
	ManifestFactory *manifests.Factory
	Namespace       string
}

func (h *Handler) Handle(ctx context.Context, event sdk.Event) error {
	// TODO: This should be adding an item to a rate limited work queue, but for
	// now correctness is more important than performance.
	switch o := event.Object.(type) {
	case *ingressv1alpha1.ClusterIngress:
		logrus.Infof("reconciling for update to clusteringress %q", o.Name)
	}
	return h.reconcile()
}

// EnsureDefaultClusterIngress ensures that a default ClusterIngress exists.
func (h *Handler) EnsureDefaultClusterIngress() error {
	ci, err := h.ManifestFactory.DefaultClusterIngress(h.InstallConfig)
	if err != nil {
		return err
	}

	err = sdk.Create(ci)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	} else if err == nil {
		logrus.Infof("created default cluster ingress %s/%s", ci.Namespace, ci.Name)
	}
	return nil
}

// Reconcile performs a full reconciliation loop for ingress, including
// generalized setup and handling of all clusteringress resources in the
// operator namespace.
func (h *Handler) reconcile() error {
	// Ensure we have all the necessary scaffolding on which to place router
	// instances.
	err := h.ensureRouterNamespace()
	if err != nil {
		return err
	}

	// Find all clusteringresses.
	ingresses := &ingressv1alpha1.ClusterIngressList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterIngress",
			APIVersion: "ingress.openshift.io/v1alpha1",
		},
	}
	err = sdk.List(h.Namespace, ingresses, sdk.WithListOptions(&metav1.ListOptions{}))
	if err != nil {
		return fmt.Errorf("failed to list clusteringresses: %v", err)
	}

	// Reconcile all the ingresses.
	errors := []error{}
	for _, ingress := range ingresses.Items {
		// Handle deleted ingress.
		// TODO: Assert/ensure that the ingress has a finalizer so we can reliably detect
		// deletion.
		if ingress.DeletionTimestamp != nil {
			// Destroy any router associated with the clusteringress.
			err := h.ensureRouterDeleted(&ingress)
			if err != nil {
				errors = append(errors, fmt.Errorf("couldn't delete clusteringress %q: %v", ingress.Name, err))
				continue
			}
			// Clean up the finalizer to allow the clusteringress to be deleted.
			if slice.ContainsString(ingress.Finalizers, ClusterIngressFinalizer) {
				ingress.Finalizers = slice.RemoveString(ingress.Finalizers, ClusterIngressFinalizer)
				err = sdk.Update(&ingress)
				if err != nil {
					errors = append(errors, fmt.Errorf("couldn't remove finalizer from clusteringress %q: %v", ingress.Name, err))
				}
			}
			continue
		}

		// Handle active ingress.
		err := h.ensureRouterForIngress(&ingress)
		if err != nil {
			errors = append(errors, fmt.Errorf("couldn't ensure clusteringress %q: %v", ingress.Name, err))
		}
	}
	return utilerrors.NewAggregate(errors)
}

// ensureRouterNamespace ensures all the necessary scaffolding exists for
// routers generally, including a namespace and all RBAC setup.
func (h *Handler) ensureRouterNamespace() error {
	cr, err := h.ManifestFactory.RouterClusterRole()
	if err != nil {
		return fmt.Errorf("couldn't build router cluster role: %v", err)
	}
	err = sdk.Create(cr)
	if err == nil {
		logrus.Infof("created router cluster role %q", cr.Name)
	} else if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("couldn't create router cluster role: %v", err)
	}

	ns, err := h.ManifestFactory.RouterNamespace()
	if err != nil {
		return fmt.Errorf("couldn't build router namespace: %v", err)
	}
	err = sdk.Create(ns)
	if err == nil {
		logrus.Infof("created router namespace %q", ns.Name)
	} else if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("couldn't create router namespace %q: %v", ns.Name, err)
	}

	sa, err := h.ManifestFactory.RouterServiceAccount()
	if err != nil {
		return fmt.Errorf("couldn't build router service account: %v", err)
	}
	err = sdk.Create(sa)
	if err == nil {
		logrus.Infof("created router service account %s/%s", sa.Namespace, sa.Name)
	} else if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("couldn't create router service account %s/%s: %v", sa.Namespace, sa.Name, err)
	}

	crb, err := h.ManifestFactory.RouterClusterRoleBinding()
	if err != nil {
		return fmt.Errorf("couldn't build router cluster role binding: %v", err)
	}
	err = sdk.Create(crb)
	if err == nil {
		logrus.Infof("created router cluster role binding %q", crb.Name)
	} else if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("couldn't create router cluster role binding: %v", err)
	}

	return nil
}

// ensureRouterForIngress ensures all necessary router resources exist for a
// given clusteringress.
func (h *Handler) ensureRouterForIngress(ci *ingressv1alpha1.ClusterIngress) error {
	ds, err := h.ManifestFactory.RouterDaemonSet(ci)
	if err != nil {
		return fmt.Errorf("couldn't build daemonset: %v", err)
	}
	err = sdk.Create(ds)
	if errors.IsAlreadyExists(err) {
		if err = sdk.Get(ds); err != nil {
			return fmt.Errorf("couldn't get daemonset %s, %v", ds.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to create daemonset %s/%s: %v", ds.Namespace, ds.Name, err)
	} else {
		logrus.Infof("created router daemonset %s/%s", ds.Namespace, ds.Name)
	}

	if ci.Spec.HighAvailability != nil {
		switch ci.Spec.HighAvailability.Type {
		case ingressv1alpha1.CloudClusterIngressHA:
			service, err := h.ManifestFactory.RouterServiceCloud(ci)
			if err != nil {
				return fmt.Errorf("couldn't build service: %v", err)
			}
			trueVar := true
			dsRef := metav1.OwnerReference{
				APIVersion: ds.APIVersion,
				Kind:       ds.Kind,
				Name:       ds.Name,
				UID:        ds.UID,
				Controller: &trueVar,
			}
			service.SetOwnerReferences([]metav1.OwnerReference{dsRef})

			err = sdk.Create(service)
			if err == nil {
				logrus.Infof("created router service %s/%s", service.Namespace, service.Name)
			} else if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create service %s/%s: %v", service.Namespace, service.Name, err)
			}
		}
	}

	return nil
}

// ensureRouterDeleted ensures that any router resources associated with the
// clusteringress are deleted.
func (h *Handler) ensureRouterDeleted(ci *ingressv1alpha1.ClusterIngress) error {
	ds, err := h.ManifestFactory.RouterDaemonSet(ci)
	if err != nil {
		return fmt.Errorf("couldn't build DaemonSet object for deletion: %v", err)
	}
	err = sdk.Delete(ds)
	if !errors.IsNotFound(err) {
		return err
	}
	return nil
}
