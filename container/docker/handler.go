// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Handler for Docker containers.
package docker

import (
	"fmt"
	"io/ioutil"
	"math"
	"path"
	"strings"
	"time"

	"github.com/google/cadvisor/container"
	containerlibcontainer "github.com/google/cadvisor/container/libcontainer"
	"github.com/google/cadvisor/fs"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/utils"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/golang/glog"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	cgroupfs "github.com/opencontainers/runc/libcontainer/cgroups/fs"
	libcontainerconfigs "github.com/opencontainers/runc/libcontainer/configs"
)

const (
	// The read write layers exist here.
	aufsRWLayer = "diff"
	// Path to the directory where docker stores log files if the json logging driver is enabled.
	pathToContainersLogDir = "containers"
)

type dockerContainerHandler struct {
	client             *docker.Client
	name               string
	id                 string
	aliases            []string
	machineInfoFactory info.MachineInfoFactory

	// Absolute path to the cgroup hierarchies of this container.
	// (e.g.: "cpu" -> "/sys/fs/cgroup/cpu/test")
	cgroupPaths map[string]string

	// Manager of this container's cgroups.
	cgroupManager cgroups.Manager

	storageDriver storageDriver
	fsInfo        fs.FsInfo

	// Time at which this container was created.
	creationTime time.Time

	// Metadata associated with the container.
	labels map[string]string
	envs   map[string]string

	// The directories in use by this container
	baseDirs  []string
	extraDirs []string

	// The container PID used to switch namespaces as required
	pid int

	// Image name used for this container.
	image string

	// The host root FS to read
	rootFs string

	// The network mode of the container
	networkMode string

	// Filesystem handler.
	fsHandler fsHandler

	ignoreMetrics container.MetricSet
}

func getRwLayerID(containerID, storageDir string, sd storageDriver, dockerVersion []int) (string, error) {
	const (
		// Docker version >=1.10.0 have a randomized ID for the root fs of a container.
		randomizedRWLayerMinorVersion = 10
		rwLayerIDFile                 = "mount-id"
	)
	if (dockerVersion[0] <= 1) && (dockerVersion[1] < randomizedRWLayerMinorVersion) {
		return containerID, nil
	}

	bytes, err := ioutil.ReadFile(path.Join(storageDir, "image", string(sd), "layerdb", "mounts", containerID, rwLayerIDFile))
	if err != nil {
		return "", fmt.Errorf("failed to identify the read-write layer ID for container %q. - %v", containerID, err)
	}
	return string(bytes), err
}

func newDockerContainerHandler(
	client *docker.Client,
	name string,
	machineInfoFactory info.MachineInfoFactory,
	fsInfo fs.FsInfo,
	storageDriver storageDriver,
	storageDir string,
	cgroupSubsystems *containerlibcontainer.CgroupSubsystems,
	inHostNamespace bool,
	metadataEnvs []string,
	dockerVersion []int,
	ignoreMetrics container.MetricSet,
) (container.ContainerHandler, error) {
	// Create the cgroup paths.
	cgroupPaths := make(map[string]string, len(cgroupSubsystems.MountPoints))
	for key, val := range cgroupSubsystems.MountPoints {
		cgroupPaths[key] = path.Join(val, name)
	}

	// Generate the equivalent cgroup manager for this container.
	cgroupManager := &cgroupfs.Manager{
		Cgroups: &libcontainerconfigs.Cgroup{
			Name: name,
		},
		Paths: cgroupPaths,
	}

	rootFs := "/"
	if !inHostNamespace {
		rootFs = "/rootfs"
		storageDir = path.Join(rootFs, storageDir)
	}

	id := ContainerNameToDockerId(name)

	rwLayerID, err := getRwLayerID(id, storageDir, storageDriver, dockerVersion)
	if err != nil {
		return nil, err
	}

	handler := &dockerContainerHandler{
		id:                 id,
		client:             client,
		name:               name,
		machineInfoFactory: machineInfoFactory,
		cgroupPaths:        cgroupPaths,
		cgroupManager:      cgroupManager,
		storageDriver:      storageDriver,
		fsInfo:             fsInfo,
		rootFs:             rootFs,
		envs:               make(map[string]string),
		ignoreMetrics:      ignoreMetrics,
	}

	// We assume that if Inspect fails then the container is not known to docker.
	ctnr, err := client.InspectContainer(id)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container %q: %v", id, err)
	}
	handler.creationTime = ctnr.Created
	handler.pid = ctnr.State.Pid

	// Add the name and bare ID as aliases of the container.
	handler.aliases = append(handler.aliases, strings.TrimPrefix(ctnr.Name, "/"), id)
	handler.labels = ctnr.Config.Labels
	handler.image = ctnr.Config.Image
	handler.networkMode = ctnr.HostConfig.NetworkMode

	// split env vars to get metadata map.
	for _, exposedEnv := range metadataEnvs {
		for _, envVar := range ctnr.Config.Env {
			splits := strings.SplitN(envVar, "=", 2)
			if splits[0] == exposedEnv {
				handler.envs[strings.ToLower(exposedEnv)] = splits[1]
			}
		}
	}

	handler.baseDirs = make([]string, 0)
	handler.extraDirs = make([]string, 0)

	// Docker API >= 1.20 exposes a "Mounts" list of structures representing mounts
	// (https://docs.docker.com/engine/reference/api/docker_remote_api_v1.20/)
	for _, mount := range ctnr.Mounts {
		handler.baseDirs = append(handler.baseDirs, path.Join(rootFs, mount.Source))
	}

	// Docker API < 1.20 exposes a "Volumes" mapping of container paths to host paths.
	// (https://docs.docker.com/engine/reference/api/docker_remote_api_v1.19/)
	for _, hostPath := range ctnr.Volumes {
		handler.baseDirs = append(handler.baseDirs, path.Join(rootFs, hostPath))
	}

	// Now, handle the rootfs
	rootfsStorageDir := ""
	switch storageDriver {
	case aufsStorageDriver:
		rootfsStorageDir = path.Join(storageDir, string(aufsStorageDriver), aufsRWLayer, rwLayerID)
	case overlayStorageDriver:
		rootfsStorageDir = path.Join(storageDir, string(overlayStorageDriver), rwLayerID)
	}

	// We support (and found) the mount
	if rootfsStorageDir != "" {
		handler.baseDirs = append(handler.baseDirs, rootfsStorageDir)
	} else {
		glog.Warningf("Unable to find root mount for container %q (storageDriver: %q)", name, storageDriver)
	}

	// Now, handle the storage dir
	logsStorageDir := path.Join(storageDir, pathToContainersLogDir, id)
	handler.extraDirs = append(handler.extraDirs, logsStorageDir)

	// And start DiskUsageMetrics (if enabled)
	if !ignoreMetrics.Has(container.DiskUsageMetrics) {
		// We need to find all our base dirs!
		handler.fsHandler = newFsHandler(time.Minute, handler.baseDirs, handler.extraDirs, fsInfo)
	}

	return handler, nil
}

func (self *dockerContainerHandler) Start() {
	// Start the filesystem handler.
	if self.fsHandler != nil {
		self.fsHandler.start()
	}
}

func (self *dockerContainerHandler) Cleanup() {
	if self.fsHandler != nil {
		self.fsHandler.stop()
	}
}

func (self *dockerContainerHandler) ContainerReference() (info.ContainerReference, error) {
	return info.ContainerReference{
		Id:        self.id,
		Name:      self.name,
		Aliases:   self.aliases,
		Namespace: DockerNamespace,
		Labels:    self.labels,
	}, nil
}

func (self *dockerContainerHandler) readLibcontainerConfig() (*libcontainerconfigs.Config, error) {
	config, err := containerlibcontainer.ReadConfig(*dockerRootDir, *dockerRunDir, self.id)
	if err != nil {
		return nil, fmt.Errorf("failed to read libcontainer config: %v", err)
	}

	// Replace cgroup parent and name with our own since we may be running in a different context.
	if config.Cgroups == nil {
		config.Cgroups = new(libcontainerconfigs.Cgroup)
	}
	config.Cgroups.Name = self.name
	config.Cgroups.Parent = "/"

	return config, nil
}

func libcontainerConfigToContainerSpec(config *libcontainerconfigs.Config, mi *info.MachineInfo) info.ContainerSpec {
	var spec info.ContainerSpec
	spec.HasMemory = true
	spec.Memory.Limit = math.MaxUint64
	spec.Memory.SwapLimit = math.MaxUint64

	if config.Cgroups.Resources != nil {
		if config.Cgroups.Resources.Memory > 0 {
			spec.Memory.Limit = uint64(config.Cgroups.Resources.Memory)
		}
		if config.Cgroups.Resources.MemorySwap > 0 {
			spec.Memory.SwapLimit = uint64(config.Cgroups.Resources.MemorySwap)
		}

		// Get CPU info
		spec.HasCpu = true
		spec.Cpu.Limit = 1024
		if config.Cgroups.Resources.CpuShares != 0 {
			spec.Cpu.Limit = uint64(config.Cgroups.Resources.CpuShares)
		}
		spec.Cpu.Mask = utils.FixCpuMask(config.Cgroups.Resources.CpusetCpus, mi.NumCores)
	}

	spec.HasDiskIo = true

	return spec
}

func (self *dockerContainerHandler) needNet() bool {
	if !self.ignoreMetrics.Has(container.NetworkUsageMetrics) {
		return !strings.HasPrefix(self.networkMode, "container:")
	}
	return false
}

func (self *dockerContainerHandler) GetSpec() (info.ContainerSpec, error) {
	mi, err := self.machineInfoFactory.GetMachineInfo()
	if err != nil {
		return info.ContainerSpec{}, err
	}
	libcontainerConfig, err := self.readLibcontainerConfig()
	if err != nil {
		return info.ContainerSpec{}, err
	}

	spec := libcontainerConfigToContainerSpec(libcontainerConfig, mi)
	spec.CreationTime = self.creationTime

	if !self.ignoreMetrics.Has(container.DiskUsageMetrics) {
		spec.HasFilesystem = true
	}

	spec.Labels = self.labels
	spec.Envs = self.envs
	spec.Image = self.image
	spec.HasNetwork = self.needNet()

	return spec, err
}

func (self *dockerContainerHandler) getFsStats(stats *info.ContainerStats) error {
	if self.fsHandler == nil {
		return nil
	}
	fsStats, err := self.fsHandler.usage()

	if err != nil {
		return err
	}

	for _, stat := range fsStats {
		stats.Filesystem = append(stats.Filesystem, *stat)
	}

	return nil
}

// TODO(vmarmol): Get from libcontainer API instead of cgroup manager when we don't have to support older Dockers.
func (self *dockerContainerHandler) GetStats() (*info.ContainerStats, error) {
	stats, err := containerlibcontainer.GetStats(self.cgroupManager, self.rootFs, self.pid, self.ignoreMetrics)
	if err != nil {
		return stats, err
	}
	// Clean up stats for containers that don't have their own network - this
	// includes containers running in Kubernetes pods that use the network of the
	// infrastructure container. This stops metrics being reported multiple times
	// for each container in a pod.
	if !self.needNet() {
		stats.Network = info.NetworkStats{}
	}

	// Get filesystem stats.
	err = self.getFsStats(stats)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

func (self *dockerContainerHandler) ListContainers(listType container.ListType) ([]info.ContainerReference, error) {
	// No-op for Docker driver.
	return []info.ContainerReference{}, nil
}

func (self *dockerContainerHandler) GetCgroupPath(resource string) (string, error) {
	path, ok := self.cgroupPaths[resource]
	if !ok {
		return "", fmt.Errorf("could not find path for resource %q for container %q\n", resource, self.name)
	}
	return path, nil
}

func (self *dockerContainerHandler) ListThreads(listType container.ListType) ([]int, error) {
	// TODO(vmarmol): Implement.
	return nil, nil
}

func (self *dockerContainerHandler) GetContainerLabels() map[string]string {
	return self.labels
}

func (self *dockerContainerHandler) ListProcesses(listType container.ListType) ([]int, error) {
	return containerlibcontainer.GetProcesses(self.cgroupManager)
}

func (self *dockerContainerHandler) WatchSubcontainers(events chan container.SubcontainerEvent) error {
	return fmt.Errorf("watch is unimplemented in the Docker container driver")
}

func (self *dockerContainerHandler) StopWatchingSubcontainers() error {
	// No-op for Docker driver.
	return nil
}

func (self *dockerContainerHandler) Exists() bool {
	return containerlibcontainer.Exists(*dockerRootDir, *dockerRunDir, self.id)
}

func DockerInfo() (map[string]string, error) {
	client, err := Client()
	if err != nil {
		return nil, fmt.Errorf("unable to communicate with docker daemon: %v", err)
	}
	info, err := client.Info()
	if err != nil {
		return nil, err
	}
	return info.Map(), nil
}

func DockerImages() ([]docker.APIImages, error) {
	client, err := Client()
	if err != nil {
		return nil, fmt.Errorf("unable to communicate with docker daemon: %v", err)
	}
	images, err := client.ListImages(docker.ListImagesOptions{All: false})
	if err != nil {
		return nil, err
	}
	return images, nil
}
