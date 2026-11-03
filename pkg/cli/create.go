package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

func newCreateCommand() *cobra.Command {
	var cidrs []string
	var subnetType string

	cmd := &cobra.Command{
		Use:   "create <name> <subnet-name> --cidr <cidr>",
		Short: "Create an AdministrativeNetworkDomain",
		Long: `Create an AdministrativeNetworkDomain with its first subnet.
Use 'plexus add-subnet' to add more subnets after creation.

Examples:
  plexus create production web --cidr 10.0.1.0/24 --type Public
  plexus create staging app --cidr 10.0.10.0/24`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSubnetName(args[0], args[1]); err != nil {
				return err
			}
			if err := validateCIDRs(cidrs); err != nil {
				return err
			}
			st, err := validateSubnetType(subnetType)
			if err != nil {
				return err
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			and := &v1beta1.AdministrativeNetworkDomain{
				ObjectMeta: metav1.ObjectMeta{
					Name: args[0],
				},
				Spec: v1beta1.AdministrativeNetworkDomainSpec{
					Subnets: []v1beta1.Subnet{{Name: args[1], CIDRs: toCIDRs(cidrs), Type: st}},
				},
			}

			if err := c.Create(context.TODO(), and); err != nil {
				return fmt.Errorf("creating AND %q: %w", args[0], err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "administrativenetworkdomain/%s created\n", args[0])
			return err
		},
	}
	cmd.Flags().StringSliceVar(&cidrs, "cidr", nil, "CIDR for the first subnet (required, repeatable for dual-stack)")
	cmd.Flags().StringVar(&subnetType, "type", "Private", "Subnet type: Public, Private, Isolated, VPNOnly")
	_ = cmd.MarkFlagRequired("cidr")
	return cmd
}
