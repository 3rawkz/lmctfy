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

// Per-container manager.

package manager

import (
	"container/list"
	"flag"
	"log"
	"sync"
	"time"

	"github.com/google/lmctfy/cadvisor/container"
	"github.com/google/lmctfy/cadvisor/info"
)

var historyDuration = flag.Int("history_duration", 60, "number of seconds of container history to keep")

// Internal mirror of the external data structure.
type containerStat struct {
	Timestamp time.Time
	Data      *info.ContainerStats
}
type containerInfo struct {
	Name          string
	Subcontainers []string
	Spec          *info.ContainerSpec
	Stats         *list.List
	StatsSummary  *info.ContainerStatsSummary
}

type containerData struct {
	handler container.ContainerHandler
	info    containerInfo
	lock    sync.Mutex

	// Tells the container to stop.
	stop chan bool
}

func (c *containerData) Start() error {
	// Force the first update.
	c.housekeepingTick()
	log.Printf("Start housekeeping for container %q\n", c.info.Name)

	go c.housekeeping()
	return nil
}

func (c *containerData) Stop() error {
	c.stop <- true
	return nil
}

func (c *containerData) GetInfo() (*containerInfo, error) {
	// TODO(vmarmol): Consider caching this.
	// Get spec and subcontainers.
	err := c.updateSpec()
	if err != nil {
		return nil, err
	}
	err = c.updateSubcontainers()
	if err != nil {
		return nil, err
	}

	// Make a copy of the info for the user.
	c.lock.Lock()
	defer c.lock.Unlock()
	ret := c.info
	return &ret, nil
}

func NewContainerData(containerName string) (*containerData, error) {
	cont := &containerData{}
	handler, err := container.NewContainerHandler(containerName)
	if err != nil {
		return nil, err
	}
	cont.handler = handler
	cont.info.Name = containerName
	cont.info.Stats = list.New()
	cont.stop = make(chan bool, 1)

	return cont, nil
}

func (c *containerData) housekeeping() {
	// Housekeep every second.
	for true {
		select {
		case <-c.stop:
			// Stop housekeeping when signaled.
			return
		case <-time.Tick(time.Second):
			start := time.Now()
			c.housekeepingTick()

			// Log if housekeeping took longer than 120ms.
			duration := time.Since(start)
			if duration >= 120*time.Millisecond {
				log.Printf("Housekeeping(%s) took %s", c.info.Name, duration)
			}
		}
	}
}

func (c *containerData) housekeepingTick() {
	err := c.updateStats()
	if err != nil {
		log.Printf("Failed to update stats for container \"%s\": %s", c.info.Name, err)
	}
}

func (c *containerData) updateSpec() error {
	spec, err := c.handler.GetSpec()
	if err != nil {
		return err
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.info.Spec = spec
	return nil
}

func (c *containerData) updateStats() error {
	stats, err := c.handler.GetStats()
	if err != nil {
		return err
	}
	if stats == nil {
		return nil
	}
	summary, err := c.handler.StatsSummary()
	if err != nil {
		return err
	}
	timestamp := time.Now()

	// Remove the front if we go over.
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.info.Stats.Len() >= *historyDuration {
		c.info.Stats.Remove(c.info.Stats.Front())
	}
	c.info.Stats.PushBack(&containerStat{
		Timestamp: timestamp,
		Data:      stats,
	})
	c.info.StatsSummary = summary
	return nil
}

func (c *containerData) updateSubcontainers() error {
	subcontainers, err := c.handler.ListContainers(container.LIST_SELF)
	if err != nil {
		return err
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.info.Subcontainers = subcontainers
	return nil
}
