/*
Copyright 2023 The Kubernetes Authors.

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

// Package options contains flags and options for initializing an apiserver
package options

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	peerreconcilers "k8s.io/apiserver/pkg/reconcilers"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/client-go/util/keyutil"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"
	netutil "k8s.io/utils/net"

	_ "k8s.io/kubernetes/pkg/features"
	kubeauthenticator "k8s.io/kubernetes/pkg/kubeapiserver/authenticator"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// Options define the flags and validation for a generic controlplane. If the
// structs are nil, the options are not added to the command line and not validated.
type Options struct {
	GenericServerRunOptions *genericoptions.ServerRunOptions
	Etcd                    *genericoptions.EtcdOptions
	SecureServing           *genericoptions.SecureServingOptionsWithLoopback
	Audit                   *genericoptions.AuditOptions
	Features                *genericoptions.FeatureOptions
	Admission               *kubeoptions.AdmissionOptions
	Authentication          *kubeoptions.BuiltInAuthenticationOptions
	Authorization           *kubeoptions.BuiltInAuthorizationOptions
	APIEnablement           *genericoptions.APIEnablementOptions
	EgressSelector          *genericoptions.EgressSelectorOptions
	Metrics                 *metrics.Options
	Logs                    *logs.Options
	Traces                  *genericoptions.TracingOptions

	EnableLogsHandler        bool
	EventTTL                 time.Duration
	MaxConnectionBytesPerSec int64

	ProxyClientCertFile string
	ProxyClientKeyFile  string

	// PeerCAFile is the ca bundle used by this kube-apiserver to verify peer apiservers'
	// serving certs when routing a request to the peer in the case the request can not be served
	// locally due to version skew.
	PeerCAFile string

	// PeerAdvertiseAddress is the IP for this kube-apiserver which is used by peer apiservers to route a request
	// to this apiserver. This happens in cases where the peer is not able to serve the request due to
	// version skew.
	PeerAdvertiseAddress peerreconcilers.PeerAdvertiseAddress

	EnableAggregatorRouting             bool
	AggregatorRejectForwardingRedirects bool

	ServiceAccountSigningKeyFile     string
	ServiceAccountIssuer             serviceaccount.TokenGenerator
	ServiceAccountTokenMaxExpiration time.Duration

	ShowHiddenMetricsForVersion string
}

// completedServerRunOptions is a private wrapper that enforces a call of Complete() before Run can be invoked.
type completedOptions struct {
	Options
}

type CompletedOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedOptions
}

// NewOptions creates a new ServerRunOptions object with default parameters
func NewOptions() *Options {
	s := Options{
		GenericServerRunOptions: genericoptions.NewServerRunOptions(),
		Etcd:                    genericoptions.NewEtcdOptions(storagebackend.NewDefaultConfig(kubeoptions.DefaultEtcdPathPrefix, nil)),
		SecureServing:           kubeoptions.NewSecureServingOptions(),
		Audit:                   genericoptions.NewAuditOptions(),
		Features:                genericoptions.NewFeatureOptions(),
		Admission:               kubeoptions.NewAdmissionOptions(),
		Authentication:          kubeoptions.NewBuiltInAuthenticationOptions().WithAll(),
		Authorization:           kubeoptions.NewBuiltInAuthorizationOptions(),
		APIEnablement:           genericoptions.NewAPIEnablementOptions(),
		EgressSelector:          genericoptions.NewEgressSelectorOptions(),
		Metrics:                 metrics.NewOptions(),
		Logs:                    logs.NewOptions(),
		Traces:                  genericoptions.NewTracingOptions(),

		EnableLogsHandler:                   true,
		EventTTL:                            1 * time.Hour,
		AggregatorRejectForwardingRedirects: true,
	}

	// Overwrite the default for storage data format.
	s.Etcd.DefaultStorageMediaType = "application/vnd.kubernetes.protobuf"

	return &s
}

func (s *Options) AddFlags(fss *cliflag.NamedFlagSets) {
	// Add the generic flags.
	s.GenericServerRunOptions.AddUniversalFlags(fss.FlagSet("generic"))
	s.Etcd.AddFlags(fss.FlagSet("etcd"))
	s.SecureServing.AddFlags(fss.FlagSet("secure serving"))
	s.Audit.AddFlags(fss.FlagSet("auditing"))
	s.Features.AddFlags(fss.FlagSet("features"))
	s.Authentication.AddFlags(fss.FlagSet("authentication"))
	s.Authorization.AddFlags(fss.FlagSet("authorization"))
	s.APIEnablement.AddFlags(fss.FlagSet("API enablement"))
	s.EgressSelector.AddFlags(fss.FlagSet("egress selector"))
	s.Admission.AddFlags(fss.FlagSet("admission"))
	s.Metrics.AddFlags(fss.FlagSet("metrics"))
	logsapi.AddFlags(s.Logs, fss.FlagSet("logs"))
	s.Traces.AddFlags(fss.FlagSet("traces"))

	// Note: the weird ""+ in below lines seems to be the only way to get gofmt to
	// arrange these text blocks sensibly. Grrr.
	fs := fss.FlagSet("misc")
	fs.DurationVar(&s.EventTTL, "event-ttl", s.EventTTL,
		"Amount of time to retain events.")

	fs.BoolVar(&s.EnableLogsHandler, "enable-logs-handler", s.EnableLogsHandler,
		"If true, install a /logs handler for the apiserver logs.")
	fs.MarkDeprecated("enable-logs-handler", "This flag will be removed in v1.19")

	fs.Int64Var(&s.MaxConnectionBytesPerSec, "max-connection-bytes-per-sec", s.MaxConnectionBytesPerSec, ""+
		"If non-zero, throttle each user connection to this number of bytes/sec. "+
		"Currently only applies to long-running requests.")

	fs.StringVar(&s.ProxyClientCertFile, "proxy-client-cert-file", s.ProxyClientCertFile, ""+
		"Client certificate used to prove the identity of the aggregator or kube-apiserver "+
		"when it must call out during a request. This includes proxying requests to a user "+
		"api-server and calling out to webhook admission plugins. It is expected that this "+
		"cert includes a signature from the CA in the --requestheader-client-ca-file flag. "+
		"That CA is published in the 'extension-apiserver-authentication' configmap in "+
		"the kube-system namespace. Components receiving calls from kube-aggregator should "+
		"use that CA to perform their half of the mutual TLS verification.")
	fs.StringVar(&s.ProxyClientKeyFile, "proxy-client-key-file", s.ProxyClientKeyFile, ""+
		"Private key for the client certificate used to prove the identity of the aggregator or kube-apiserver "+
		"when it must call out during a request. This includes proxying requests to a user "+
		"api-server and calling out to webhook admission plugins.")

	fs.StringVar(&s.PeerCAFile, "peer-ca-file", s.PeerCAFile,
		"If set and the UnknownVersionInteroperabilityProxy feature gate is enabled, this file will be used to verify serving certificates of peer kube-apiservers. "+
			"This flag is only used in clusters configured with multiple kube-apiservers for high availability.")

	fs.StringVar(&s.PeerAdvertiseAddress.PeerAdvertiseIP, "peer-advertise-ip", s.PeerAdvertiseAddress.PeerAdvertiseIP,
		"If set and the UnknownVersionInteroperabilityProxy feature gate is enabled, this IP will be used by peer kube-apiservers to proxy requests to this kube-apiserver "+
			"when the request cannot be handled by the peer due to version skew between the kube-apiservers. "+
			"This flag is only used in clusters configured with multiple kube-apiservers for high availability. ")

	fs.StringVar(&s.PeerAdvertiseAddress.PeerAdvertisePort, "peer-advertise-port", s.PeerAdvertiseAddress.PeerAdvertisePort,
		"If set and the UnknownVersionInteroperabilityProxy feature gate is enabled, this port will be used by peer kube-apiservers to proxy requests to this kube-apiserver "+
			"when the request cannot be handled by the peer due to version skew between the kube-apiservers. "+
			"This flag is only used in clusters configured with multiple kube-apiservers for high availability. ")

	fs.BoolVar(&s.EnableAggregatorRouting, "enable-aggregator-routing", s.EnableAggregatorRouting,
		"Turns on aggregator routing requests to endpoints IP rather than cluster IP.")

	fs.BoolVar(&s.AggregatorRejectForwardingRedirects, "aggregator-reject-forwarding-redirect", s.AggregatorRejectForwardingRedirects,
		"Aggregator reject forwarding redirect response back to client.")

	fs.StringVar(&s.ServiceAccountSigningKeyFile, "service-account-signing-key-file", s.ServiceAccountSigningKeyFile, ""+
		"Path to the file that contains the current private key of the service account token issuer. The issuer will sign issued ID tokens with this private key.")
}

// Params: {"kubernetes.default.svc", "kubernetes.default", "kubernetes"}, 10.96.0.2
// DefaultAdvertiseAddress(): sets default advertised address to 172.18.0.2
// MaybeDefaultWithSelfSignedCerts(): keyCert = {/etc/kubernetes/pki/apiserver.crt /etc/kubernetes/pki/apiserver.key}
// Authorization.Complete(): o.Modes= [Node RBAC]
// ExternalHost = 172.18.0.2
// ApplyAuthorization(): Anonymous.Allow = true
// completed.ServiceAccountSigningKeyFile = /etc/kubernetes/pki/sa.key
// completed.Authentication.ServiceAccounts.Issuers = https://kubernetes.default.svc.cluster.local
// JWTTokenGenerator(): generated JWT token based on above issuer and private key file
func (o *Options) Complete(alternateDNS []string, alternateIPs []net.IP) (CompletedOptions, error) {
	if o == nil {
		return CompletedOptions{completedOptions: &completedOptions{}}, nil
	}

	completed := completedOptions{
		Options: *o,
	}

	// set defaults
	if err := completed.GenericServerRunOptions.DefaultAdvertiseAddress(completed.SecureServing.SecureServingOptions); err != nil {
		return CompletedOptions{}, err
	}

	if err := completed.SecureServing.MaybeDefaultWithSelfSignedCerts(completed.GenericServerRunOptions.AdvertiseAddress.String(), alternateDNS, alternateIPs); err != nil {
		return CompletedOptions{}, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	if len(completed.GenericServerRunOptions.ExternalHost) == 0 {
		if len(completed.GenericServerRunOptions.AdvertiseAddress) > 0 {
			completed.GenericServerRunOptions.ExternalHost = completed.GenericServerRunOptions.AdvertiseAddress.String()
		} else {
			hostname, err := os.Hostname()
			if err != nil {
				return CompletedOptions{}, fmt.Errorf("error finding host name: %v", err)
			}
			completed.GenericServerRunOptions.ExternalHost = hostname
		}
		klog.Infof("external host was not specified, using %v", completed.GenericServerRunOptions.ExternalHost)
	}
	
	// put authorization options in final state
	// set modes: [Node RBAC]
	completed.Authorization.Complete()
	// adjust authentication for completed authorization
	// sets Anonymous.Allow = true
	completed.Authentication.ApplyAuthorization(completed.Authorization)

	// Use (ServiceAccountSigningKeyFile != "") as a proxy to the user enabling
	// TokenRequest functionality. This defaulting was convenient, but messed up
	// a lot of people when they rotated their serving cert with no idea it was
	// connected to their service account keys. We are taking this opportunity to
	// remove this problematic defaulting.
	// From Docs: Specifies the path to a file that contains the current private key of the service account 
	// token issuer. The issuer signs issued ID tokens with this private key.
	// completed.ServiceAccountSigningKeyFile = /etc/kubernetes/pki/sa.key
	// completed.Authentication.ServiceAccounts.Issuers = https://kubernetes.default.svc.cluster.local
	if completed.ServiceAccountSigningKeyFile == "" {
		// Default to the private server key for service account token signing
		if len(completed.Authentication.ServiceAccounts.KeyFiles) == 0 && completed.SecureServing.ServerCert.CertKey.KeyFile != "" {
			if kubeauthenticator.IsValidServiceAccountKeyFile(completed.SecureServing.ServerCert.CertKey.KeyFile) {
				completed.Authentication.ServiceAccounts.KeyFiles = []string{completed.SecureServing.ServerCert.CertKey.KeyFile}
			} else {
				klog.Warning("No TLS key provided, service account token authentication disabled")
			}
		}
	}

	if completed.ServiceAccountSigningKeyFile != "" && len(completed.Authentication.ServiceAccounts.Issuers) != 0 && completed.Authentication.ServiceAccounts.Issuers[0] != "" {
		sk, err := keyutil.PrivateKeyFromFile(completed.ServiceAccountSigningKeyFile)
		if err != nil {
			return CompletedOptions{}, fmt.Errorf("failed to parse service-account-issuer-key-file: %v", err)
		}
		if completed.Authentication.ServiceAccounts.MaxExpiration != 0 {
			lowBound := time.Hour
			upBound := time.Duration(1<<32) * time.Second
			if completed.Authentication.ServiceAccounts.MaxExpiration < lowBound ||
				completed.Authentication.ServiceAccounts.MaxExpiration > upBound {
				return CompletedOptions{}, fmt.Errorf("the service-account-max-token-expiration must be between 1 hour and 2^32 seconds")
			}
			if completed.Authentication.ServiceAccounts.ExtendExpiration {
				if completed.Authentication.ServiceAccounts.MaxExpiration < serviceaccount.WarnOnlyBoundTokenExpirationSeconds*time.Second {
					klog.Warningf("service-account-extend-token-expiration is true, in order to correctly trigger safe transition logic, service-account-max-token-expiration must be set longer than %d seconds (currently %s)", serviceaccount.WarnOnlyBoundTokenExpirationSeconds, completed.Authentication.ServiceAccounts.MaxExpiration)
				}
				if completed.Authentication.ServiceAccounts.MaxExpiration < serviceaccount.ExpirationExtensionSeconds*time.Second {
					klog.Warningf("service-account-extend-token-expiration is true, enabling tokens valid up to %d seconds, which is longer than service-account-max-token-expiration set to %s seconds", serviceaccount.ExpirationExtensionSeconds, completed.Authentication.ServiceAccounts.MaxExpiration)
				}
			}
		}

		completed.ServiceAccountIssuer, err = serviceaccount.JWTTokenGenerator(completed.Authentication.ServiceAccounts.Issuers[0], sk)
		if err != nil {
			return CompletedOptions{}, fmt.Errorf("failed to build token generator: %v", err)
		}
		completed.ServiceAccountTokenMaxExpiration = completed.Authentication.ServiceAccounts.MaxExpiration
	}

	// map is empty, so skip
	for key, value := range completed.APIEnablement.RuntimeConfig {
		if key == "v1" || strings.HasPrefix(key, "v1/") ||
			key == "api/v1" || strings.HasPrefix(key, "api/v1/") {
			delete(completed.APIEnablement.RuntimeConfig, key)
			completed.APIEnablement.RuntimeConfig["/v1"] = value
		}
		if key == "api/legacy" {
			delete(completed.APIEnablement.RuntimeConfig, key)
		}
	}

	return CompletedOptions{
		completedOptions: &completed,
	}, nil
}

// ServiceIPRange checks if the serviceClusterIPRange flag is nil, raising a warning if so and
// setting service ip range to the default value in kubeoptions.DefaultServiceIPCIDR
// for now until the default is removed per the deprecation timeline guidelines.
// Returns service ip range, api server service IP, and an error
func ServiceIPRange(passedServiceClusterIPRange net.IPNet) (net.IPNet, net.IP, error) {
	serviceClusterIPRange := passedServiceClusterIPRange
	if passedServiceClusterIPRange.IP == nil {
		klog.Warningf("No CIDR for service cluster IPs specified. Default value which was %s is deprecated and will be removed in future releases. Please specify it using --service-cluster-ip-range on kube-apiserver.", kubeoptions.DefaultServiceIPCIDR.String())
		serviceClusterIPRange = kubeoptions.DefaultServiceIPCIDR
	}

	size := integer.Int64Min(netutil.RangeSize(&serviceClusterIPRange), 1<<16)
	if size < 8 {
		return net.IPNet{}, net.IP{}, fmt.Errorf("the service cluster IP range must be at least %d IP addresses", 8)
	}

	// Select the first valid IP from ServiceClusterIPRange to use as the GenericAPIServer service IP.
	apiServerServiceIP, err := netutil.GetIndexedIP(&serviceClusterIPRange, 1)
	if err != nil {
		return net.IPNet{}, net.IP{}, err
	}
	klog.V(4).Infof("Setting service IP to %q (read-write).", apiServerServiceIP)

	return serviceClusterIPRange, apiServerServiceIP, nil
}
