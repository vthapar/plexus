package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

func newAddSubnetCommand() *cobra.Command {
	var cidrs []string
	var subnetType string
	var clusterSelector []string
	var nodeSelector []string

	cmd := &cobra.Command{
		Use:   "add-subnet <network-domain> <subnet-name>",
		Short: "Add a subnet to an AdministrativeNetworkDomain",
		Long: `Add a new subnet to an existing AdministrativeNetworkDomain.

Examples:
  plexus add-subnet production web --cidr 10.0.1.0/24 --type Public
  plexus add-subnet production backend --cidr 10.0.2.0/24
  plexus add-subnet production dual --cidr 10.0.3.0/24 --cidr fd00::3/64 --type Private
  plexus add-subnet production zoned --cidr 10.0.4.0/24 --cluster-selector region=eu-west --node-selector topology.kubernetes.io/zone=rack-a`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ndName := args[0]
			subnetName := args[1]

			if err := validateCIDRs(cidrs); err != nil {
				return err
			}

			st, err := validateSubnetType(subnetType)
			if err != nil {
				return err
			}

			var az *v1beta1.AvailabilityZone
			if len(clusterSelector) > 0 || len(nodeSelector) > 0 {
				csLabels, err := parseKeyValuePairs(clusterSelector)
				if err != nil {
					return fmt.Errorf("invalid --cluster-selector: %w", err)
				}
				nsLabels, err := parseKeyValuePairs(nodeSelector)
				if err != nil {
					return fmt.Errorf("invalid --node-selector: %w", err)
				}
				if len(csLabels) == 0 {
					return fmt.Errorf("--cluster-selector is required when --node-selector is specified")
				}
				az = &v1beta1.AvailabilityZone{
					ClusterSelector: &metav1.LabelSelector{MatchLabels: csLabels},
					NodeSelector:    nsLabels,
				}
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			and := &v1beta1.AdministrativeNetworkDomain{}
			if err := c.Get(context.TODO(), client.ObjectKey{Name: ndName}, and); err != nil {
				return fmt.Errorf("getting AND %q: %w", ndName, err)
			}

			if and.Spec == nil {
				and.Spec = &v1beta1.AdministrativeNetworkDomainSpec{}
			}

			for _, s := range and.Spec.Subnets {
				if s.Name == subnetName {
					return fmt.Errorf("subnet %q already exists in AND %q", subnetName, ndName)
				}
			}

			and.Spec.Subnets = append(and.Spec.Subnets, v1beta1.Subnet{
				Name:             subnetName,
				CIDRs:            toCIDRs(cidrs),
				Type:             &st,
				AvailabilityZone: az,
			})

			patchBytes, err := json.Marshal(map[string]interface{}{
				"spec": map[string]interface{}{
					"subnets": and.Spec.Subnets,
				},
			})
			if err != nil {
				return fmt.Errorf("marshaling patch: %w", err)
			}

			if err := c.Patch(context.TODO(), and, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
				return fmt.Errorf("patching AND %q: %w", ndName, err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "subnet/%s added to administrativenetworkdomain/%s\n", subnetName, ndName)
			return err
		},
	}

	cmd.Flags().StringSliceVar(&cidrs, "cidr", nil, "CIDR for the subnet (required, repeatable for dual-stack)")
	cmd.Flags().StringVar(&subnetType, "type", "Private", "Subnet type: Public, Private, Isolated, VPNOnly")
	cmd.Flags().StringSliceVar(&clusterSelector, "cluster-selector", nil, "Cluster selector labels as key=value (repeatable)")
	cmd.Flags().StringSliceVar(&nodeSelector, "node-selector", nil, "Node selector labels as key=value (repeatable)")
	_ = cmd.MarkFlagRequired("cidr")

	return cmd
}
