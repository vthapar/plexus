package cli

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

var validSubnetTypes = map[v1beta1.SubnetType]bool{
	v1beta1.SubnetTypePublic:   true,
	v1beta1.SubnetTypePrivate:  true,
	v1beta1.SubnetTypeIsolated: true,
	v1beta1.SubnetTypeVPNOnly:  true,
}

func validateSubnetType(t string) (v1beta1.SubnetType, error) {
	st := v1beta1.SubnetType(t)
	if !validSubnetTypes[st] {
		return "", fmt.Errorf("invalid subnet type %q: must be one of Public, Private, Isolated, VPNOnly", t)
	}
	return st, nil
}

func validateCIDRs(cidrs []string) error {
	if len(cidrs) == 0 {
		return fmt.Errorf("at least one CIDR is required")
	}
	if len(cidrs) > 2 {
		return fmt.Errorf("at most two CIDRs are allowed (one IPv4, one IPv6 for dual-stack)")
	}

	hasV4, hasV6 := false, false
	for _, cidr := range cidrs {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		if !ip.Equal(network.IP) {
			return fmt.Errorf("invalid CIDR %q: use the network address %s", cidr, network.String())
		}
		if ip.To4() != nil {
			if hasV4 {
				return fmt.Errorf("two IPv4 CIDRs specified: dual-stack requires one IPv4 and one IPv6")
			}
			hasV4 = true
		} else {
			if hasV6 {
				return fmt.Errorf("two IPv6 CIDRs specified: dual-stack requires one IPv4 and one IPv6")
			}
			hasV6 = true
		}
	}
	return nil
}

func validateSubnetName(domainName, subnetName string) error {
	if errs := validation.IsDNS1123Label(subnetName); len(errs) > 0 {
		return fmt.Errorf("invalid subnet name %q: %s", subnetName, strings.Join(errs, ", "))
	}
	combined := domainName + "-" + subnetName
	if len(combined) > 63 {
		return fmt.Errorf("combined AND and subnet name %q exceeds 63 characters", combined)
	}
	if errs := validation.IsDNS1123Label(combined); len(errs) > 0 {
		return fmt.Errorf("invalid combined AND and subnet name %q: %s", combined, strings.Join(errs, ", "))
	}
	return nil
}

func toCIDRs(strs []string) []v1beta1.CIDR {
	cidrs := make([]v1beta1.CIDR, len(strs))
	for i, s := range strs {
		cidrs[i] = v1beta1.CIDR(s)
	}
	return cidrs
}

func cidrStrings(cidrs []v1beta1.CIDR) string {
	strs := make([]string, len(cidrs))
	for i, c := range cidrs {
		strs[i] = string(c)
	}
	return strings.Join(strs, ",")
}

func parseKeyValuePairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label %q: must be key=value", p)
		}
		result[k] = v
	}
	return result, nil
}
