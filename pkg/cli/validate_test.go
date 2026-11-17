package cli

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

func TestValidateCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cidrs   []string
		wantErr bool
	}{
		{name: "IPv4 network", cidrs: []string{"10.0.1.0/24"}},
		{name: "dual stack", cidrs: []string{"10.0.1.0/24", "fd00::/64"}},
		{name: "IPv4 host bits", cidrs: []string{"10.0.1.5/24"}, wantErr: true},
		{name: "IPv6 host bits", cidrs: []string{"fd00::1/64"}, wantErr: true},
		{name: "two IPv4", cidrs: []string{"10.0.1.0/24", "10.0.2.0/24"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCIDRs(tc.cidrs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateCIDRs(%v) error = %v, wantErr %v", tc.cidrs, err, tc.wantErr)
			}
		})
	}
}

func TestValidateSubnetName(t *testing.T) {
	for _, tc := range []struct {
		domain  string
		subnet  string
		wantErr bool
	}{
		{domain: "production", subnet: "web"},
		{domain: "production", subnet: "Bad_Name", wantErr: true},
		{domain: "production", subnet: "", wantErr: true},
		{domain: strings.Repeat("a", 60), subnet: "web", wantErr: true},
		{domain: "has.dot", subnet: "web", wantErr: true},
	} {
		err := validateSubnetName(tc.domain, tc.subnet)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateSubnetName(%q, %q) error = %v, wantErr %v", tc.domain, tc.subnet, err, tc.wantErr)
		}
	}
}

func TestFormatAZExpression(t *testing.T) {
	az := v1beta1.AvailabilityZone{ClusterSelector: metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "region", Operator: metav1.LabelSelectorOpIn, Values: []string{"east", "west"},
		}},
	}}
	if got := formatAZ(az); !strings.Contains(got, "region In east,west") {
		t.Fatalf("formatAZ() = %q, missing selector expression", got)
	}
}
