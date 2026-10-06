package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PlexusControllerConfig is a singleton resource that configures the Plexus
// operator. It specifies which backend to use and provides backend-specific
// settings such as VTEP ranges, FRR selectors, and IPAM configuration.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=plexuscontrollerconfigs,scope=Cluster,singular=plexuscontrollerconfig
// +kubebuilder:object:root=true
type PlexusControllerConfig struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired configuration for the Plexus operator.
	// +optional
	Spec *PlexusControllerConfigSpec `json:"spec,omitempty"`
}

// PlexusControllerConfigSpec defines the desired configuration for the Plexus operator.
//
// +kubebuilder:validation:XValidation:rule="self.backend != 'ovn-kubernetes' || has(self.ovnKubernetes)",message="ovnKubernetes configuration is required when backend is 'ovn-kubernetes'"
type PlexusControllerConfigSpec struct {
	// backend selects which network backend the operator uses to
	// translate AdministrativeNetworkDomains into platform resources.
	//
	// +kubebuilder:validation:Enum="ovn-kubernetes"
	// +default="ovn-kubernetes"
	// +optional
	Backend *string `json:"backend,omitempty"`

	// ovnKubernetes holds configuration specific to the OVN-Kubernetes
	// backend. Required when backend is "ovn-kubernetes".
	//
	// +optional
	OVNKubernetes *OVNKubernetesConfig `json:"ovnKubernetes,omitempty"`
}

// OVNKubernetesConfig holds settings for the OVN-Kubernetes backend.
type OVNKubernetesConfig struct {
	// vtepCIDRs defines the IP ranges used for VTEP (VXLAN Tunnel
	// Endpoint) discovery in EVPN mode. The Plexus operator creates
	// a shared unmanaged VTEP resource with these CIDRs. In
	// multi-cluster deployments, this range must cover the VTEP IPs
	// of all participating clusters so that cross-cluster EVPN
	// tunnels can be established.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	// +required
	VTEPCIDRs []CIDR `json:"vtepCIDRs,omitempty"`

	// frrConfigurationSelector selects the base FRRConfiguration
	// resource that RouteAdvertisements reference for BGP peering.
	// This must match an existing FRRConfiguration on the cluster.
	// In multi-cluster deployments, both the hub and each spoke cluster
	// must have an FRRConfiguration matching this selector for BGP
	// advertisements to be established.
	// The Plexus controller reads the ASN from this FRRConfiguration's
	// spec.bgp.routers[].asn for use in route-leaking FRR config.
	//
	// +required
	FRRConfigurationSelector *metav1.LabelSelector `json:"frrConfigurationSelector,omitempty"`
}

// PlexusControllerConfigList contains a list of PlexusControllerConfig resources.
//
// +kubebuilder:object:root=true
type PlexusControllerConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlexusControllerConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &PlexusControllerConfig{}, &PlexusControllerConfigList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}

// CIDR represents an IP network in CIDR notation (e.g. "10.100.0.0/16" or "fd00:100::/64").
// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="must be a valid network address in CIDR notation"
// +kubebuilder:validation:MaxLength=43
type CIDR string
