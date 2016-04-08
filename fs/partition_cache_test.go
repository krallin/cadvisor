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

package fs

import (
	"errors"
	"fmt"
	"github.com/stretchr/testify/assert"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	dockerMountInfo "github.com/docker/docker/pkg/mount"
)

type testMountInfoClient struct {
	Calls  int
	mounts []*dockerMountInfo.Info
}

func (self *testMountInfoClient) GetMounts() ([]*dockerMountInfo.Info, error) {
	self.Calls += 1
	if len(self.mounts) == 0 {
		return nil, fmt.Errorf("No test mounts!")
	}
	return self.mounts, nil
}

func TestBaseCacheOperations(t *testing.T) {
	as := assert.New(t)

	// Prepare dummy mounts
	mounts := []*dockerMountInfo.Info{
		&dockerMountInfo.Info{
			Major:      1,
			Minor:      11,
			Source:     "/dev/sda1",
			Mountpoint: "/",
			Fstype:     "ext4",
		},
		&dockerMountInfo.Info{
			Major:      2,
			Minor:      22,
			Source:     "/dev/sdb1",
			Mountpoint: "/var/lib/docker",
			Fstype:     "xfs",
		},
		&dockerMountInfo.Info{
			Major:      3,
			Minor:      33,
			Mountpoint: "/tmp",
			Fstype:     "tmpfs",
		},
	}

	mountInfoClient := &testMountInfoClient{
		mounts: mounts,
	}

	partitionCache := newPartitionCache(Context{
		DockerRoot: "/var/lib/docker",
		DockerInfo: make(map[string]string),
	}, &testDmsetup{}, mountInfoClient)

	// PartitionForDevice should work
	p1, err := partitionCache.PartitionForDevice("/dev/sda1")
	as.NoError(err)
	as.Equal(uint(1), p1.major)
	as.Equal(uint(11), p1.minor)
	as.Equal("/", p1.mountpoint)

	p2, err := partitionCache.PartitionForDevice("/dev/sdb1")
	as.NoError(err)
	as.Equal(uint(2), p2.major)
	as.Equal(uint(22), p2.minor)
	as.Equal("/var/lib/docker", p2.mountpoint)

	// DeviceInfoForMajorMinor should work as well
	d1, err := partitionCache.DeviceInfoForMajorMinor(1, 11)
	as.NoError(err)
	as.Equal("/dev/sda1", d1.Device)
	as.Equal(uint(1), d1.Major)
	as.Equal(uint(11), d1.Minor)

	d2, err := partitionCache.DeviceInfoForMajorMinor(2, 22)
	as.NoError(err)
	as.Equal("/dev/sdb1", d2.Device)

	// tmpfs should be ignored
	_, err = partitionCache.DeviceInfoForMajorMinor(3, 33)
	as.Error(err)

	// Check labels
	d3, err := partitionCache.DeviceNameForLabel(LabelSystemRoot)
	as.NoError(err)
	as.Equal("/dev/sda1", d3)

	d4, err := partitionCache.DeviceNameForLabel(LabelDockerImages)
	as.NoError(err)
	as.Equal("/dev/sdb1", d4)

	// Test that the cache refreshes automatically when data is missing
	var n int

	baseCalls := mountInfoClient.Calls

	partitionCache.Clear()
	n = 0
	partitionCache.ApplyOverPartitions(func(d string, p partition) error {
		n += 1
		return nil
	})
	as.Equal(2, n)
	as.Equal(baseCalls+1, mountInfoClient.Calls)

	partitionCache.Clear()
	n = 0
	partitionCache.ApplyOverLabels(func(l string, d string) error {
		n += 1
		return nil
	})
	as.Equal(2, n)
	as.Equal(baseCalls+2, mountInfoClient.Calls)

	partitionCache.Clear()
	p, err := partitionCache.PartitionForDevice("/dev/sda1")
	as.NoError(err)
	as.Equal("/", p.mountpoint)
	as.Equal(baseCalls+3, mountInfoClient.Calls)

	partitionCache.Clear()
	i, err := partitionCache.DeviceInfoForMajorMinor(1, 11)
	as.NoError(err)
	as.Equal("/dev/sda1", i.Device)
	as.Equal(baseCalls+4, mountInfoClient.Calls)

	partitionCache.Clear()
	d, err := partitionCache.DeviceNameForLabel(LabelSystemRoot)
	as.NoError(err)
	as.Equal("/dev/sda1", d)
	as.Equal(baseCalls+5, mountInfoClient.Calls)
}

func TestDeviceMapperLabelling(t *testing.T) {
	as := assert.New(t)
	mounts := []*dockerMountInfo.Info{
		&dockerMountInfo.Info{
			Major:      1,
			Minor:      11,
			Source:     "/dev/sda1",
			Mountpoint: "/",
			Fstype:     "ext4",
		},
	}

	dockerInfo := map[string]string{
		"Driver":       "devicemapper",
		"DriverStatus": "[[\"Pool Name\", \"thin-pool\"]]",
	}

	partitionCache := newPartitionCache(Context{
		DockerRoot: "/var/lib/docker",
		DockerInfo: dockerInfo,
	}, &testDmsetup{
		data: []byte("0 409534464 thin-pool 253:6 2:22 128 32768 1 skip_block_zeroing"),
		err:  nil,
	}, &testMountInfoClient{
		mounts: mounts,
	})

	// Check that we get the right devices back via major / minor
	d1, err := partitionCache.DeviceInfoForMajorMinor(2, 22)
	as.NoError(err)
	as.Equal("thin-pool", d1.Device)

	d2, err := partitionCache.DeviceInfoForMajorMinor(1, 11)
	as.NoError(err)
	as.Equal("/dev/sda1", d2.Device)

	// Check that the Docker images label is properly applied
	d3, err := partitionCache.DeviceNameForLabel(LabelDockerImages)
	as.NoError(err)
	as.Equal("thin-pool", d3)

	// Verify that the partition exists as well
	p, err := partitionCache.PartitionForDevice("thin-pool")
	as.NoError(err)
	as.Equal("devicemapper", p.fsType)
	as.Equal(uint(128), p.blockSize)
}

type raceMounts struct {
	i int32
}

func (self *raceMounts) GetMounts() ([]*dockerMountInfo.Info, error) {
	i := atomic.AddInt32(&self.i, 1)

	mounts := make([]*dockerMountInfo.Info, i)

	var j int32
	for j = 0; j < i; j++ {
		mounts[j] = &dockerMountInfo.Info{
			Major:      int(j),
			Minor:      int(j * 10),
			Source:     fmt.Sprintf("%d", i),
			Mountpoint: fmt.Sprintf("%d", j),
			Fstype:     "ext4",
		}
	}

	return mounts, nil
}

func TestPartitionCacheRefreshRaces(t *testing.T) {
	as := assert.New(t)
	n := 100

	partitionCache := newPartitionCache(Context{
		DockerRoot: "/var/lib/docker",
		DockerInfo: make(map[string]string),
	}, &testDmsetup{}, &raceMounts{i: 0})

	wg := &sync.WaitGroup{}
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			partitionCache.Refresh()
		}()
	}

	wg.Wait()

	// We can largely rely on the data race detector to throw an error here,
	// but it's still worth checking that all the partitions that were added
	// via the same update (by checking the device, which we set to i.
	var last string
	err := partitionCache.ApplyOverPartitions(func(d string, _ partition) error {
		if len(last) == 0 {
			last = d
			return nil
		}

		if last != d {
			return fmt.Errorf("Race (mismatched sources): %s, %s", last, d)
		}

		return nil
	})

	as.NoError(err)
}

func TestAddSystemRootLabel(t *testing.T) {
	labels := map[string]string{}
	partitions := map[string]partition{
		"/dev/mapper/vg_vagrant-lv_root": {
			mountpoint: "/",
		},
		"vg_vagrant-docker--pool": {
			mountpoint: "",
			fsType:     "devicemapper",
		},
	}

	addSystemRootLabel(labels, partitions)
	if e, a := "/dev/mapper/vg_vagrant-lv_root", labels[LabelSystemRoot]; e != a {
		t.Errorf("expected %q, got %q", e, a)
	}
}

func TestAddDockerImagesLabel(t *testing.T) {
	tests := []struct {
		name                           string
		driver                         string
		driverStatus                   string
		dmsetupTable                   string
		getDockerDeviceMapperInfoError error
		partitions                     map[string]partition
		expectedDockerDevice           string
		expectedPartition              *partition
	}{
		{
			name:         "devicemapper, not loopback",
			driver:       "devicemapper",
			driverStatus: `[["Pool Name", "vg_vagrant-docker--pool"]]`,
			dmsetupTable: "0 53870592 thin-pool 253:2 253:3 1024 0 1 skip_block_zeroing",
			partitions: map[string]partition{
				"/dev/mapper/vg_vagrant-lv_root": {
					mountpoint: "/",
					fsType:     "devicemapper",
				},
			},
			expectedDockerDevice: "vg_vagrant-docker--pool",
			expectedPartition: &partition{
				fsType:    "devicemapper",
				major:     253,
				minor:     3,
				blockSize: 1024,
			},
		},
		{
			name:         "devicemapper, loopback on non-root partition",
			driver:       "devicemapper",
			driverStatus: `[["Data loop file","/var/lib/docker/devicemapper/devicemapper/data"]]`,
			partitions: map[string]partition{
				"/dev/mapper/vg_vagrant-lv_root": {
					mountpoint: "/",
					fsType:     "devicemapper",
				},
				"/dev/sdb1": {
					mountpoint: "/var/lib/docker/devicemapper",
				},
			},
			expectedDockerDevice: "/dev/sdb1",
		},
		{
			name: "multiple mounts - innermost check",
			partitions: map[string]partition{
				"/dev/sda1": {
					mountpoint: "/",
					fsType:     "ext4",
				},
				"/dev/sdb1": {
					mountpoint: "/var/lib/docker",
					fsType:     "ext4",
				},
				"/dev/sdb2": {
					mountpoint: "/var/lib/docker/btrfs",
					fsType:     "btrfs",
				},
			},
			expectedDockerDevice: "/dev/sdb2",
		},
	}

	for _, tt := range tests {
		dmsetup := &testDmsetup{
			data: []byte(tt.dmsetupTable),
		}
		partitions := tt.partitions
		labels := map[string]string{}

		context := Context{
			DockerRoot: "/var/lib/docker",
			DockerInfo: map[string]string{
				"Driver":       tt.driver,
				"DriverStatus": tt.driverStatus,
			},
		}

		addDockerImagesLabel(context, dmsetup, labels, partitions)

		if e, a := tt.expectedDockerDevice, labels[LabelDockerImages]; e != a {
			t.Errorf("%s: docker device: expected %q, got %q", tt.name, e, a)
		}

		if tt.expectedPartition == nil {
			continue
		}
		if e, a := *tt.expectedPartition, partitions[tt.expectedDockerDevice]; !reflect.DeepEqual(e, a) {
			t.Errorf("%s: docker partition: expected %#v, got %#v", tt.name, e, a)
		}
	}
}

func TestGetDockerDeviceMapperInfo(t *testing.T) {
	tests := []struct {
		name              string
		driver            string
		driverStatus      string
		dmsetupTable      string
		dmsetupTableError error
		expectedDevice    string
		expectedPartition *partition
		expectedError     bool
	}{
		{
			name:              "not devicemapper",
			driver:            "btrfs",
			expectedDevice:    "",
			expectedPartition: nil,
			expectedError:     false,
		},
		{
			name:              "error unmarshaling driver status",
			driver:            "devicemapper",
			driverStatus:      "{[[[asdf",
			expectedDevice:    "",
			expectedPartition: nil,
			expectedError:     true,
		},
		{
			name:              "loopback",
			driver:            "devicemapper",
			driverStatus:      `[["Data loop file","/var/lib/docker/devicemapper/devicemapper/data"]]`,
			expectedDevice:    "",
			expectedPartition: nil,
			expectedError:     false,
		},
		{
			name:              "missing pool name",
			driver:            "devicemapper",
			driverStatus:      `[[]]`,
			expectedDevice:    "",
			expectedPartition: nil,
			expectedError:     true,
		},
		{
			name:              "error invoking dmsetup",
			driver:            "devicemapper",
			driverStatus:      `[["Pool Name", "vg_vagrant-docker--pool"]]`,
			dmsetupTableError: errors.New("foo"),
			expectedDevice:    "",
			expectedPartition: nil,
			expectedError:     true,
		},
		{
			name:              "unable to parse dmsetup table",
			driver:            "devicemapper",
			driverStatus:      `[["Pool Name", "vg_vagrant-docker--pool"]]`,
			dmsetupTable:      "no data here!",
			expectedDevice:    "",
			expectedPartition: nil,
			expectedError:     true,
		},
		{
			name:           "happy path",
			driver:         "devicemapper",
			driverStatus:   `[["Pool Name", "vg_vagrant-docker--pool"]]`,
			dmsetupTable:   "0 53870592 thin-pool 253:2 253:3 1024 0 1 skip_block_zeroing",
			expectedDevice: "vg_vagrant-docker--pool",
			expectedPartition: &partition{
				fsType:    "devicemapper",
				major:     253,
				minor:     3,
				blockSize: 1024,
			},
			expectedError: false,
		},
	}

	for _, tt := range tests {
		dmsetup := &testDmsetup{
			data: []byte(tt.dmsetupTable),
		}

		dockerInfo := map[string]string{
			"Driver":       tt.driver,
			"DriverStatus": tt.driverStatus,
		}

		device, partition, err := getDockerDeviceMapperInfo(dockerInfo, dmsetup)

		if tt.expectedError && err == nil {
			t.Errorf("%s: expected error but got nil", tt.name)
			continue
		}
		if !tt.expectedError && err != nil {
			t.Errorf("%s: unexpected error: %v", tt.name, err)
			continue
		}

		if e, a := tt.expectedDevice, device; e != a {
			t.Errorf("%s: device: expected %q, got %q", tt.name, e, a)
		}

		if e, a := tt.expectedPartition, partition; !reflect.DeepEqual(e, a) {
			t.Errorf("%s: partition: expected %#v, got %#v", tt.name, e, a)
		}
	}
}
