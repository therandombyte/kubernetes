/*
Copyright 2016 The Kubernetes Authors.

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

// Package app implements a server that runs a set of active
// components.  This includes replication controllers, service endpoints and
// nodes.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	genericfeatures "k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/quota/v1/generic"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	restclient "k8s.io/client-go/rest"
	cloudnodelifecyclecontroller "k8s.io/cloud-provider/controllers/nodelifecycle"
	routecontroller "k8s.io/cloud-provider/controllers/route"
	servicecontroller "k8s.io/cloud-provider/controllers/service"
	cpnames "k8s.io/cloud-provider/names"
	"k8s.io/component-base/featuregate"
	"k8s.io/controller-manager/controller"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/kubernetes/cmd/kube-controller-manager/names"
	pkgcontroller "k8s.io/kubernetes/pkg/controller"
	endpointcontroller "k8s.io/kubernetes/pkg/controller/endpoint"
	"k8s.io/kubernetes/pkg/controller/garbagecollector"
	namespacecontroller "k8s.io/kubernetes/pkg/controller/namespace"
	nodeipamcontroller "k8s.io/kubernetes/pkg/controller/nodeipam"
	nodeipamconfig "k8s.io/kubernetes/pkg/controller/nodeipam/config"
	"k8s.io/kubernetes/pkg/controller/nodeipam/ipam"
	lifecyclecontroller "k8s.io/kubernetes/pkg/controller/nodelifecycle"
	"k8s.io/kubernetes/pkg/controller/podgc"
	replicationcontroller "k8s.io/kubernetes/pkg/controller/replication"
	"k8s.io/kubernetes/pkg/controller/resourceclaim"
	resourcequotacontroller "k8s.io/kubernetes/pkg/controller/resourcequota"
	serviceaccountcontroller "k8s.io/kubernetes/pkg/controller/serviceaccount"
	"k8s.io/kubernetes/pkg/controller/storageversiongc"
	"k8s.io/kubernetes/pkg/controller/tainteviction"
	ttlcontroller "k8s.io/kubernetes/pkg/controller/ttl"
	"k8s.io/kubernetes/pkg/controller/ttlafterfinished"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach"
	"k8s.io/kubernetes/pkg/controller/volume/ephemeral"
	"k8s.io/kubernetes/pkg/controller/volume/expand"
	persistentvolumecontroller "k8s.io/kubernetes/pkg/controller/volume/persistentvolume"
	"k8s.io/kubernetes/pkg/controller/volume/pvcprotection"
	"k8s.io/kubernetes/pkg/controller/volume/pvprotection"
	"k8s.io/kubernetes/pkg/features"
	quotainstall "k8s.io/kubernetes/pkg/quota/v1/install"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	"k8s.io/utils/clock"
	netutils "k8s.io/utils/net"
)

const (
	// defaultNodeMaskCIDRIPv4 is default mask size for IPv4 node cidr
	defaultNodeMaskCIDRIPv4 = 24
	// defaultNodeMaskCIDRIPv6 is default mask size for IPv6 node cidr
	defaultNodeMaskCIDRIPv6 = 64
)

func newServiceLBControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:                      cpnames.ServiceLBController,
		aliases:                   []string{"service"},
		initFunc:                  startServiceLBController,
		isCloudProviderController: true,
	}
}

func startServiceLBController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	serviceController, err := servicecontroller.New(
		controllerContext.Cloud,
		controllerContext.ClientBuilder.ClientOrDie("service-controller"),
		controllerContext.InformerFactory.Core().V1().Services(),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.ComponentConfig.KubeCloudShared.ClusterName,
		utilfeature.DefaultFeatureGate,
	)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.FromContext(ctx).Error(err, "Failed to start service controller")
		return nil, false, nil
	}
	go serviceController.Run(ctx, int(controllerContext.ComponentConfig.ServiceController.ConcurrentServiceSyncs), controllerContext.ControllerManagerMetrics)
	return nil, true, nil
}
func newNodeIpamControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.NodeIpamController,
		aliases:  []string{"nodeipam"},
		initFunc: startNodeIpamController,
	}
}

func startNodeIpamController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	var serviceCIDR *net.IPNet
	var secondaryServiceCIDR *net.IPNet
	logger := klog.FromContext(ctx)

	// should we start nodeIPAM
	if !controllerContext.ComponentConfig.KubeCloudShared.AllocateNodeCIDRs {
		return nil, false, nil
	}

	// Cannot run cloud ipam controller if cloud provider is nil (--cloud-provider not set or set to 'external')
	if controllerContext.Cloud == nil && controllerContext.ComponentConfig.KubeCloudShared.CIDRAllocatorType == string(ipam.CloudAllocatorType) {
		return nil, false, errors.New("--cidr-allocator-type is set to 'CloudAllocator' but cloud provider is not configured")
	}

	clusterCIDRs, err := validateCIDRs(controllerContext.ComponentConfig.KubeCloudShared.ClusterCIDR)
	if err != nil {
		return nil, false, err
	}

	// service cidr processing
	if len(strings.TrimSpace(controllerContext.ComponentConfig.NodeIPAMController.ServiceCIDR)) != 0 {
		_, serviceCIDR, err = netutils.ParseCIDRSloppy(controllerContext.ComponentConfig.NodeIPAMController.ServiceCIDR)
		if err != nil {
			logger.Info("Warning: unsuccessful parsing of service CIDR", "CIDR", controllerContext.ComponentConfig.NodeIPAMController.ServiceCIDR, "err", err)
		}
	}

	if len(strings.TrimSpace(controllerContext.ComponentConfig.NodeIPAMController.SecondaryServiceCIDR)) != 0 {
		_, secondaryServiceCIDR, err = netutils.ParseCIDRSloppy(controllerContext.ComponentConfig.NodeIPAMController.SecondaryServiceCIDR)
		if err != nil {
			logger.Info("Warning: unsuccessful parsing of service CIDR", "CIDR", controllerContext.ComponentConfig.NodeIPAMController.SecondaryServiceCIDR, "err", err)
		}
	}

	// the following checks are triggered if both serviceCIDR and secondaryServiceCIDR are provided
	if serviceCIDR != nil && secondaryServiceCIDR != nil {
		// should be dual stack (from different IPFamilies)
		dualstackServiceCIDR, err := netutils.IsDualStackCIDRs([]*net.IPNet{serviceCIDR, secondaryServiceCIDR})
		if err != nil {
			return nil, false, fmt.Errorf("failed to perform dualstack check on serviceCIDR and secondaryServiceCIDR error:%v", err)
		}
		if !dualstackServiceCIDR {
			return nil, false, fmt.Errorf("serviceCIDR and secondaryServiceCIDR are not dualstack (from different IPfamiles)")
		}
	}

	// only --node-cidr-mask-size-ipv4 and --node-cidr-mask-size-ipv6 supported with dual stack clusters.
	// --node-cidr-mask-size flag is incompatible with dual stack clusters.
	nodeCIDRMaskSizes, err := setNodeCIDRMaskSizes(controllerContext.ComponentConfig.NodeIPAMController, clusterCIDRs)
	if err != nil {
		return nil, false, err
	}

	nodeIpamController, err := nodeipamcontroller.NewNodeIpamController(
		ctx,
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.Cloud,
		controllerContext.ClientBuilder.ClientOrDie("node-controller"),
		clusterCIDRs,
		serviceCIDR,
		secondaryServiceCIDR,
		nodeCIDRMaskSizes,
		ipam.CIDRAllocatorType(controllerContext.ComponentConfig.KubeCloudShared.CIDRAllocatorType),
	)
	if err != nil {
		return nil, true, err
	}
	go nodeIpamController.RunWithMetrics(ctx, controllerContext.ControllerManagerMetrics)
	return nil, true, nil
}

func newNodeLifecycleControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.NodeLifecycleController,
		aliases:  []string{"nodelifecycle"},
		initFunc: startNodeLifecycleController,
	}
}

func startNodeLifecycleController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	lifecycleController, err := lifecyclecontroller.NewNodeLifecycleController(
		ctx,
		controllerContext.InformerFactory.Coordination().V1().Leases(),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.InformerFactory.Apps().V1().DaemonSets(),
		// node lifecycle controller uses existing cluster role from node-controller
		controllerContext.ClientBuilder.ClientOrDie("node-controller"),
		controllerContext.ComponentConfig.KubeCloudShared.NodeMonitorPeriod.Duration,
		controllerContext.ComponentConfig.NodeLifecycleController.NodeStartupGracePeriod.Duration,
		controllerContext.ComponentConfig.NodeLifecycleController.NodeMonitorGracePeriod.Duration,
		controllerContext.ComponentConfig.NodeLifecycleController.NodeEvictionRate,
		controllerContext.ComponentConfig.NodeLifecycleController.SecondaryNodeEvictionRate,
		controllerContext.ComponentConfig.NodeLifecycleController.LargeClusterSizeThreshold,
		controllerContext.ComponentConfig.NodeLifecycleController.UnhealthyZoneThreshold,
	)
	if err != nil {
		return nil, true, err
	}
	go lifecycleController.Run(ctx)
	return nil, true, nil
}

func newTaintEvictionControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.TaintEvictionController,
		initFunc: startTaintEvictionController,
		requiredFeatureGates: []featuregate.Feature{
			features.SeparateTaintEvictionController,
		},
	}
}

func startTaintEvictionController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	taintEvictionController, err := tainteviction.New(
		ctx,
		// taint-manager uses existing cluster role from node-controller
		controllerContext.ClientBuilder.ClientOrDie("node-controller"),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerName,
	)
	if err != nil {
		return nil, false, err
	}
	go taintEvictionController.Run(ctx)
	return nil, true, nil
}

func newCloudNodeLifecycleControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:                      cpnames.CloudNodeLifecycleController,
		aliases:                   []string{"cloud-node-lifecycle"},
		initFunc:                  startCloudNodeLifecycleController,
		isCloudProviderController: true,
	}
}

func startCloudNodeLifecycleController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	cloudNodeLifecycleController, err := cloudnodelifecyclecontroller.NewCloudNodeLifecycleController(
		controllerContext.InformerFactory.Core().V1().Nodes(),
		// cloud node lifecycle controller uses existing cluster role from node-controller
		controllerContext.ClientBuilder.ClientOrDie("node-controller"),
		controllerContext.Cloud,
		controllerContext.ComponentConfig.KubeCloudShared.NodeMonitorPeriod.Duration,
	)
	if err != nil {
		// the controller manager should continue to run if the "Instances" interface is not
		// supported, though it's unlikely for a cloud provider to not support it
		logger.Error(err, "Failed to start cloud node lifecycle controller")
		return nil, false, nil
	}

	go cloudNodeLifecycleController.Run(ctx, controllerContext.ControllerManagerMetrics)
	return nil, true, nil
}

func newNodeRouteControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:                      cpnames.NodeRouteController,
		aliases:                   []string{"route"},
		initFunc:                  startNodeRouteController,
		isCloudProviderController: true,
	}
}

func startNodeRouteController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	if !controllerContext.ComponentConfig.KubeCloudShared.AllocateNodeCIDRs || !controllerContext.ComponentConfig.KubeCloudShared.ConfigureCloudRoutes {
		logger.Info("Will not configure cloud provider routes for allocate-node-cidrs", "CIDRs", controllerContext.ComponentConfig.KubeCloudShared.AllocateNodeCIDRs, "routes", controllerContext.ComponentConfig.KubeCloudShared.ConfigureCloudRoutes)
		return nil, false, nil
	}
	if controllerContext.Cloud == nil {
		logger.Info("Warning: configure-cloud-routes is set, but no cloud provider specified. Will not configure cloud provider routes.")
		return nil, false, nil
	}
	routes, ok := controllerContext.Cloud.Routes()
	if !ok {
		logger.Info("Warning: configure-cloud-routes is set, but cloud provider does not support routes. Will not configure cloud provider routes.")
		return nil, false, nil
	}

	clusterCIDRs, err := validateCIDRs(controllerContext.ComponentConfig.KubeCloudShared.ClusterCIDR)
	if err != nil {
		return nil, false, err
	}

	routeController := routecontroller.New(routes,
		controllerContext.ClientBuilder.ClientOrDie("route-controller"),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.ComponentConfig.KubeCloudShared.ClusterName,
		clusterCIDRs)
	go routeController.Run(ctx, controllerContext.ComponentConfig.KubeCloudShared.RouteReconciliationPeriod.Duration, controllerContext.ControllerManagerMetrics)
	return nil, true, nil
}

func newPersistentVolumeBinderControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.PersistentVolumeBinderController,
		aliases:  []string{"persistentvolume-binder"},
		initFunc: startPersistentVolumeBinderController,
	}
}

func startPersistentVolumeBinderController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	plugins, err := ProbeControllerVolumePlugins(logger, controllerContext.ComponentConfig.PersistentVolumeBinderController.VolumeConfiguration)
	if err != nil {
		return nil, true, fmt.Errorf("failed to probe volume plugins when starting persistentvolume controller: %v", err)
	}

	params := persistentvolumecontroller.ControllerParameters{
		KubeClient:                controllerContext.ClientBuilder.ClientOrDie("persistent-volume-binder"),
		SyncPeriod:                controllerContext.ComponentConfig.PersistentVolumeBinderController.PVClaimBinderSyncPeriod.Duration,
		VolumePlugins:             plugins,
		Cloud:                     controllerContext.Cloud,
		ClusterName:               controllerContext.ComponentConfig.KubeCloudShared.ClusterName,
		VolumeInformer:            controllerContext.InformerFactory.Core().V1().PersistentVolumes(),
		ClaimInformer:             controllerContext.InformerFactory.Core().V1().PersistentVolumeClaims(),
		ClassInformer:             controllerContext.InformerFactory.Storage().V1().StorageClasses(),
		PodInformer:               controllerContext.InformerFactory.Core().V1().Pods(),
		NodeInformer:              controllerContext.InformerFactory.Core().V1().Nodes(),
		EnableDynamicProvisioning: controllerContext.ComponentConfig.PersistentVolumeBinderController.VolumeConfiguration.EnableDynamicProvisioning,
	}
	volumeController, volumeControllerErr := persistentvolumecontroller.NewController(ctx, params)
	if volumeControllerErr != nil {
		return nil, true, fmt.Errorf("failed to construct persistentvolume controller: %v", volumeControllerErr)
	}
	go volumeController.Run(ctx)
	return nil, true, nil
}

func newPersistentVolumeAttachDetachControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.PersistentVolumeAttachDetachController,
		aliases:  []string{"attachdetach"},
		initFunc: startPersistentVolumeAttachDetachController,
	}
}

func startPersistentVolumeAttachDetachController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	csiNodeInformer := controllerContext.InformerFactory.Storage().V1().CSINodes()
	csiDriverInformer := controllerContext.InformerFactory.Storage().V1().CSIDrivers()

	plugins, err := ProbeAttachableVolumePlugins(logger)
	if err != nil {
		return nil, true, fmt.Errorf("failed to probe volume plugins when starting attach/detach controller: %v", err)
	}

	ctx = klog.NewContext(ctx, logger)
	attachDetachController, attachDetachControllerErr :=
		attachdetach.NewAttachDetachController(
			logger,
			controllerContext.ClientBuilder.ClientOrDie("attachdetach-controller"),
			controllerContext.InformerFactory.Core().V1().Pods(),
			controllerContext.InformerFactory.Core().V1().Nodes(),
			controllerContext.InformerFactory.Core().V1().PersistentVolumeClaims(),
			controllerContext.InformerFactory.Core().V1().PersistentVolumes(),
			csiNodeInformer,
			csiDriverInformer,
			controllerContext.InformerFactory.Storage().V1().VolumeAttachments(),
			controllerContext.Cloud,
			plugins,
			GetDynamicPluginProber(controllerContext.ComponentConfig.PersistentVolumeBinderController.VolumeConfiguration),
			controllerContext.ComponentConfig.AttachDetachController.DisableAttachDetachReconcilerSync,
			controllerContext.ComponentConfig.AttachDetachController.ReconcilerSyncLoopPeriod.Duration,
			attachdetach.DefaultTimerConfig,
		)
	if attachDetachControllerErr != nil {
		return nil, true, fmt.Errorf("failed to start attach/detach controller: %v", attachDetachControllerErr)
	}
	go attachDetachController.Run(ctx)
	return nil, true, nil
}

func newPersistentVolumeExpanderControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.PersistentVolumeExpanderController,
		aliases:  []string{"persistentvolume-expander"},
		initFunc: startPersistentVolumeExpanderController,
	}
}

func startPersistentVolumeExpanderController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	logger := klog.FromContext(ctx)
	plugins, err := ProbeExpandableVolumePlugins(logger, controllerContext.ComponentConfig.PersistentVolumeBinderController.VolumeConfiguration)
	if err != nil {
		return nil, true, fmt.Errorf("failed to probe volume plugins when starting volume expand controller: %v", err)
	}
	csiTranslator := csitrans.New()

	expandController, expandControllerErr := expand.NewExpandController(
		controllerContext.ClientBuilder.ClientOrDie("expand-controller"),
		controllerContext.InformerFactory.Core().V1().PersistentVolumeClaims(),
		controllerContext.Cloud,
		plugins,
		csiTranslator,
		csimigration.NewPluginManager(csiTranslator, utilfeature.DefaultFeatureGate),
	)

	if expandControllerErr != nil {
		return nil, true, fmt.Errorf("failed to start volume expand controller: %v", expandControllerErr)
	}
	go expandController.Run(ctx)
	return nil, true, nil
}

func newEphemeralVolumeControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.EphemeralVolumeController,
		aliases:  []string{"ephemeral-volume"},
		initFunc: startEphemeralVolumeController,
	}
}

func startEphemeralVolumeController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	ephemeralController, err := ephemeral.NewController(
		controllerContext.ClientBuilder.ClientOrDie("ephemeral-volume-controller"),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().PersistentVolumeClaims())
	if err != nil {
		return nil, true, fmt.Errorf("failed to start ephemeral volume controller: %v", err)
	}
	go ephemeralController.Run(ctx, int(controllerContext.ComponentConfig.EphemeralVolumeController.ConcurrentEphemeralVolumeSyncs))
	return nil, true, nil
}

const defaultResourceClaimControllerWorkers = 10

func newResourceClaimControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.ResourceClaimController,
		aliases:  []string{"resource-claim-controller"},
		initFunc: startResourceClaimController,
		requiredFeatureGates: []featuregate.Feature{
			features.DynamicResourceAllocation, // TODO update app.TestFeatureGatedControllersShouldNotDefineAliases when removing this feature
		},
	}
}

func startResourceClaimController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	ephemeralController, err := resourceclaim.NewController(
		klog.FromContext(ctx),
		controllerContext.ClientBuilder.ClientOrDie("resource-claim-controller"),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Resource().V1alpha2().PodSchedulingContexts(),
		controllerContext.InformerFactory.Resource().V1alpha2().ResourceClaims(),
		controllerContext.InformerFactory.Resource().V1alpha2().ResourceClaimTemplates())
	if err != nil {
		return nil, true, fmt.Errorf("failed to start resource claim controller: %v", err)
	}
	go ephemeralController.Run(ctx, defaultResourceClaimControllerWorkers)
	return nil, true, nil
}

func newEndpointsControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.EndpointsController,
		aliases:  []string{"endpoint"},
		initFunc: startEndpointsController,
	}
}

func startEndpointsController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go endpointcontroller.NewEndpointController(
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().Services(),
		controllerContext.InformerFactory.Core().V1().Endpoints(),
		controllerContext.ClientBuilder.ClientOrDie("endpoint-controller"),
		controllerContext.ComponentConfig.EndpointController.EndpointUpdatesBatchPeriod.Duration,
	).Run(ctx, int(controllerContext.ComponentConfig.EndpointController.ConcurrentEndpointSyncs))
	return nil, true, nil
}

func newReplicationControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.ReplicationControllerController,
		aliases:  []string{"replicationcontroller"},
		initFunc: startReplicationController,
	}
}

func startReplicationController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go replicationcontroller.NewReplicationManager(
		klog.FromContext(ctx),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().ReplicationControllers(),
		controllerContext.ClientBuilder.ClientOrDie("replication-controller"),
		replicationcontroller.BurstReplicas,
	).Run(ctx, int(controllerContext.ComponentConfig.ReplicationController.ConcurrentRCSyncs))
	return nil, true, nil
}

func newPodGarbageCollectorControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.PodGarbageCollectorController,
		aliases:  []string{"podgc"},
		initFunc: startPodGarbageCollectorController,
	}
}

func startPodGarbageCollectorController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go podgc.NewPodGC(
		ctx,
		controllerContext.ClientBuilder.ClientOrDie("pod-garbage-collector"),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.InformerFactory.Core().V1().Nodes(),
		int(controllerContext.ComponentConfig.PodGCController.TerminatedPodGCThreshold),
	).Run(ctx)
	return nil, true, nil
}

func newResourceQuotaControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.ResourceQuotaController,
		aliases:  []string{"resourcequota"},
		initFunc: startResourceQuotaController,
	}
}

func startResourceQuotaController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	resourceQuotaControllerClient := controllerContext.ClientBuilder.ClientOrDie("resourcequota-controller")
	resourceQuotaControllerDiscoveryClient := controllerContext.ClientBuilder.DiscoveryClientOrDie("resourcequota-controller")
	discoveryFunc := resourceQuotaControllerDiscoveryClient.ServerPreferredNamespacedResources
	listerFuncForResource := generic.ListerFuncForResourceFunc(controllerContext.InformerFactory.ForResource)
	quotaConfiguration := quotainstall.NewQuotaConfigurationForControllers(listerFuncForResource)

	resourceQuotaControllerOptions := &resourcequotacontroller.ControllerOptions{
		QuotaClient:               resourceQuotaControllerClient.CoreV1(),
		ResourceQuotaInformer:     controllerContext.InformerFactory.Core().V1().ResourceQuotas(),
		ResyncPeriod:              pkgcontroller.StaticResyncPeriodFunc(controllerContext.ComponentConfig.ResourceQuotaController.ResourceQuotaSyncPeriod.Duration),
		InformerFactory:           controllerContext.ObjectOrMetadataInformerFactory,
		ReplenishmentResyncPeriod: controllerContext.ResyncPeriod,
		DiscoveryFunc:             discoveryFunc,
		IgnoredResourcesFunc:      quotaConfiguration.IgnoredResources,
		InformersStarted:          controllerContext.InformersStarted,
		Registry:                  generic.NewRegistry(quotaConfiguration.Evaluators()),
		UpdateFilter:              quotainstall.DefaultUpdateFilter(),
	}
	resourceQuotaController, err := resourcequotacontroller.NewController(ctx, resourceQuotaControllerOptions)
	if err != nil {
		return nil, false, err
	}
	go resourceQuotaController.Run(ctx, int(controllerContext.ComponentConfig.ResourceQuotaController.ConcurrentResourceQuotaSyncs))

	// Periodically the quota controller to detect new resource types
	go resourceQuotaController.Sync(ctx, discoveryFunc, 30*time.Second)

	return nil, true, nil
}

func newNamespaceControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.NamespaceController,
		aliases:  []string{"namespace"},
		initFunc: startNamespaceController,
	}
}

func startNamespaceController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	// the namespace cleanup controller is very chatty.  It makes lots of discovery calls and then it makes lots of delete calls
	// the ratelimiter negatively affects its speed.  Deleting 100 total items in a namespace (that's only a few of each resource
	// including events), takes ~10 seconds by default.
	nsKubeconfig := controllerContext.ClientBuilder.ConfigOrDie("namespace-controller")
	nsKubeconfig.QPS *= 20
	nsKubeconfig.Burst *= 100
	namespaceKubeClient := clientset.NewForConfigOrDie(nsKubeconfig)
	return startModifiedNamespaceController(ctx, controllerContext, namespaceKubeClient, nsKubeconfig)
}

func startModifiedNamespaceController(ctx context.Context, controllerContext ControllerContext, namespaceKubeClient clientset.Interface, nsKubeconfig *restclient.Config) (controller.Interface, bool, error) {

	// gets a rest/http client from client-go to retrieve k8s object metadata
	metadataClient, err := metadata.NewForConfig(nsKubeconfig)
	if err != nil {
		return nil, true, err
	}

	// DiscoveryInterface holds the methods that discover server-supported API groups, versions and resources.
	// Discovery() retrieves the DiscoveryClient
	// Skipped: ServerPreferredResources uses the provided discovery interface to look up preferred resources
	discoverResourcesFn := namespaceKubeClient.Discovery().ServerPreferredNamespacedResources

	namespaceController := namespacecontroller.NewNamespaceController(
		ctx,
		namespaceKubeClient,
		metadataClient,
		discoverResourcesFn,
		controllerContext.InformerFactory.Core().V1().Namespaces(),
		controllerContext.ComponentConfig.NamespaceController.NamespaceSyncPeriod.Duration,
		v1.FinalizerKubernetes,
	)
	go namespaceController.Run(ctx, int(controllerContext.ComponentConfig.NamespaceController.ConcurrentNamespaceSyncs))

	return nil, true, nil
}

func newServiceAccountControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.ServiceAccountController,
		aliases:  []string{"serviceaccount"},
		initFunc: startServiceAccountController,
	}
}

func startServiceAccountController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	sac, err := serviceaccountcontroller.NewServiceAccountsController(
		controllerContext.InformerFactory.Core().V1().ServiceAccounts(),
		controllerContext.InformerFactory.Core().V1().Namespaces(),
		controllerContext.ClientBuilder.ClientOrDie("service-account-controller"),
		serviceaccountcontroller.DefaultServiceAccountsControllerOptions(),
	)
	if err != nil {
		return nil, true, fmt.Errorf("error creating ServiceAccount controller: %v", err)
	}
	go sac.Run(ctx, 1)
	return nil, true, nil
}

func newTTLControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.TTLController,
		aliases:  []string{"ttl"},
		initFunc: startTTLController,
	}
}

func startTTLController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go ttlcontroller.NewTTLController(
		ctx,
		controllerContext.InformerFactory.Core().V1().Nodes(),
		controllerContext.ClientBuilder.ClientOrDie("ttl-controller"),
	).Run(ctx, 5)
	return nil, true, nil
}

func newGarbageCollectorControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.GarbageCollectorController,
		aliases:  []string{"garbagecollector"},
		initFunc: startGarbageCollectorController,
	}
}

func startGarbageCollectorController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	if !controllerContext.ComponentConfig.GarbageCollectorController.EnableGarbageCollector {
		return nil, false, nil
	}

	gcClientset := controllerContext.ClientBuilder.ClientOrDie("generic-garbage-collector")
	discoveryClient := controllerContext.ClientBuilder.DiscoveryClientOrDie("generic-garbage-collector")

	config := controllerContext.ClientBuilder.ConfigOrDie("generic-garbage-collector")
	// Increase garbage collector controller's throughput: each object deletion takes two API calls,
	// so to get |config.QPS| deletion rate we need to allow 2x more requests for this controller.
	config.QPS *= 2
	metadataClient, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, true, err
	}

	ignoredResources := make(map[schema.GroupResource]struct{})
	for _, r := range controllerContext.ComponentConfig.GarbageCollectorController.GCIgnoredResources {
		ignoredResources[schema.GroupResource{Group: r.Group, Resource: r.Resource}] = struct{}{}
	}
	garbageCollector, err := garbagecollector.NewGarbageCollector(
		gcClientset,
		metadataClient,
		controllerContext.RESTMapper,
		ignoredResources,
		controllerContext.ObjectOrMetadataInformerFactory,
		controllerContext.InformersStarted,
	)
	if err != nil {
		return nil, true, fmt.Errorf("failed to start the generic garbage collector: %v", err)
	}

	// Start the garbage collector.
	workers := int(controllerContext.ComponentConfig.GarbageCollectorController.ConcurrentGCSyncs)
	go garbageCollector.Run(ctx, workers)

	// Periodically refresh the RESTMapper with new discovery information and sync
	// the garbage collector.
	go garbageCollector.Sync(ctx, discoveryClient, 30*time.Second)

	return garbageCollector, true, nil
}

func newPersistentVolumeClaimProtectionControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.PersistentVolumeClaimProtectionController,
		aliases:  []string{"pvc-protection"},
		initFunc: startPersistentVolumeClaimProtectionController,
	}
}

func startPersistentVolumeClaimProtectionController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	pvcProtectionController, err := pvcprotection.NewPVCProtectionController(
		klog.FromContext(ctx),
		controllerContext.InformerFactory.Core().V1().PersistentVolumeClaims(),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.ClientBuilder.ClientOrDie("pvc-protection-controller"),
	)
	if err != nil {
		return nil, true, fmt.Errorf("failed to start the pvc protection controller: %v", err)
	}
	go pvcProtectionController.Run(ctx, 1)
	return nil, true, nil
}

func newPersistentVolumeProtectionControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.PersistentVolumeProtectionController,
		aliases:  []string{"pv-protection"},
		initFunc: startPersistentVolumeProtectionController,
	}
}

func startPersistentVolumeProtectionController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go pvprotection.NewPVProtectionController(
		klog.FromContext(ctx),
		controllerContext.InformerFactory.Core().V1().PersistentVolumes(),
		controllerContext.ClientBuilder.ClientOrDie("pv-protection-controller"),
	).Run(ctx, 1)
	return nil, true, nil
}

func newTTLAfterFinishedControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.TTLAfterFinishedController,
		aliases:  []string{"ttl-after-finished"},
		initFunc: startTTLAfterFinishedController,
	}
}

func startTTLAfterFinishedController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go ttlafterfinished.New(
		ctx,
		controllerContext.InformerFactory.Batch().V1().Jobs(),
		controllerContext.ClientBuilder.ClientOrDie("ttl-after-finished-controller"),
	).Run(ctx, int(controllerContext.ComponentConfig.TTLAfterFinishedController.ConcurrentTTLSyncs))
	return nil, true, nil
}

func newLegacyServiceAccountTokenCleanerControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.LegacyServiceAccountTokenCleanerController,
		aliases:  []string{"legacy-service-account-token-cleaner"},
		initFunc: startLegacyServiceAccountTokenCleanerController,
		requiredFeatureGates: []featuregate.Feature{
			features.LegacyServiceAccountTokenCleanUp, // TODO update app.TestFeatureGatedControllersShouldNotDefineAliases when removing this feature
		},
	}
}

func startLegacyServiceAccountTokenCleanerController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	cleanUpPeriod := controllerContext.ComponentConfig.LegacySATokenCleaner.CleanUpPeriod.Duration
	legacySATokenCleaner, err := serviceaccountcontroller.NewLegacySATokenCleaner(
		controllerContext.InformerFactory.Core().V1().ServiceAccounts(),
		controllerContext.InformerFactory.Core().V1().Secrets(),
		controllerContext.InformerFactory.Core().V1().Pods(),
		controllerContext.ClientBuilder.ClientOrDie("legacy-service-account-token-cleaner"),
		clock.RealClock{},
		serviceaccountcontroller.LegacySATokenCleanerOptions{
			CleanUpPeriod: cleanUpPeriod,
			SyncInterval:  serviceaccountcontroller.DefaultCleanerSyncInterval,
		})
	if err != nil {
		return nil, true, fmt.Errorf("failed to start the legacy service account token cleaner: %v", err)
	}
	go legacySATokenCleaner.Run(ctx)
	return nil, true, nil
}

// processCIDRs is a helper function that works on a comma separated cidrs and returns
// a list of typed cidrs
// error if failed to parse any of the cidrs or invalid length of cidrs
func validateCIDRs(cidrsList string) ([]*net.IPNet, error) {
	// failure: bad cidrs in config
	clusterCIDRs, dualStack, err := processCIDRs(cidrsList)
	if err != nil {
		return nil, err
	}

	// failure: more than one cidr but they are not configured as dual stack
	if len(clusterCIDRs) > 1 && !dualStack {
		return nil, fmt.Errorf("len of ClusterCIDRs==%v and they are not configured as dual stack (at least one from each IPFamily", len(clusterCIDRs))
	}

	// failure: more than cidrs is not allowed even with dual stack
	if len(clusterCIDRs) > 2 {
		return nil, fmt.Errorf("length of clusterCIDRs is:%v more than max allowed of 2", len(clusterCIDRs))
	}

	return clusterCIDRs, nil
}

// processCIDRs is a helper function that works on a comma separated cidrs and returns
// a list of typed cidrs
// a flag if cidrs represents a dual stack
// error if failed to parse any of the cidrs
func processCIDRs(cidrsList string) ([]*net.IPNet, bool, error) {
	cidrsSplit := strings.Split(strings.TrimSpace(cidrsList), ",")

	cidrs, err := netutils.ParseCIDRs(cidrsSplit)
	if err != nil {
		return nil, false, err
	}

	// if cidrs has an error then the previous call will fail
	// safe to ignore error checking on next call
	dualstack, _ := netutils.IsDualStackCIDRs(cidrs)

	return cidrs, dualstack, nil
}

// setNodeCIDRMaskSizes returns the IPv4 and IPv6 node cidr mask sizes to the value provided
// for --node-cidr-mask-size-ipv4 and --node-cidr-mask-size-ipv6 respectively. If value not provided,
// then it will return default IPv4 and IPv6 cidr mask sizes.
func setNodeCIDRMaskSizes(cfg nodeipamconfig.NodeIPAMControllerConfiguration, clusterCIDRs []*net.IPNet) ([]int, error) {

	sortedSizes := func(maskSizeIPv4, maskSizeIPv6 int) []int {
		nodeMaskCIDRs := make([]int, len(clusterCIDRs))

		for idx, clusterCIDR := range clusterCIDRs {
			if netutils.IsIPv6CIDR(clusterCIDR) {
				nodeMaskCIDRs[idx] = maskSizeIPv6
			} else {
				nodeMaskCIDRs[idx] = maskSizeIPv4
			}
		}
		return nodeMaskCIDRs
	}

	// --node-cidr-mask-size flag is incompatible with dual stack clusters.
	ipv4Mask, ipv6Mask := defaultNodeMaskCIDRIPv4, defaultNodeMaskCIDRIPv6
	isDualstack := len(clusterCIDRs) > 1

	// case one: cluster is dualstack (i.e, more than one cidr)
	if isDualstack {
		// if --node-cidr-mask-size then fail, user must configure the correct dual-stack mask sizes (or use default)
		if cfg.NodeCIDRMaskSize != 0 {
			return nil, errors.New("usage of --node-cidr-mask-size is not allowed with dual-stack clusters")

		}

		if cfg.NodeCIDRMaskSizeIPv4 != 0 {
			ipv4Mask = int(cfg.NodeCIDRMaskSizeIPv4)
		}
		if cfg.NodeCIDRMaskSizeIPv6 != 0 {
			ipv6Mask = int(cfg.NodeCIDRMaskSizeIPv6)
		}
		return sortedSizes(ipv4Mask, ipv6Mask), nil
	}

	maskConfigured := cfg.NodeCIDRMaskSize != 0
	maskV4Configured := cfg.NodeCIDRMaskSizeIPv4 != 0
	maskV6Configured := cfg.NodeCIDRMaskSizeIPv6 != 0
	isSingleStackIPv6 := netutils.IsIPv6CIDR(clusterCIDRs[0])

	// original flag is set
	if maskConfigured {
		// original mask flag is still the main reference.
		if maskV4Configured || maskV6Configured {
			return nil, errors.New("usage of --node-cidr-mask-size-ipv4 and --node-cidr-mask-size-ipv6 is not allowed if --node-cidr-mask-size is set. For dual-stack clusters please unset it and use IPFamily specific flags")
		}

		mask := int(cfg.NodeCIDRMaskSize)
		return sortedSizes(mask, mask), nil
	}

	if maskV4Configured {
		if isSingleStackIPv6 {
			return nil, errors.New("usage of --node-cidr-mask-size-ipv4 is not allowed for a single-stack IPv6 cluster")
		}

		ipv4Mask = int(cfg.NodeCIDRMaskSizeIPv4)
	}

	// !maskV4Configured && !maskConfigured && maskV6Configured
	if maskV6Configured {
		if !isSingleStackIPv6 {
			return nil, errors.New("usage of --node-cidr-mask-size-ipv6 is not allowed for a single-stack IPv4 cluster")
		}

		ipv6Mask = int(cfg.NodeCIDRMaskSizeIPv6)
	}
	return sortedSizes(ipv4Mask, ipv6Mask), nil
}

func newStorageVersionGarbageCollectorControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:     names.StorageVersionGarbageCollectorController,
		aliases:  []string{"storage-version-gc"},
		initFunc: startStorageVersionGarbageCollectorController,
		requiredFeatureGates: []featuregate.Feature{
			genericfeatures.APIServerIdentity,
			genericfeatures.StorageVersionAPI,
		},
	}
}

func startStorageVersionGarbageCollectorController(ctx context.Context, controllerContext ControllerContext, controllerName string) (controller.Interface, bool, error) {
	go storageversiongc.NewStorageVersionGC(
		ctx,
		controllerContext.ClientBuilder.ClientOrDie("storage-version-garbage-collector"),
		controllerContext.InformerFactory.Coordination().V1().Leases(),
		controllerContext.InformerFactory.Internal().V1alpha1().StorageVersions(),
	).Run(ctx)
	return nil, true, nil
}
