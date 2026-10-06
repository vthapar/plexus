package ovnkubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

const (
	labelNetworkDomain = "plexus.io/network-domain"
	labelSubnet        = "plexus.io/subnet"
	labelSubnetType    = "plexus.io/subnet-type"

	labelPrimaryUDN = "k8s.ovn.org/primary-user-defined-network"

	annotationNodeSelector = "scheduler.alpha.kubernetes.io/node-selector"
)

// namespaceName returns the deterministic namespace name for a subnet:
// "<and-name>-<subnet-name>".
func namespaceName(and *v1beta1.AdministrativeNetworkDomain, subnet *v1beta1.Subnet) string {
	return fmt.Sprintf("%s-%s", and.Name, subnet.Name)
}

// desiredLabels returns the labels that should be set on the namespace for a subnet.
func desiredLabels(and *v1beta1.AdministrativeNetworkDomain, subnet *v1beta1.Subnet) map[string]string {
	return map[string]string{
		labelNetworkDomain: and.Name,
		labelSubnet:        subnet.Name,
		labelSubnetType:    string(subnet.TypeValue()),
		labelPrimaryUDN:    "",
	}
}

// nodeSelectorAnnotationValue builds a deterministic, sorted "key=value" annotation
// from the AZ NodeSelector map. Returns empty string if no selector is set.
func nodeSelectorAnnotationValue(subnet *v1beta1.Subnet) string {
	if subnet.AvailabilityZone == nil || len(subnet.AvailabilityZone.NodeSelector) == 0 {
		return ""
	}

	keys := make([]string, 0, len(subnet.AvailabilityZone.NodeSelector))
	for k := range subnet.AvailabilityZone.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + subnet.AvailabilityZone.NodeSelector[k]
	}
	return strings.Join(pairs, ",")
}

// desiredAnnotations returns the annotations that should be set on the namespace.
func desiredAnnotations(subnet *v1beta1.Subnet) map[string]string {
	annotations := map[string]string{}
	if v := nodeSelectorAnnotationValue(subnet); v != "" {
		annotations[annotationNodeSelector] = v
	}
	return annotations
}

// reconcileSubnet ensures the namespace for the given subnet exists with
// the correct labels and annotations, then reconciles the CUDN.
// The cl parameter specifies which cluster to create resources on.
func (b *OVNKubernetesBackend) reconcileSubnet(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, subnet *v1beta1.Subnet, cl client.Client) error {
	nsName := namespaceName(and, subnet)
	labels := desiredLabels(and, subnet)
	annotations := desiredAnnotations(subnet)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nsName,
			Labels:      labels,
			Annotations: annotations,
		},
	}

	existing := &corev1.Namespace{}
	err := cl.Get(ctx, client.ObjectKeyFromObject(ns), existing)
	if apierrors.IsNotFound(err) {
		b.log.Info("creating namespace for subnet", "namespace", nsName, "subnet", subnet.Name)
		if err := cl.Create(ctx, ns); err != nil {
			return fmt.Errorf("creating namespace %q: %w", nsName, err)
		}
		return b.reconcileCUDN(ctx, and, subnet, cl)
	}
	if err != nil {
		return fmt.Errorf("getting namespace %q: %w", nsName, err)
	}

	if err := b.reconcileNamespaceMetadata(ctx, existing, labels, annotations, cl); err != nil {
		return err
	}

	return b.reconcileCUDN(ctx, and, subnet, cl)
}

// reconcileNamespaceMetadata updates labels and annotations on an existing
// namespace if they have drifted from the desired state.
func (b *OVNKubernetesBackend) reconcileNamespaceMetadata(
	ctx context.Context,
	ns *corev1.Namespace,
	desiredLabels map[string]string,
	desiredAnnotations map[string]string,
	cl client.Client,
) error {
	needsUpdate := false

	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	for k, v := range desiredLabels {
		if ns.Labels[k] != v {
			ns.Labels[k] = v
			needsUpdate = true
		}
	}

	if ns.Annotations == nil {
		ns.Annotations = make(map[string]string)
	}
	desiredValue, wantAnnotation := desiredAnnotations[annotationNodeSelector]
	existingValue, hasAnnotation := ns.Annotations[annotationNodeSelector]
	if wantAnnotation && existingValue != desiredValue {
		ns.Annotations[annotationNodeSelector] = desiredValue
		needsUpdate = true
	} else if !wantAnnotation && hasAnnotation {
		delete(ns.Annotations, annotationNodeSelector)
		needsUpdate = true
	}

	if needsUpdate {
		b.log.Info("updating namespace metadata", "namespace", ns.Name)
		if err := cl.Update(ctx, ns); err != nil {
			return fmt.Errorf("updating namespace %q metadata: %w", ns.Name, err)
		}
	}

	return nil
}

// deleteSubnet removes the CUDN (cluster-scoped, not cascade-deleted) and
// then the namespace for the given subnet.
func (b *OVNKubernetesBackend) deleteSubnet(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, subnet *v1beta1.Subnet, cl client.Client) error {
	if err := b.deleteCUDN(ctx, and, subnet, cl); err != nil {
		return err
	}

	nsName := namespaceName(and, subnet)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}

	err := cl.Delete(ctx, ns)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting namespace %q: %w", nsName, err)
	}
	return nil
}
