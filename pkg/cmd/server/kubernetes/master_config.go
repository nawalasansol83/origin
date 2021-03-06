package kubernetes

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

	etcdclient "github.com/coreos/go-etcd/etcd"

	"k8s.io/kubernetes/cmd/kube-apiserver/app"
	cmapp "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	"k8s.io/kubernetes/pkg/admission"
	kapi "k8s.io/kubernetes/pkg/api"
	kapilatest "k8s.io/kubernetes/pkg/api/latest"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apiserver"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/cloudprovider"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/master"
	"k8s.io/kubernetes/pkg/storage"
	etcdstorage "k8s.io/kubernetes/pkg/storage/etcd"
	"k8s.io/kubernetes/pkg/util"
	kerrors "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/intstr"
	saadmit "k8s.io/kubernetes/plugin/pkg/admission/serviceaccount"

	"github.com/openshift/origin/pkg/cmd/flagtypes"
	oadmission "github.com/openshift/origin/pkg/cmd/server/admission"
	configapi "github.com/openshift/origin/pkg/cmd/server/api"
	"github.com/openshift/origin/pkg/cmd/server/etcd"
	cmdflags "github.com/openshift/origin/pkg/cmd/util/flags"
	"github.com/openshift/origin/pkg/cmd/util/pluginconfig"
	projectcache "github.com/openshift/origin/pkg/project/cache"
)

// AdmissionPlugins is the full list of admission control plugins to enable in the order they must run
var AdmissionPlugins = []string{"NamespaceLifecycle", "OriginPodNodeEnvironment", "LimitRanger", "ServiceAccount", "SecurityContextConstraint", "ResourceQuota", "SCCExecRestrictions"}

// MasterConfig defines the required values to start a Kubernetes master
type MasterConfig struct {
	Options    configapi.KubernetesMasterConfig
	KubeClient *kclient.Client

	Master            *master.Config
	ControllerManager *cmapp.CMServer
	CloudProvider     cloudprovider.Interface
}

func BuildKubernetesMasterConfig(options configapi.MasterConfig, requestContextMapper kapi.RequestContextMapper, kubeClient *kclient.Client, projectCache *projectcache.ProjectCache) (*MasterConfig, error) {
	if options.KubernetesMasterConfig == nil {
		return nil, errors.New("insufficient information to build KubernetesMasterConfig")
	}

	// Connect and setup etcd interfaces
	etcdClient, err := etcd.EtcdClient(options.EtcdClientInfo)
	if err != nil {
		return nil, err
	}

	kubeletClientConfig := configapi.GetKubeletClientConfig(options)
	kubeletClient, err := kubeletclient.NewStaticKubeletClient(kubeletClientConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to configure Kubelet client: %v", err)
	}

	// in-order list of plug-ins that should intercept admission decisions
	// TODO: Push node environment support to upstream in future

	_, portString, err := net.SplitHostPort(options.ServingInfo.BindAddress)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		return nil, err
	}

	portRange, err := util.ParsePortRange(options.KubernetesMasterConfig.ServicesNodePortRange)
	if err != nil {
		return nil, err
	}

	podEvictionTimeout, err := time.ParseDuration(options.KubernetesMasterConfig.PodEvictionTimeout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse PodEvictionTimeout: %v", err)
	}

	server := app.NewAPIServer()
	server.EventTTL = 2 * time.Hour
	server.ServiceClusterIPRange = net.IPNet(flagtypes.DefaultIPNet(options.KubernetesMasterConfig.ServicesSubnet))
	server.ServiceNodePortRange = *portRange
	admissionPlugins := AdmissionPlugins
	server.AdmissionControl = strings.Join(admissionPlugins, ",")

	// resolve extended arguments
	// TODO: this should be done in config validation (along with the above) so we can provide
	// proper errors
	if err := cmdflags.Resolve(options.KubernetesMasterConfig.APIServerArguments, server.AddFlags); len(err) > 0 {
		return nil, kerrors.NewAggregate(err)
	}

	if len(options.KubernetesMasterConfig.AdmissionConfig.PluginOrderOverride) > 0 {
		admissionPlugins := options.KubernetesMasterConfig.AdmissionConfig.PluginOrderOverride
		server.AdmissionControl = strings.Join(admissionPlugins, ",")
	}

	cmserver := cmapp.NewCMServer()
	cmserver.PodEvictionTimeout = podEvictionTimeout
	// resolve extended arguments
	// TODO: this should be done in config validation (along with the above) so we can provide
	// proper errors
	if err := cmdflags.Resolve(options.KubernetesMasterConfig.ControllerArguments, cmserver.AddFlags); len(err) > 0 {
		return nil, kerrors.NewAggregate(err)
	}

	cloud, err := cloudprovider.InitCloudProvider(cmserver.CloudProvider, cmserver.CloudConfigFile)
	if err != nil {
		return nil, err
	}
	if cloud != nil {
		glog.V(2).Infof("Successfully initialized cloud provider: %q from the config file: %q\n", server.CloudProvider, server.CloudConfigFile)
	}

	// This is a placeholder to provide additional initialization
	// objects to plugins
	pluginInitializer := oadmission.PluginInitializer{
		ProjectCache: projectCache,
	}

	plugins := []admission.Interface{}
	for _, pluginName := range strings.Split(server.AdmissionControl, ",") {
		switch pluginName {
		case saadmit.PluginName:
			// we need to set some custom parameters on the service account admission controller, so create that one by hand
			saAdmitter := saadmit.NewServiceAccount(kubeClient)
			saAdmitter.LimitSecretReferences = options.ServiceAccountConfig.LimitSecretReferences
			saAdmitter.Run()
			plugins = append(plugins, saAdmitter)

		default:
			configFile := server.AdmissionControlConfigFile
			pluginConfig := options.KubernetesMasterConfig.AdmissionConfig.PluginConfig

			// Check whether a config is specified for this plugin. If not, default to the
			// global plugin config file specifiedd in the server config.
			if cfg, hasConfig := pluginConfig[pluginName]; hasConfig {
				configFile, err = pluginconfig.GetPluginConfig(cfg)
				if err != nil {
					return nil, err
				}
			}
			plugin := admission.InitPlugin(pluginName, kubeClient, configFile)
			if plugin != nil {
				plugins = append(plugins, plugin)
			}

		}
	}
	pluginInitializer.Initialize(plugins)
	// ensure that plugins have been properly initialized
	if err := oadmission.Validate(plugins); err != nil {
		return nil, err
	}
	admissionController := admission.NewChainHandler(plugins...)

	var proxyClientCerts []tls.Certificate
	if len(options.KubernetesMasterConfig.ProxyClientInfo.CertFile) > 0 {
		clientCert, err := tls.LoadX509KeyPair(
			options.KubernetesMasterConfig.ProxyClientInfo.CertFile,
			options.KubernetesMasterConfig.ProxyClientInfo.KeyFile,
		)
		if err != nil {
			return nil, err
		}
		proxyClientCerts = append(proxyClientCerts, clientCert)
	}

	// TODO you have to know every APIGroup you're enabling or upstream will panic.  It's alternative to panicing is Fataling
	// It needs a refactor to return errors
	storageDestinations := master.NewStorageDestinations()
	// storageVersions is a map from API group to allowed versions that must be a version exposed by the REST API or it breaks.
	// We need to fix the upstream to stop using the storage version as a preferred api version.
	storageVersions := map[string]string{}

	enabledKubeVersions := configapi.GetEnabledAPIVersionsForGroup(*options.KubernetesMasterConfig, configapi.APIGroupKube)
	if len(enabledKubeVersions) > 0 {
		kubeStorageVersion := unversioned.GroupVersion{Group: configapi.APIGroupKube, Version: options.EtcdStorageConfig.KubernetesStorageVersion}
		databaseStorage, err := NewEtcdStorage(etcdClient, kubeStorageVersion, options.EtcdStorageConfig.KubernetesStoragePrefix)
		if err != nil {
			return nil, fmt.Errorf("Error setting up Kubernetes server storage: %v", err)
		}
		storageDestinations.AddAPIGroup(configapi.APIGroupKube, databaseStorage)
		storageVersions[configapi.APIGroupKube] = options.EtcdStorageConfig.KubernetesStorageVersion
	}

	enabledExtensionsVersions := configapi.GetEnabledAPIVersionsForGroup(*options.KubernetesMasterConfig, configapi.APIGroupExtensions)
	if len(enabledExtensionsVersions) > 0 {
		groupMeta, err := kapilatest.Group(configapi.APIGroupExtensions)
		if err != nil {
			return nil, fmt.Errorf("Error setting up Kubernetes extensions server storage: %v", err)
		}
		// TODO expose storage version options for api groups
		databaseStorage, err := NewEtcdStorage(etcdClient, groupMeta.GroupVersion, options.EtcdStorageConfig.KubernetesStoragePrefix)
		if err != nil {
			return nil, fmt.Errorf("Error setting up Kubernetes extensions server storage: %v", err)
		}
		storageDestinations.AddAPIGroup(configapi.APIGroupExtensions, databaseStorage)
		storageVersions[configapi.APIGroupExtensions] = enabledExtensionsVersions[0]
	}

	// Preserve previous behavior of using the first non-loopback address
	// TODO: Deprecate this behavior and just require a valid value to be passed in
	publicAddress := net.ParseIP(options.KubernetesMasterConfig.MasterIP)
	if publicAddress == nil || publicAddress.IsUnspecified() || publicAddress.IsLoopback() {
		hostIP, err := util.ChooseHostInterface()
		if err != nil {
			glog.Fatalf("Unable to find suitable network address.error='%v'. Set the masterIP directly to avoid this error.", err)
		}
		publicAddress = hostIP
		glog.Infof("Will report %v as public IP address.", publicAddress)
	}

	m := &master.Config{
		PublicAddress: publicAddress,
		ReadWritePort: port,

		StorageDestinations: storageDestinations,
		StorageVersions:     storageVersions,

		EventTTL: server.EventTTL,
		//MinRequestTimeout: server.MinRequestTimeout,

		ServiceClusterIPRange: (*net.IPNet)(&server.ServiceClusterIPRange),
		ServiceNodePortRange:  server.ServiceNodePortRange,

		RequestContextMapper: requestContextMapper,

		KubeletClient:  kubeletClient,
		APIPrefix:      KubeAPIPrefix,
		APIGroupPrefix: KubeAPIGroupPrefix,

		EnableCoreControllers: true,

		MasterCount: options.KubernetesMasterConfig.MasterCount,

		Authorizer:       apiserver.NewAlwaysAllowAuthorizer(),
		AdmissionControl: admissionController,

		APIGroupVersionOverrides: getAPIGroupVersionOverrides(options),

		// Set the TLS options for proxying to pods and services
		// Proxying to nodes uses the kubeletClient TLS config (so can provide a different cert, and verify the node hostname)
		ProxyTLSClientConfig: &tls.Config{
			// Proxying to pods and services cannot verify hostnames, since they are contacted on randomly allocated IPs
			InsecureSkipVerify: true,
			Certificates:       proxyClientCerts,
		},
	}

	if options.DNSConfig != nil {
		_, dnsPortStr, err := net.SplitHostPort(options.DNSConfig.BindAddress)
		if err != nil {
			return nil, fmt.Errorf("unable to parse DNS bind address %s: %v", options.DNSConfig.BindAddress, err)
		}
		dnsPort, err := strconv.Atoi(dnsPortStr)
		if err != nil {
			return nil, fmt.Errorf("invalid DNS port: %v", err)
		}
		m.ExtraServicePorts = append(m.ExtraServicePorts,
			kapi.ServicePort{Name: "dns", Port: 53, Protocol: kapi.ProtocolUDP, TargetPort: intstr.FromInt(dnsPort)},
			kapi.ServicePort{Name: "dns-tcp", Port: 53, Protocol: kapi.ProtocolTCP, TargetPort: intstr.FromInt(dnsPort)},
		)
		m.ExtraEndpointPorts = append(m.ExtraEndpointPorts,
			kapi.EndpointPort{Name: "dns", Port: dnsPort, Protocol: kapi.ProtocolUDP},
			kapi.EndpointPort{Name: "dns-tcp", Port: dnsPort, Protocol: kapi.ProtocolTCP},
		)
	}

	kmaster := &MasterConfig{
		Options:    *options.KubernetesMasterConfig,
		KubeClient: kubeClient,

		Master:            m,
		ControllerManager: cmserver,
		CloudProvider:     cloud,
	}

	return kmaster, nil
}

// getAPIGroupVersionOverrides builds the overrides in the format expected by master.Config.APIGroupVersionOverrides
func getAPIGroupVersionOverrides(options configapi.MasterConfig) map[string]master.APIGroupVersionOverride {
	apiGroupVersionOverrides := map[string]master.APIGroupVersionOverride{}
	for group := range options.KubernetesMasterConfig.DisabledAPIGroupVersions {
		for _, version := range configapi.GetDisabledAPIVersionsForGroup(*options.KubernetesMasterConfig, group) {
			gv := unversioned.GroupVersion{Group: group, Version: version}
			if group == "" {
				// TODO: when rebasing, check the parseRuntimeConfig impl to make sure we're still building the right magic container
				// Create "disabled" key for v1 identically to k8s.io/kubernetes/cmd/kube-apiserver/app/server.go#parseRuntimeConfig
				gv.Group = "api"
			}
			apiGroupVersionOverrides[gv.String()] = master.APIGroupVersionOverride{Disable: true}
		}
	}
	return apiGroupVersionOverrides
}

// NewEtcdStorage returns a storage interface for the provided storage version.
func NewEtcdStorage(client *etcdclient.Client, version unversioned.GroupVersion, prefix string) (helper storage.Interface, err error) {
	group, err := kapilatest.Group(version.Group)
	if err != nil {
		return nil, err
	}
	interfaces, err := group.InterfacesFor(version)
	if err != nil {
		return nil, err
	}
	return etcdstorage.NewEtcdStorage(client, interfaces.Codec, prefix), nil
}
