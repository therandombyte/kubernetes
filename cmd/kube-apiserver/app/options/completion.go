/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package options

import (
	"fmt"
	"net"
	"strings"

	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	_ "k8s.io/component-base/metrics/prometheus/workqueue"
	netutils "k8s.io/utils/net"

	controlplane "k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/pkg/kubeapiserver"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
)

// completedOptions is a private wrapper that enforces a call of Complete() before Run can be invoked.
type completedOptions struct {
	controlplane.CompletedOptions
	CloudProvider *kubeoptions.CloudProviderOptions

	Extra
}

type CompletedOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedOptions
}

// Complete set default ServerRunOptions.
// Should be called after kube-apiserver flags parsed.
// <Nikhil>: another hardcoding on line 60, thats great
// options.Complete(): sets default advertised address to 172.18.0.2
// 			   get api server key and cert, set mode as Node and RBAC, set Anonymous.Allow= true
//			   set sa.key as sa signing key file & sa issuer as https://kubernetes.default.svc.cluster.local
//			   generate JWT token for sa key file
func (opts *ServerRunOptions) Complete() (CompletedOptions, error) {
	if opts == nil {
		return CompletedOptions{completedOptions: &completedOptions{}}, nil
	}

	// process opts.ServiceClusterIPRange from list to Primary and Secondary
	// we process secondary only if provided by user
	apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, err := getServiceIPAndRanges(opts.ServiceClusterIPRanges)
	if err != nil {
		return CompletedOptions{}, err
	}
	controlplane, err := opts.Options.Complete([]string{"kubernetes.default.svc", "kubernetes.default", "kubernetes"}, []net.IP{apiServerServiceIP})
	if err != nil {
		return CompletedOptions{}, err
	}

	completed := completedOptions{
		CompletedOptions: controlplane,
		CloudProvider:    opts.CloudProvider,

		Extra: opts.Extra,
	}

	completed.PrimaryServiceClusterIPRange = primaryServiceIPRange
	completed.SecondaryServiceClusterIPRange = secondaryServiceIPRange
	completed.APIServerServiceIP = apiServerServiceIP

	if completed.Etcd != nil && completed.Etcd.EnableWatchCache {
		sizes := kubeapiserver.DefaultWatchCacheSizes()
		// Ensure that overrides parse correctly.
		userSpecified, err := apiserveroptions.ParseWatchCacheSizes(completed.Etcd.WatchCacheSizes)
		if err != nil {
			return CompletedOptions{}, err
		}
		for resource, size := range userSpecified {
			sizes[resource] = size
		}
		completed.Etcd.WatchCacheSizes, err = apiserveroptions.WriteWatchCacheSizes(sizes)
		if err != nil {
			return CompletedOptions{}, err
		}
	}

	return CompletedOptions{
		completedOptions: &completed,
	}, nil
}

func getServiceIPAndRanges(serviceClusterIPRanges string) (net.IP, net.IPNet, net.IPNet, error) {
	serviceClusterIPRangeList := []string{}
	if serviceClusterIPRanges != "" {
		serviceClusterIPRangeList = strings.Split(serviceClusterIPRanges, ",")
	}

	var apiServerServiceIP net.IP
	var primaryServiceIPRange net.IPNet     // IP and mask will represent a network
	var secondaryServiceIPRange net.IPNet
	var err error

	// nothing provided by user, use default range (only applies to the Primary)
	if len(serviceClusterIPRangeList) == 0 {
		var primaryServiceClusterCIDR net.IPNet
		primaryServiceIPRange, apiServerServiceIP, err = controlplane.ServiceIPRange(primaryServiceClusterCIDR)
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges: %v", err)
		}
		return apiServerServiceIP, primaryServiceIPRange, net.IPNet{}, nil
	}

	_, primaryServiceClusterCIDR, err := netutils.ParseCIDRSloppy(serviceClusterIPRangeList[0])
	if err != nil {
		return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[0] is not a valid cidr")
	}

	primaryServiceIPRange, apiServerServiceIP, err = controlplane.ServiceIPRange(*primaryServiceClusterCIDR)
	if err != nil {
		return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges for primary service cidr: %v", err)
	}

	// user provided at least two entries
	// note: validation asserts that the list is max of two dual stack entries
	if len(serviceClusterIPRangeList) > 1 {
		_, secondaryServiceClusterCIDR, err := netutils.ParseCIDRSloppy(serviceClusterIPRangeList[1])
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[1] is not an ip net")
		}
		secondaryServiceIPRange = *secondaryServiceClusterCIDR
	}
	return apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, nil
}
