package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AdministrativeNetworkDomain defines an isolated network boundary that groups one or more
// subnets into a logically isolated network with its own address space and
// automatic intra-domain routing. Plexus translates this intent into
// backend-specific resources (UDNs, CNCs, RouteAdvertisements, etc. for the
// OVN-Kubernetes backend).
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=administrativenetworkdomains,scope=Cluster,shortName=and,singular=administrativenetworkdomain
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Whether all subnets are reconciled"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(self.spec.subnets) || self.spec.subnets.all(s, size(self.metadata.name) + size(s.name) + 1 <= 63)",message="combined <and-name>-<subnet-name> must not exceed 63 characters (Kubernetes namespace name limit)"
type AdministrativeNetworkDomain struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of the AdministrativeNetworkDomain.
	// +optional
	Spec *AdministrativeNetworkDomainSpec `json:"spec,omitempty"`

	// status defines the observed state of the AdministrativeNetworkDomain.
	// +optional
	Status *AdministrativeNetworkDomainStatus `json:"status,omitempty"`
}

// AdministrativeNetworkDomainSpec defines the desired state of an AdministrativeNetworkDomain.
type AdministrativeNetworkDomainSpec struct {
	// subnets defines the subnets within this AdministrativeNetworkDomain. The Plexus
	// controller creates backend-specific network resources for each entry.
	// For the OVN-Kubernetes backend, each subnet becomes a namespace +
	// ClusterUserDefinedNetwork. Subnets within the same AdministrativeNetworkDomain can
	// have different topologies and transports.
	//
	// An AND with no subnets is valid; this supports workflows where the
	// domain is created by one team (e.g. platform) and subnets are added
	// later by another (e.g. networking). The controller will not provision
	// any network resources until at least one subnet is added.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=name
	Subnets []Subnet `json:"subnets,omitempty"`
}

// SubnetType defines the external connectivity class of an AdministrativeNetworkDomain subnet.
// +kubebuilder:validation:Enum=Public;Private;Isolated;VPNOnly
type SubnetType string

const (
	SubnetTypePublic   SubnetType = "Public"
	SubnetTypePrivate  SubnetType = "Private"
	SubnetTypeIsolated SubnetType = "Isolated"
	SubnetTypeVPNOnly  SubnetType = "VPNOnly"
)

// Subnets returns the configured subnets, if any.
func (in *AdministrativeNetworkDomain) Subnets() []Subnet {
	if in == nil || in.Spec == nil {
		return nil
	}
	return in.Spec.Subnets
}

// TypeValue returns the subnet type, defaulting to Private when omitted.
func (in *Subnet) TypeValue() SubnetType {
	if in == nil || in.Type == nil {
		return SubnetTypePrivate
	}
	return *in.Type
}

// CIDR represents an IP network in CIDR notation (e.g. "10.0.0.0/16" or "fd00::/64").
// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="must be a valid network address in CIDR notation"
// +kubebuilder:validation:MaxLength=43
type CIDR string

// Subnet defines a subnet within the AdministrativeNetworkDomain. The Plexus controller
// creates backend-specific resources from this definition. For the
// OVN-Kubernetes backend, each subnet maps to a namespace + UDN (L2 EVPN).
// Each subnet maps 1:1 to a single namespace.
type Subnet struct {
	// name is the subnet name. For the OVN-Kubernetes backend, the resulting
	// namespace and UDN are named "<networkdomain>-<subnet>".
	//
	// The combined "<networkdomain>-<subnet>" must respect Kubernetes
	// namespace name limits (63 characters, DNS label).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable once set"
	// +required
	Name string `json:"name,omitempty"`

	// cidrs defines the IP address range(s) for this subnet.
	// At most two CIDRs may be specified: one IPv4 and one IPv6
	// for dual-stack. If a single CIDR is provided, the subnet is
	// single-stack (v4 or v6).
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=atomic
	// +required
	//
	// +kubebuilder:validation:XValidation:rule="self.size() <= 1 || (self.exists(c, c.contains(':')) && self.exists(c, !c.contains(':')))",message="when two CIDRs are specified they must be from different address families (one IPv4, one IPv6)"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cidrs is immutable once set"
	CIDRs []CIDR `json:"cidrs,omitempty"`

	// type defines the subnet type which determines its external
	// connectivity and the resources the Plexus controller provisions:
	//
	// - Public: externally reachable. OVN-Kubernetes backend creates
	//   RouteAdvertisements (BGP) to export subnet routes.
	// - Private: no direct external reachability. Outbound traffic is
	//   SNATed using node IPs.
	// - Isolated: no routes outside the AdministrativeNetworkDomain. No
	//   RouteAdvertisements, no SNAT, no intra-domain routing. Traffic
	//   is confined to the subnet.
	// - VPNOnly: traffic exits the AdministrativeNetworkDomain exclusively through a
	//   VPN connection.
	//
	// Defaults to Private if not specified.
	//
	// +optional
	// +default="Private"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable once set"
	Type *SubnetType `json:"type,omitempty"`

	// availabilityZone optionally pins this subnet to a specific
	// failure domain. It selects which clusters and which nodes
	// within those clusters the subnet is placed on.
	//
	// If omitted, the subnet spans all clusters and all nodes
	// (no AZ pinning).
	//
	// +optional
	AvailabilityZone *AvailabilityZone `json:"availabilityZone,omitempty"`
}

// AvailabilityZone selects the failure domain for an AdministrativeNetworkDomain subnet.
type AvailabilityZone struct {
	// clusterSelector selects which clusters this subnet is placed on
	// in multi-cluster deployments. The hub Plexus controller matches
	// these labels against its cluster inventory. Only clusters whose
	// labels satisfy the selector receive the resources for this subnet.
	//
	// +required
	ClusterSelector *metav1.LabelSelector `json:"clusterSelector,omitempty"`

	// nodeSelector optionally restricts the subnet to nodes matching
	// these labels within each target cluster. The Plexus controller
	// translates this into a namespace node selector so that all pods
	// in the subnet schedule only on matching nodes.
	//
	// Typical use: AZ pinning via
	// { "topology.kubernetes.io/zone": "rack-a" }.
	//
	// If empty or omitted, the subnet spans all nodes in the cluster.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// AdministrativeNetworkDomainStatus defines the observed state of an AdministrativeNetworkDomain.
type AdministrativeNetworkDomainStatus struct {
	// conditions reports the status of AdministrativeNetworkDomain operations.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// AdministrativeNetworkDomainList contains a list of AdministrativeNetworkDomain resources.
//
// +kubebuilder:object:root=true
type AdministrativeNetworkDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdministrativeNetworkDomain `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &AdministrativeNetworkDomain{}, &AdministrativeNetworkDomainList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
