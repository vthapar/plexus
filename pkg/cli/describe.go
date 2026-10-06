package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

func newDescribeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "describe <name>",
		Short: "Show details of an AdministrativeNetworkDomain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			and := &v1beta1.AdministrativeNetworkDomain{}
			if err := c.Get(context.TODO(), client.ObjectKey{Name: args[0]}, and); err != nil {
				return fmt.Errorf("getting AND %q: %w", args[0], err)
			}

			out := cmd.OutOrStdout()

			if _, err := fmt.Fprintf(out, "Name:         %s\n", and.Name); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "Created:      %s (%s ago)\n",
				and.CreationTimestamp.Format(time.RFC3339),
				formatDuration(time.Since(and.CreationTimestamp.Time))); err != nil {
				return err
			}
			subnets := and.Subnets()
			if _, err := fmt.Fprintf(out, "Subnets:      %d\n", len(subnets)); err != nil {
				return err
			}

			if len(subnets) > 0 {
				if _, err := fmt.Fprintln(out); err != nil {
					return err
				}
				w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				if _, err := fmt.Fprintln(w, "  SUBNET\tCIDRS\tTYPE\tAVAILABILITY ZONE"); err != nil {
					return err
				}
				for _, s := range subnets {
					if _, err := fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
						s.Name,
						cidrStrings(s.CIDRs),
						s.TypeValue(),
						formatAZ(s.AvailabilityZone)); err != nil {
						return err
					}
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}

			if and.Status != nil && len(and.Status.Conditions) > 0 {
				if _, err := fmt.Fprintln(out); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(out, "Conditions:"); err != nil {
					return err
				}
				w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				if _, err := fmt.Fprintln(w, "  TYPE\tSTATUS\tREASON\tMESSAGE\tLAST TRANSITION"); err != nil {
					return err
				}
				for _, c := range and.Status.Conditions {
					if _, err := fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
						c.Type,
						c.Status,
						c.Reason,
						c.Message,
						formatDuration(time.Since(c.LastTransitionTime.Time))+" ago"); err != nil {
						return err
					}
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}

			return nil
		},
	}
}

func formatAZ(az *v1beta1.AvailabilityZone) string {
	if az == nil {
		return "<none>"
	}

	var parts []string

	if az.ClusterSelector != nil && len(az.ClusterSelector.MatchLabels) > 0 {
		for k, v := range az.ClusterSelector.MatchLabels {
			parts = append(parts, fmt.Sprintf("cluster(%s=%s)", k, v))
		}
	}
	if len(az.NodeSelector) > 0 {
		for k, v := range az.NodeSelector {
			parts = append(parts, fmt.Sprintf("node(%s=%s)", k, v))
		}
	}

	if len(parts) == 0 {
		return "<empty>"
	}
	return strings.Join(parts, ", ")
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
