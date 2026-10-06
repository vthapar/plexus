package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

func newDeleteSubnetCommand() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete-subnet <network-domain> <subnet-name>",
		Short: "Delete a subnet from an AdministrativeNetworkDomain",
		Long: `Delete a subnet from an existing AdministrativeNetworkDomain.
This will trigger the controller to clean up all backend resources
associated with the subnet.

Examples:
  plexus delete-subnet production web
  plexus delete-subnet production web --yes`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ndName := args[0]
			subnetName := args[1]

			if !yes {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(),
					"This will delete subnet %q from AND %q and remove its associated resources. Continue? [y/N] ",
					subnetName, ndName); err != nil {
					return err
				}
				reader := bufio.NewReader(cmd.InOrStdin())
				answer, err := reader.ReadString('\n')
				if err != nil {
					return fmt.Errorf("reading confirmation: %w", err)
				}
				if strings.TrimSpace(strings.ToLower(answer)) != "y" {
					_, err = fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return err
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

			subnets := and.Subnets()
			found := false
			remaining := make([]v1beta1.Subnet, 0, len(subnets))
			for _, s := range subnets {
				if s.Name == subnetName {
					found = true
					continue
				}
				remaining = append(remaining, s)
			}
			if !found {
				return fmt.Errorf("subnet %q not found in AND %q", subnetName, ndName)
			}

			patchBytes, err := json.Marshal(map[string]interface{}{
				"spec": map[string]interface{}{
					"subnets": remaining,
				},
			})
			if err != nil {
				return fmt.Errorf("marshaling patch: %w", err)
			}

			if err := c.Patch(context.TODO(), and, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
				return fmt.Errorf("patching AND %q: %w", ndName, err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "subnet/%s deleted from administrativenetworkdomain/%s\n", subnetName, ndName)
			return err
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}
