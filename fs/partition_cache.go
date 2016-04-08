// Copyright 2016 Google Inc. All Rights Reserved.
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

// +build linux
package fs

import (
	"encoding/json"
	"fmt"
	"github.com/golang/glog"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

const (
	LabelSystemRoot   = "root"
	LabelDockerImages = "docker-images"
)

type RealPartitionCache struct {
	dmsetup   dmsetupClient
	mountInfo mountInfoClient
	// Docker configuration
	context Context
	// Map from block device path to partition information.
	partitions map[string]partition
	// Labels are intent-specific tags that are auto-detected.
	labels map[string]string
	// For operations on partitions and labels
	lock sync.Mutex
}

func newPartitionCache(context Context, dmsetup dmsetupClient, mountInfo mountInfoClient) PartitionCache {
	partitionCache := &RealPartitionCache{
		context:    context,
		dmsetup:    dmsetup,
		mountInfo:  mountInfo,
		partitions: make(map[string]partition),
		labels:     make(map[string]string),
	}
	return partitionCache
}

func NewPartitionCache(context Context) PartitionCache {
	return newPartitionCache(context, &defaultDmsetupClient{}, &defaultMountInfoClient{})
}

func (self *RealPartitionCache) updateCache(labels map[string]string, partitions map[string]partition) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.labels = labels
	self.partitions = partitions
}

func (self *RealPartitionCache) Clear() {
	self.updateCache(make(map[string]string), make(map[string]partition))
}

func (self *RealPartitionCache) Refresh() error {
	partitions := make(map[string]partition)
	labels := make(map[string]string)

	supportedFsType := map[string]bool{
		// all ext systems are checked through prefix.
		"btrfs": true,
		"xfs":   true,
		"zfs":   true,
	}

	mounts, err := self.mountInfo.GetMounts()
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		var Fstype string
		if !strings.HasPrefix(mount.Fstype, "ext") && !supportedFsType[mount.Fstype] {
			continue
		}
		// Avoid bind mounts.
		if _, ok := partitions[mount.Source]; ok {
			continue
		}
		if mount.Fstype == "zfs" {
			Fstype = mount.Fstype // REVIEW: Is this correct?
		}
		partitions[mount.Source] = partition{
			fsType:     Fstype,
			mountpoint: mount.Mountpoint,
			major:      uint(mount.Major),
			minor:      uint(mount.Minor),
		}
	}

	addDockerImagesLabel(self.context, self.dmsetup, labels, partitions)
	addSystemRootLabel(labels, partitions)

	self.updateCache(labels, partitions)
	return nil
}

func (self *RealPartitionCache) ApplyOverPartitions(f func(d string, p partition) error) error {
	if len(self.partitions) == 0 {
		glog.Infof("Partition cache is empty: updating")
		err := self.Refresh()
		if err != nil {
			return err
		}
	}

	for device, partition := range self.partitions {
		err := f(device, partition)
		if err != nil {
			return err
		}
	}

	return nil
}

func (self *RealPartitionCache) ApplyOverLabels(f func(l string, d string) error) error {
	if len(self.labels) == 0 {
		glog.Infof("Partition cache is empty: updating")
		err := self.Refresh()
		if err != nil {
			return err
		}
	}

	for label, device := range self.labels {
		err := f(label, device)
		if err != nil {
			return err
		}
	}

	return nil
}

func (self *RealPartitionCache) PartitionForDevice(device string) (partition, error) {
	p, ok := self.partitions[device]
	if ok {
		return p, nil
	}

	glog.Infof("Partition cache miss for device %q, refreshing partition cache", device)
	err := self.Refresh()
	if err != nil {
		return partition{}, err
	}

	p, ok = self.partitions[device]
	if ok {
		return p, nil
	}

	return partition{}, fmt.Errorf("No partition for device %s", device)
}

func (self *RealPartitionCache) DeviceInfoForMajorMinor(major uint, minor uint) (*DeviceInfo, error) {
	var ret *DeviceInfo = nil

	err := self.ApplyOverPartitions(func(device string, partition partition) error {
		if partition.major == major && partition.minor == minor {
			ret = &DeviceInfo{
				Device: device,
				Major:  major,
				Minor:  minor,
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	if ret == nil {
		return nil, fmt.Errorf("could not find device with major: %d, minor: %d in partition cache", major, minor)
	}

	return ret, nil
}

func (self *RealPartitionCache) DeviceNameForLabel(label string) (string, error) {
	d, ok := self.labels[label]
	if ok {
		return d, nil
	}

	glog.Infof("Partition cache miss for label %q, refreshing partition cache", label)
	err := self.Refresh()
	if err != nil {
		return "", err
	}

	d, ok = self.labels[label]
	if ok {
		return d, nil
	}

	return "", fmt.Errorf("No device for label %s", label)
}

// addSystemRootLabel attempts to determine which device contains the mount for /.
func addSystemRootLabel(labels map[string]string, partitions map[string]partition) {
	for src, p := range partitions {
		if p.mountpoint == "/" {
			if _, ok := labels[LabelSystemRoot]; !ok {
				labels[LabelSystemRoot] = src
			}
		}
	}
}

// addDockerImagesLabel attempts to determine which device contains the mount for docker images.
func addDockerImagesLabel(context Context, dmsetup dmsetupClient, labels map[string]string, partitions map[string]partition) {
	dockerDev, dockerPartition, err := getDockerDeviceMapperInfo(context.DockerInfo, dmsetup)
	if err != nil {
		glog.Warningf("Could not get Docker devicemapper device: %v", err)
	}
	if len(dockerDev) > 0 && dockerPartition != nil {
		partitions[dockerDev] = *dockerPartition
		labels[LabelDockerImages] = dockerDev
	} else {
		dockerPaths := getDockerImagePaths(context)

		for src, p := range partitions {
			updateDockerImagesPath(src, p.mountpoint, dockerPaths, labels, partitions)
		}
	}
}

// Generate a list of possible mount points for docker image management from the docker root directory.
// Right now, we look for each type of supported graph driver directories, but we can do better by parsing
// some of the context from `docker info`.
func getDockerImagePaths(context Context) []string {
	// TODO(rjnagal): Detect docker root and graphdriver directories from docker info.
	dockerRoot := context.DockerRoot
	dockerImagePaths := []string{}
	for _, dir := range []string{"devicemapper", "btrfs", "aufs", "overlay", "zfs"} {
		dockerImagePaths = append(dockerImagePaths, path.Join(dockerRoot, dir))
	}
	for dockerRoot != "/" && dockerRoot != "." {
		dockerImagePaths = append(dockerImagePaths, dockerRoot)
		dockerRoot = filepath.Dir(dockerRoot)
	}
	dockerImagePaths = append(dockerImagePaths, "/")
	return dockerImagePaths
}

// This method compares the mountpoint with possible docker image mount points. If a match is found,
// docker images label is added to the partition.
func updateDockerImagesPath(source string, mountpoint string, dockerImagePaths []string, labels map[string]string, partitions map[string]partition) {
	for _, v := range dockerImagePaths {
		if v == mountpoint {
			if i, ok := labels[LabelDockerImages]; ok {
				// pick the innermost mountpoint.
				mnt := partitions[i].mountpoint
				if len(mnt) < len(mountpoint) {
					labels[LabelDockerImages] = source
				}
			} else {
				labels[LabelDockerImages] = source
			}
		}
	}
}

// Devicemapper thin provisioning is detailed at
// https://www.kernel.org/doc/Documentation/device-mapper/thin-provisioning.txt
func dockerDMDevice(driverStatus [][]string, dmsetup dmsetupClient) (string, uint, uint, uint, error) {
	poolName := dockerStatusValue(driverStatus, "Pool Name")
	if len(poolName) == 0 {
		return "", 0, 0, 0, fmt.Errorf("Could not get dm pool name")
	}

	out, err := dmsetup.table(poolName)
	if err != nil {
		return "", 0, 0, 0, err
	}

	major, minor, dataBlkSize, err := parseDMTable(string(out))
	if err != nil {
		return "", 0, 0, 0, err
	}

	return poolName, major, minor, dataBlkSize, nil
}

// getDockerDeviceMapperInfo returns information about the devicemapper device and "partition" if
// docker is using devicemapper for its storage driver. If a loopback device is being used, don't
// return any information or error, as we want to report based on the actual partition where the
// loopback file resides, instead of the loopback file itself.
func getDockerDeviceMapperInfo(dockerInfo map[string]string, dmsetup dmsetupClient) (string, *partition, error) {
	if storageDriver, ok := dockerInfo["Driver"]; ok && storageDriver != DeviceMapper.String() {
		return "", nil, nil
	}

	var driverStatus [][]string
	if err := json.Unmarshal([]byte(dockerInfo["DriverStatus"]), &driverStatus); err != nil {
		return "", nil, err
	}

	dataLoopFile := dockerStatusValue(driverStatus, "Data loop file")
	if len(dataLoopFile) > 0 {
		return "", nil, nil
	}

	dev, major, minor, blockSize, err := dockerDMDevice(driverStatus, dmsetup)
	if err != nil {
		return "", nil, err
	}

	return dev, &partition{
		fsType:    DeviceMapper.String(),
		major:     major,
		minor:     minor,
		blockSize: blockSize,
	}, nil
}

func dockerStatusValue(status [][]string, target string) string {
	for _, v := range status {
		if len(v) == 2 && strings.ToLower(v[0]) == strings.ToLower(target) {
			return v[1]
		}
	}
	return ""
}
