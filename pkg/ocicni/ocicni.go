package ocicni

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/containernetworking/cni/libcni"
	cniinvoke "github.com/containernetworking/cni/pkg/invoke"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	cnicurrent "github.com/containernetworking/cni/pkg/types/current"
	cniversion "github.com/containernetworking/cni/pkg/version"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
)

type cniNetworkPlugin struct {
	cniConfig *libcni.CNIConfig
	loNetwork *cniNetwork

	sync.RWMutex
	defaultNetName string
	networks       map[string]*cniNetwork

	nsManager *nsManager
	confDir   string
	binDirs   []string

	shutdownChan chan struct{}
	watcher      *fsnotify.Watcher
	done         *sync.WaitGroup

	// The pod map provides synchronization for a given pod's network
	// operations.  Each pod's setup/teardown/status operations
	// are synchronized against each other, but network operations of other
	// pods can proceed in parallel.
	podsLock sync.Mutex
	pods     map[string]*podLock

	// For testcases
	exec     cniinvoke.Exec
	cacheDir string
}

type cniNetwork struct {
	name     string
	filePath string
	config   *libcni.NetworkConfigList
}

var errMissingDefaultNetwork = errors.New("Missing CNI default network")

type podLock struct {
	// Count of in-flight operations for this pod; when this reaches zero
	// the lock can be removed from the pod map
	refcount uint

	// Lock to synchronize operations for this specific pod
	mu sync.Mutex
}

func buildFullPodName(podNetwork PodNetwork) string {
	return podNetwork.Namespace + "_" + podNetwork.Name
}

// Lock network operations for a specific pod.  If that pod is not yet in
// the pod map, it will be added.  The reference count for the pod will
// be increased.
func (plugin *cniNetworkPlugin) podLock(podNetwork PodNetwork) *sync.Mutex {
	plugin.podsLock.Lock()
	defer plugin.podsLock.Unlock()

	fullPodName := buildFullPodName(podNetwork)
	lock, ok := plugin.pods[fullPodName]
	if !ok {
		lock = &podLock{}
		plugin.pods[fullPodName] = lock
	}
	lock.refcount++
	return &lock.mu
}

// Unlock network operations for a specific pod.  The reference count for the
// pod will be decreased.  If the reference count reaches zero, the pod will be
// removed from the pod map.
func (plugin *cniNetworkPlugin) podUnlock(podNetwork PodNetwork) {
	plugin.podsLock.Lock()
	defer plugin.podsLock.Unlock()

	fullPodName := buildFullPodName(podNetwork)
	lock, ok := plugin.pods[fullPodName]
	if !ok {
		logrus.Warningf("Unbalanced pod lock unref for %s", fullPodName)
		return
	} else if lock.refcount == 0 {
		// This should never ever happen, but handle it anyway
		delete(plugin.pods, fullPodName)
		logrus.Errorf("Pod lock for %s still in map with zero refcount", fullPodName)
		return
	}
	lock.refcount--
	lock.mu.Unlock()
	if lock.refcount == 0 {
		delete(plugin.pods, fullPodName)
	}
}

func newWatcher(confDir string) (*fsnotify.Watcher, error) {
	// Ensure plugin directory exists, because the following monitoring logic
	// relies on that.
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create %q: %v", confDir, err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("could not create new watcher %v", err)
	}
	defer func() {
		// Close watcher on error
		if err != nil {
			watcher.Close()
		}
	}()

	if err = watcher.Add(confDir); err != nil {
		return nil, fmt.Errorf("failed to add watch on %q: %v", confDir, err)
	}

	return watcher, nil
}

func (plugin *cniNetworkPlugin) monitorConfDir(start *sync.WaitGroup) {
	start.Done()
	plugin.done.Add(1)
	defer plugin.done.Done()
	for {
		select {
		case event := <-plugin.watcher.Events:
			logrus.Warningf("CNI monitoring event %v", event)

			var defaultDeleted bool
			createWrite := (event.Op&fsnotify.Create == fsnotify.Create ||
				event.Op&fsnotify.Write == fsnotify.Write)
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				// Care about the event if the default network
				// was just deleted
				defNet := plugin.getDefaultNetwork()
				if defNet != nil && event.Name == defNet.filePath {
					defaultDeleted = true
				}

			}
			if !createWrite && !defaultDeleted {
				continue
			}

			if err := plugin.syncNetworkConfig(); err != nil {
				logrus.Errorf("CNI config loading failed, continue monitoring: %v", err)
				continue
			}

		case err := <-plugin.watcher.Errors:
			if err == nil {
				continue
			}
			logrus.Errorf("CNI monitoring error %v", err)
			return

		case <-plugin.shutdownChan:
			return
		}
	}
}

// InitCNI takes a binary directory in which to search for CNI plugins, and
// a configuration directory in which to search for CNI JSON config files.
// If no valid CNI configs exist, network requests will fail until valid CNI
// config files are present in the config directory.
// If defaultNetName is not empty, a CNI config with that network name will
// be used as the default CNI network, and container network operations will
// fail until that network config is present and valid.
func InitCNI(defaultNetName string, confDir string, binDirs ...string) (CNIPlugin, error) {
	return initCNI(nil, "", defaultNetName, confDir, binDirs...)
}

// Internal function to allow faking out exec functions for testing
func initCNI(exec cniinvoke.Exec, cacheDir, defaultNetName string, confDir string, binDirs ...string) (CNIPlugin, error) {
	if confDir == "" {
		confDir = DefaultConfDir
	}
	if len(binDirs) == 0 {
		binDirs = []string{DefaultBinDir}
	}
	plugin := &cniNetworkPlugin{
		cniConfig:      libcni.NewCNIConfig(binDirs, exec),
		defaultNetName: defaultNetName,
		networks:       make(map[string]*cniNetwork),
		loNetwork:      getLoNetwork(),
		confDir:        confDir,
		binDirs:        binDirs,
		shutdownChan:   make(chan struct{}),
		done:           &sync.WaitGroup{},
		pods:           make(map[string]*podLock),
		exec:           exec,
		cacheDir:       cacheDir,
	}

	if exec == nil {
		exec = &cniinvoke.DefaultExec{
			RawExec:       &cniinvoke.RawExec{Stderr: os.Stderr},
			PluginDecoder: cniversion.PluginDecoder{},
		}
	}

	nsm, err := newNSManager()
	if err != nil {
		return nil, err
	}
	plugin.nsManager = nsm

	plugin.syncNetworkConfig()

	plugin.watcher, err = newWatcher(plugin.confDir)
	if err != nil {
		return nil, err
	}

	startWg := sync.WaitGroup{}
	startWg.Add(1)
	go plugin.monitorConfDir(&startWg)
	startWg.Wait()

	return plugin, nil
}

func (plugin *cniNetworkPlugin) Shutdown() error {
	close(plugin.shutdownChan)
	plugin.watcher.Close()
	plugin.done.Wait()
	return nil
}

func loadNetworks(confDir string, cni *libcni.CNIConfig) (map[string]*cniNetwork, string, error) {
	files, err := libcni.ConfFiles(confDir, []string{".conf", ".conflist", ".json"})
	if err != nil {
		return nil, "", err
	}

	networks := make(map[string]*cniNetwork)
	defaultNetName := ""

	sort.Strings(files)
	for _, confFile := range files {
		var confList *libcni.NetworkConfigList
		if strings.HasSuffix(confFile, ".conflist") {
			confList, err = libcni.ConfListFromFile(confFile)
			if err != nil {
				logrus.Warningf("Error loading CNI config list file %s: %v", confFile, err)
				continue
			}
		} else {
			conf, err := libcni.ConfFromFile(confFile)
			if err != nil {
				logrus.Warningf("Error loading CNI config file %s: %v", confFile, err)
				continue
			}
			if conf.Network.Type == "" {
				logrus.Warningf("Error loading CNI config file %s: no 'type'; perhaps this is a .conflist?", confFile)
				continue
			}
			confList, err = libcni.ConfListFromConf(conf)
			if err != nil {
				logrus.Warningf("Error converting CNI config file %s to list: %v", confFile, err)
				continue
			}
		}
		if len(confList.Plugins) == 0 {
			logrus.Warningf("CNI config list %s has no networks, skipping", confFile)
			continue
		}

		// Validation on CNI config should be done to pre-check presence
		// of plugins which are necessary.
		if _, err := cni.ValidateNetworkList(context.TODO(), confList); err != nil {
			logrus.Warningf("Error validating CNI config file %s: %v", confFile, err)
			continue
		}

		if confList.Name == "" {
			confList.Name = path.Base(confFile)
		}

		cniNet := &cniNetwork{
			name:     confList.Name,
			filePath: confFile,
			config:   confList,
		}

		logrus.Infof("Found CNI network %s (type=%v) at %s", confList.Name, confList.Plugins[0].Network.Type, confFile)

		networks[confList.Name] = cniNet

		if defaultNetName == "" {
			defaultNetName = confList.Name
		}
	}

	return networks, defaultNetName, nil
}

func getLoNetwork() *cniNetwork {
	loConfig, err := libcni.ConfListFromBytes([]byte(`{
  "cniVersion": "0.3.1",
  "name": "cni-loopback",
  "plugins": [{
    "type": "loopback"
  }]
}`))
	if err != nil {
		// The hardcoded config above should always be valid and unit tests will
		// catch this
		panic(err)
	}
	loNetwork := &cniNetwork{
		name:   "lo",
		config: loConfig,
	}

	return loNetwork
}

func (plugin *cniNetworkPlugin) syncNetworkConfig() error {
	networks, defaultNetName, err := loadNetworks(plugin.confDir, plugin.cniConfig)
	if err != nil {
		return err
	}

	plugin.Lock()
	defer plugin.Unlock()
	if plugin.defaultNetName == "" {
		plugin.defaultNetName = defaultNetName
	}
	plugin.networks = networks

	return nil
}

func (plugin *cniNetworkPlugin) getNetwork(name string) (*cniNetwork, error) {
	plugin.RLock()
	defer plugin.RUnlock()
	net, ok := plugin.networks[name]
	if !ok {
		return nil, fmt.Errorf("CNI network %q not found", name)
	}
	return net, nil
}

func (plugin *cniNetworkPlugin) GetDefaultNetworkName() string {
	plugin.RLock()
	defer plugin.RUnlock()
	return plugin.defaultNetName
}

func (plugin *cniNetworkPlugin) getDefaultNetwork() *cniNetwork {
	defaultNetName := plugin.GetDefaultNetworkName()
	if defaultNetName == "" {
		return nil
	}
	network, _ := plugin.getNetwork(defaultNetName)
	return network
}

// networksAvailable returns an error if the pod requests no networks and the
// plugin has no default network, and thus the plugin has no idea what network
// to attach the pod to.
func (plugin *cniNetworkPlugin) networksAvailable(podNetwork *PodNetwork) error {
	if len(podNetwork.Networks) == 0 && plugin.getDefaultNetwork() == nil {
		return errMissingDefaultNetwork
	}
	return nil
}

func (plugin *cniNetworkPlugin) Name() string {
	return CNIPluginName
}

func (plugin *cniNetworkPlugin) forEachNetwork(podNetwork *PodNetwork, actionFn func(*cniNetwork, *PodNetwork, *libcni.RuntimeConf) error) error {
	networks := podNetwork.Networks
	if len(networks) == 0 {
		networks = append(networks, NetAttachment{
			Name: plugin.GetDefaultNetworkName(),
		})
	}

	allIfNames := make(map[string]bool)
	for _, req := range networks {
		if req.Ifname != "" {
			// Make sure the requested name isn't already assigned
			if allIfNames[req.Ifname] {
				return fmt.Errorf("network %q requested interface name %q already assigned", req.Name, req.Ifname)
			}
			allIfNames[req.Ifname] = true
		}
	}

	for _, network := range networks {
		ifName := network.Ifname
		if ifName == "" {
			for i := 0; i < 10000; i++ {
				candidate := fmt.Sprintf("eth%d", i)
				if !allIfNames[candidate] {
					allIfNames[candidate] = true
					ifName = candidate
					break
				}
			}
			if ifName == "" {
				return fmt.Errorf("failed to find free interface name for network %q", network.Name)
			}
		}

		rt, err := buildCNIRuntimeConf(plugin.cacheDir, podNetwork, ifName, podNetwork.RuntimeConfig[network.Name])
		if err != nil {
			logrus.Errorf("error building CNI runtime config: %v", err)
			return err
		}

		cniNet, err := plugin.getNetwork(network.Name)
		if err != nil {
			logrus.Errorf(err.Error())
			return err
		}

		if err := actionFn(cniNet, podNetwork, rt); err != nil {
			return err
		}
	}
	return nil
}

func buildLoopbackRuntimeConf(cacheDir string, podNetwork *PodNetwork) *libcni.RuntimeConf {
	return &libcni.RuntimeConf{
		ContainerID: podNetwork.ID,
		NetNS:       podNetwork.NetNS,
		CacheDir:    cacheDir,
		IfName:      "lo",
	}
}

func (plugin *cniNetworkPlugin) SetUpPod(podNetwork PodNetwork) ([]NetResult, error) {
	if err := plugin.networksAvailable(&podNetwork); err != nil {
		return nil, err
	}

	plugin.podLock(podNetwork).Lock()
	defer plugin.podUnlock(podNetwork)

	loRt := buildLoopbackRuntimeConf(plugin.cacheDir, &podNetwork)
	if _, err := plugin.loNetwork.addToNetwork(loRt, plugin.cniConfig); err != nil {
		logrus.Errorf("Error while adding to cni lo network: %s", err)
		return nil, err
	}

	results := make([]NetResult, 0)
	if err := plugin.forEachNetwork(&podNetwork, func(network *cniNetwork, podNetwork *PodNetwork, rt *libcni.RuntimeConf) error {
		result, err := network.addToNetwork(rt, plugin.cniConfig)
		if err != nil {
			logrus.Errorf("Error while adding pod to CNI network %q: %s", network.name, err)
			return err
		}
		results = append(results, NetResult{
			Result: result,
			NetAttachment: NetAttachment{
				Name:   network.name,
				Ifname: rt.IfName,
			},
		})
		return nil
	}); err != nil {
		return nil, err
	}

	return results, nil
}

func (plugin *cniNetworkPlugin) TearDownPod(podNetwork PodNetwork) error {
	if err := plugin.networksAvailable(&podNetwork); err != nil {
		return err
	}

	plugin.podLock(podNetwork).Lock()
	defer plugin.podUnlock(podNetwork)

	return plugin.forEachNetwork(&podNetwork, func(network *cniNetwork, podNetwork *PodNetwork, rt *libcni.RuntimeConf) error {
		if err := network.deleteFromNetwork(rt, plugin.cniConfig); err != nil {
			logrus.Errorf("Error while removing pod from CNI network %q: %s", network.name, err)
			return err
		}
		return nil
	})
}

// GetPodNetworkStatus returns IP addressing and interface details for all
// networks attached to the pod.
func (plugin *cniNetworkPlugin) GetPodNetworkStatus(podNetwork PodNetwork) ([]NetResult, error) {
	plugin.podLock(podNetwork).Lock()
	defer plugin.podUnlock(podNetwork)

	results := make([]NetResult, 0)
	if err := plugin.forEachNetwork(&podNetwork, func(network *cniNetwork, podNetwork *PodNetwork, rt *libcni.RuntimeConf) error {
		result, err := network.checkNetwork(rt, plugin.cniConfig, plugin.nsManager, podNetwork.NetNS)
		if err != nil {
			logrus.Errorf("Error while checking pod to CNI network %q: %s", network.name, err)
			return err
		}
		if result != nil {
			results = append(results, NetResult{
				Result: result,
				NetAttachment: NetAttachment{
					Name:   network.name,
					Ifname: rt.IfName,
				},
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return results, nil
}

func (network *cniNetwork) addToNetwork(rt *libcni.RuntimeConf, cni *libcni.CNIConfig) (cnitypes.Result, error) {
	logrus.Infof("About to add CNI network %s (type=%v)", network.name, network.config.Plugins[0].Network.Type)
	res, err := cni.AddNetworkList(context.Background(), network.config, rt)
	if err != nil {
		logrus.Errorf("Error adding network: %v", err)
		return nil, err
	}

	return res, nil
}

func (network *cniNetwork) checkNetwork(rt *libcni.RuntimeConf, cni *libcni.CNIConfig, nsManager *nsManager, netns string) (cnitypes.Result, error) {
	logrus.Infof("About to check CNI network %s (type=%v)", network.name, network.config.Plugins[0].Network.Type)

	gtet, err := cniversion.GreaterThanOrEqualTo(network.config.CNIVersion, "0.4.0")
	if err != nil {
		return nil, err
	}

	var result cnitypes.Result

	// When CNIVersion supports Check, use it.  Otherwise fall back on what was done initially.
	if gtet {
		err = cni.CheckNetworkList(context.Background(), network.config, rt)
		logrus.Infof("Checking CNI network %s (config version=%v)", network.name, network.config.CNIVersion)
		if err != nil {
			logrus.Errorf("Error checking network: %v", err)
			return nil, err
		}
	}

	result, err = cni.GetNetworkListCachedResult(network.config, rt)
	if err != nil {
		logrus.Errorf("Error GetNetworkListCachedResult: %v", err)
		return nil, err
	} else if result != nil {
		return result, nil
	}

	// result doesn't exist, create one
	logrus.Infof("Checking CNI network %s (config version=%v) nsManager=%v", network.name, network.config.CNIVersion, nsManager)

	var cniInterface *cnicurrent.Interface
	ips := []*cnicurrent.IPConfig{}
	errs := []error{}
	for _, version := range []string{"4", "6"} {
		ip, mac, err := getContainerDetails(nsManager, netns, rt.IfName, "-"+version)
		if err == nil {
			if cniInterface == nil {
				cniInterface = &cnicurrent.Interface{
					Name:    rt.IfName,
					Mac:     mac.String(),
					Sandbox: netns,
				}
			}
			ips = append(ips, &cnicurrent.IPConfig{
				Version:   version,
				Interface: cnicurrent.Int(0),
				Address:   *ip,
			})
		} else {
			errs = append(errs, err)
		}
	}
	if cniInterface == nil || len(ips) == 0 {
		return nil, fmt.Errorf("neither IPv4 nor IPv6 found when retrieving network status: %v", errs)
	}

	result = &cnicurrent.Result{
		CNIVersion: network.config.CNIVersion,
		Interfaces: []*cnicurrent.Interface{cniInterface},
		IPs:        ips,
	}

	// Result must be the same CNIVersion as the CNI config
	converted, err := result.GetAsVersion(network.config.CNIVersion)
	if err != nil {
		return nil, err
	}

	return converted, nil
}

func (network *cniNetwork) deleteFromNetwork(rt *libcni.RuntimeConf, cni *libcni.CNIConfig) error {
	logrus.Infof("About to del CNI network %s (type=%v)", network.name, network.config.Plugins[0].Network.Type)
	if err := cni.DelNetworkList(context.Background(), network.config, rt); err != nil {
		logrus.Errorf("Error deleting network: %v", err)
		return err
	}
	return nil
}

func buildCNIRuntimeConf(cacheDir string, podNetwork *PodNetwork, ifName string, runtimeConfig RuntimeConfig) (*libcni.RuntimeConf, error) {
	logrus.Infof("Got pod network %+v", podNetwork)

	rt := &libcni.RuntimeConf{
		ContainerID: podNetwork.ID,
		NetNS:       podNetwork.NetNS,
		CacheDir:    cacheDir,
		IfName:      ifName,
		Args: [][2]string{
			{"IgnoreUnknown", "1"},
			{"K8S_POD_NAMESPACE", podNetwork.Namespace},
			{"K8S_POD_NAME", podNetwork.Name},
			{"K8S_POD_INFRA_CONTAINER_ID", podNetwork.ID},
		},
		CapabilityArgs: map[string]interface{}{},
	}

	// Add requested static IP to CNI_ARGS
	ip := runtimeConfig.IP
	if ip != "" {
		if tstIP := net.ParseIP(ip); tstIP == nil {
			return nil, fmt.Errorf("unable to parse IP address %q", ip)
		}
		rt.Args = append(rt.Args, [2]string{"IP", ip})
	}

	// Set PortMappings in Capabilities
	if len(runtimeConfig.PortMappings) != 0 {
		rt.CapabilityArgs["portMappings"] = runtimeConfig.PortMappings
	}

	// Set Bandwidth in Capabilities
	if runtimeConfig.Bandwidth != nil {
		rt.CapabilityArgs["bandwidth"] = map[string]uint64{
			"ingressRate":  runtimeConfig.Bandwidth.IngressRate,
			"ingressBurst": runtimeConfig.Bandwidth.IngressBurst,
			"egressRate":   runtimeConfig.Bandwidth.EgressRate,
			"egressBurst":  runtimeConfig.Bandwidth.EgressBurst,
		}
	}

	// Set IpRanges in Capabilities
	if len(runtimeConfig.IpRanges) > 0 {
		rt.CapabilityArgs["ipRanges"] = runtimeConfig.IpRanges
	}

	return rt, nil
}

func (plugin *cniNetworkPlugin) Status() error {
	if plugin.getDefaultNetwork() == nil {
		return errMissingDefaultNetwork
	}
	return nil
}
