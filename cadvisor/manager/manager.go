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

package manager

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/lmctfy/cadvisor/container"
	"github.com/google/lmctfy/cadvisor/info"
)

type Manager interface {
	// Start the manager, blocks forever.
	Start() error

	// Get information about a container.
	GetContainerInfo(containerName string) (*info.ContainerInfo, error)

	// Get information about the machine.
	GetMachineInfo() (*info.MachineInfo, error)
}

func New() (Manager, error) {
	newManager := &manager{}
	newManager.containers = make(map[string]*containerData)

	machineInfo, err := getMachineInfo()
	if err != nil {
		return nil, err
	}
	newManager.machineInfo = *machineInfo
	log.Printf("Machine: %+v", newManager.machineInfo)
	return newManager, nil
}

type manager struct {
	containers     map[string]*containerData
	containersLock sync.RWMutex
	machineInfo    info.MachineInfo
}

// Start the container manager.
func (m *manager) Start() error {
	// Create root and then recover all containers.
	_, err := m.createContainer("/")
	if err != nil {
		return err
	}
	log.Printf("Starting recovery of all containers")
	err = m.detectContainers()
	if err != nil {
		return err
	}
	log.Printf("Recovery completed")

	// Look for new containers in the main housekeeping thread.
	for t := range time.Tick(time.Second) {
		start := time.Now()

		// Check for new containers.
		err = m.detectContainers()
		if err != nil {
			log.Printf("Failed to detect containers: %s", err)
		}

		// Log if housekeeping took more than 100ms.
		duration := time.Since(start)
		if duration >= 100*time.Millisecond {
			log.Printf("Global Housekeeping(%d) took %s", t.Unix(), duration)
		}
	}
	return nil
}

// Get a container by name.
func (m *manager) GetContainerInfo(containerName string) (*info.ContainerInfo, error) {
	log.Printf("Get(%s)", containerName)
	var cont *containerData
	var ok bool
	func() {
		m.containersLock.RLock()
		defer m.containersLock.RUnlock()

		// Ensure we have the container.
		cont, ok = m.containers[containerName]
	}()
	if !ok {
		return nil, fmt.Errorf("unknown container \"%s\"", containerName)
	}

	// Get the info from the container.
	cinfo, err := cont.GetInfo()
	if err != nil {
		return nil, err
	}

	// Make a copy of the info for the user.
	ret := &info.ContainerInfo{
		Name:          cinfo.Name,
		Subcontainers: cinfo.Subcontainers,
		Spec:          cinfo.Spec,
		StatsSummary:  cinfo.StatsSummary,
	}

	// Set default value to an actual value
	if ret.Spec.Memory != nil {
		// Memory.Limit is 0 means there's no limit
		if ret.Spec.Memory.Limit == 0 {
			ret.Spec.Memory.Limit = uint64(m.machineInfo.MemoryCapacity)
		}
	}
	ret.Stats = make([]*info.ContainerStats, 0, cinfo.Stats.Len())
	for e := cinfo.Stats.Front(); e != nil; e = e.Next() {
		data := e.Value.(*containerStat)
		ret.Stats = append(ret.Stats, data.Data)
	}
	return ret, nil
}

func (m *manager) GetMachineInfo() (*info.MachineInfo, error) {
	// Copy and return the MachineInfo.
	ret := m.machineInfo
	return &ret, nil
}

// Create a container. This expects to only be called from the global manager thread.
func (m *manager) createContainer(containerName string) (*containerData, error) {
	cont, err := NewContainerData(containerName)
	if err != nil {
		return nil, err
	}

	// Add to the containers map.
	func() {
		m.containersLock.Lock()
		defer m.containersLock.Unlock()

		log.Printf("Added container: %s", containerName)
		m.containers[containerName] = cont
	}()

	// Start the container's housekeeping.
	cont.Start()
	return cont, nil
}

func (m *manager) destroyContainer(containerName string) error {
	m.containersLock.Lock()
	defer m.containersLock.Unlock()

	cont, ok := m.containers[containerName]
	if !ok {
		return fmt.Errorf("Expected container \"%s\" to exist during destroy", containerName)
	}

	// Tell the container to stop.
	err := cont.Stop()
	if err != nil {
		return err
	}

	// Remove the container from our records.
	delete(m.containers, containerName)
	log.Printf("Destroyed container: %s", containerName)
	return nil
}

type empty struct{}

// Detect all containers that have been added or deleted.
func (m *manager) getContainersDiff() (added []string, removed []string, err error) {
	// TODO(vmarmol): We probably don't need to lock around / since it will always be there.
	m.containersLock.RLock()
	defer m.containersLock.RUnlock()

	// Get all containers on the system.
	cont, ok := m.containers["/"]
	if !ok {
		return nil, nil, fmt.Errorf("Failed to find container \"/\" while checking for new containers")
	}
	allContainers, err := cont.handler.ListContainers(container.LIST_RECURSIVE)
	if err != nil {
		return nil, nil, err
	}
	allContainers = append(allContainers, "/")

	// Determine which were added and which were removed.
	allContainersSet := make(map[string]*empty)
	for name, _ := range m.containers {
		allContainersSet[name] = &empty{}
	}
	for _, name := range allContainers {
		delete(allContainersSet, name)
		_, ok := m.containers[name]
		if !ok {
			added = append(added, name)
		}
	}

	// Removed ones are no longer in the container listing.
	for name, _ := range allContainersSet {
		removed = append(removed, name)
	}

	return
}

// Detect the existing containers and reflect the setup here.
func (m *manager) detectContainers() error {
	added, removed, err := m.getContainersDiff()
	if err != nil {
		return err
	}

	// Add the new containers.
	for _, name := range added {
		_, err = m.createContainer(name)
		if err != nil {
			return fmt.Errorf("Failed to create existing container: %s: %s", name, err)
		}
	}

	// Remove the old containers.
	for _, name := range removed {
		err = m.destroyContainer(name)
		if err != nil {
			return fmt.Errorf("Failed to destroy existing container: %s: %s", name, err)
		}
	}

	return nil
}
