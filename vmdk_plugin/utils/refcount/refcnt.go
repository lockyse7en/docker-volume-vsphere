// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build linux

//
// Refcount discovery from local docker.
//
// Docker issues mount/unmount to a volume plugin each time container
// using this volume is started or stopped(or killed). So if multiple  containers
// use the same volume, it is the responsibility of the plugin to track it.
// This file implements this tracking via refcounting volume usage, and recovering
// refcounts and mount states on plugin or docker restart
//
// When  Docker is killed (-9). Docker may forget
// all about volume usage and consider clean slate. This could lead to plugin
// locking volumes which Docker does not need so we need to recover correct
// refcounts.
//
// Note: when Docker Is properly restarted ('service docker restart'), it shuts
// down accurately  and sends unmounts so refcounts stays in sync. In this case
// there is no need to do anything special in the plugin. Thus all discovery
// code is mainly for crash recovery and cleanup.
//
// Refcounts are changed in Mount/Unmount. The code in this file provides
// generic refcnt API and also supports refcount discovery on restarts:
// - Connects to Docker over unix socket, enumerates Volume Mounts and builds
//   "volume mounts refcount" map as Docker sees it.
// - Gets actual mounts from /proc/mounts, and makes sure the refcounts and
//   actual mounts are in sync.
//
// The process is initiated on plugin start,and ONLY if Docker is already
// running and thus answering client.Info() request.
//
// After refcount discovery, results are compared to /proc/mounts content.
//
// We rely on all plugin mounts being in /mnt/vmdk/<volume_name>, and will
// unount stuff there at will - this place SHOULD NOT be used for manual mounts.
//
// If a volume IS mounted, but should not be (refcount = 0)
//   - we assume there was a restart of VM or even ESX, and
//     the mount is stale (since Docker does not need it)
//   - we unmount / detach it
//
// If a volume is NOT mounted, but should be (refcount > 0)
//   - this should never happen since Docker using volume means bind mount is
//     active so the disk should not have been unmounted
//   - we just log an error and keep going. Recovery in this case is manual
//
// We assume that mounted (in Docker VM) and attached (to Docker VM) is the
// same. If something is attached to VM but not mounted (so from refcnt and
// mountspoint of view the volume is not used, but the VMDK is still attached
// to the VM) - we leave it to manual recovery.
//
// The RefCountsMap is safe to be used by multiple goroutines and has a single
// RWMutex to serialize operations on the map and refCounts.
// The serialization of operations per volume is assured by the volume/store
// of the docker daemon.
//

package refcount

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/filters"
	"github.com/vmware/docker-volume-vsphere/vmdk_plugin/drivers"
	"golang.org/x/net/context"
)

const (
	ApiVersion              = "v1.22"
	DockerUSocket           = "unix:///var/run/docker.sock"
	defaultSleepIntervalSec = 1

	// consts for finding and parsing linux mount information
	linuxMountsFile = "/proc/mounts"
	photonDriver    = "photon"
)

// info about individual volume ref counts and mount
type refCount struct {
	// refcount for the given volume.
	count uint

	// Is the volume mounted from OS point of view
	// (i.e. entry in /proc/mounts exists)
	mounted bool

	// Volume is mounted from this device. Used on recovery only , for info
	// purposes. Value is empty during normal operation
	dev string
}

// RefCountsMap struct
type RefCountsMap struct {
	refMap map[string]*refCount // Map of refCounts
	mtx    *sync.RWMutex        // Synchronizes RefCountsMap ops
}

var (
	// vmdk or local. We use "vmdk" only in production, but need "local" to
	// allow no-ESX test. sanity_test.go '-d' flag allows to switch it to local
	driverName string

	// header for Docker Remote API
	defaultHeaders map[string]string

	// root dir for mounted volumes
	mountRoot string
)

// local init() for initializing stuff in before running any code in this file
func init() {
	defaultHeaders = map[string]string{"User-Agent": "engine-api-client-1.0"}
}

// NewRefCountsMap - creates a new RefCountsMap
func NewRefCountsMap() *RefCountsMap {
	return &RefCountsMap{
		refMap: make(map[string]*refCount),
		mtx:    &sync.RWMutex{},
	}
}

// Creates a new refCount
func newRefCount() *refCount {
	return &refCount{
		count: 0,
	}
}

// Init Refcounts. Discover volume usage refcounts from Docker.
// This functions does not sync with mount/unmount handlers and should be called
// and completed BEFORE we start accepting Mount/unmount requests.
func (r RefCountsMap) Init(d drivers.VolumeDriver, mountDir string, name string) {
	e := os.Getenv("VDVS_DISCOVER_VOLUMES")
	if e == "" {
		log.Debug("RefCountsMap.Init: Skipping Docker volumes discovery - VDVS_DISCOVER_VOLUMES not set")
		return
	}
	c, err := client.NewClient(DockerUSocket, ApiVersion, nil, defaultHeaders)
	if err != nil {
		log.Panicf("Failed to create client for Docker at %s.( %v)",
			DockerUSocket, err)
	}
	mountRoot = mountDir
	driverName = name

	log.Infof("Getting volume data from %s", DockerUSocket)
	info, err := c.Info(context.Background())
	if err != nil {
		log.Infof("Can't connect to %s, skipping discovery", DockerUSocket)
		// TODO: Issue #369
		// Docker is not running, inform ESX to detach docker volumes, if any
		// d.detachAllVolumes()
		return
	}
	log.Debugf("Docker info: version=%s, root=%s, OS=%s",
		info.ServerVersion, info.DockerRootDir, info.OperatingSystem)

	// connects (and polls if needed) and then calls discovery
	err = r.discoverAndSync(c, d)
	if err != nil {
		log.Errorf("Failed to discover mount refcounts(%v)", err)
		return
	}

	// RLocks the RefCountsMap
	r.mtx.RLock()
	defer r.mtx.RUnlock()
	log.Infof("Discovered %d volumes in use.", len(r.refMap))
	for name, cnt := range r.refMap {
		log.Infof("Volume name=%s count=%d mounted=%t device='%s'",
			name, cnt.count, cnt.mounted, cnt.dev)
	}
}

// Returns ref count for the volume.
// If volume is not referred (not in the map), return 0
func (r RefCountsMap) GetCount(vol string) uint {
	// RLocks the RefCountsMap
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	rc := r.refMap[vol]
	if rc == nil {
		return 0
	}
	return rc.count
}

// Incr refCount for the volume vol. Creates new entry if needed.
func (r RefCountsMap) Incr(vol string) uint {
	// Locks the RefCountsMap
	r.mtx.Lock()
	defer r.mtx.Unlock()

	rc := r.refMap[vol]
	if rc == nil {
		rc = newRefCount()
		r.refMap[vol] = rc
	}
	rc.count++
	return rc.count
}

// Decr recfcount for the volume vol and returns the new count
// returns -1  for error (and resets count to 0)
// also deletes the node from the map if refcount drops to 0
func (r RefCountsMap) Decr(vol string) (uint, error) {
	// Locks the RefCountsMap
	r.mtx.Lock()
	defer r.mtx.Unlock()

	rc := r.refMap[vol]
	if rc == nil {
		return 0, fmt.Errorf("Decr: Missing refcount. name=%s", vol)
	}

	if rc.count == 0 {
		// we should NEVER get here. Even if Docker sends Unmount before Mount,
		// it should be caught in previous check. So delete the entry (in case
		// someone upstairs does 'recover', and panic.
		delete(r.refMap, vol)
		log.Warning("Decr: refcnt already 0 (rc.count=0), name=%s", vol)
		return 0, nil
	}

	rc.count--

	if rc.count < 0 {
		log.Warningf("Decr: Internal error, refcnt is negative. Trying to recover, deleting the counter - name=%s refcnt=%d", vol, rc.count)
	}
	// Deletes the refcount only if there are no references
	if rc.count <= 0 {
		delete(r.refMap, vol)
	}
	return rc.count, nil
}

// enumberates volumes and  builds RefCountsMap, then sync with mount info
func (r RefCountsMap) discoverAndSync(c *client.Client, d drivers.VolumeDriver) error {
	// we assume to  have empty refcounts. Let's enforce

	r.mtx.Lock() // Lock the RefCountsMap to purge the refcounts
	for name := range r.refMap {
		delete(r.refMap, name)
	}
	r.mtx.Unlock() // Unlock.

	filters := filters.NewArgs()
	filters.Add("status", "running")
	filters.Add("status", "paused")
	filters.Add("status", "restarting")
	containers, err := c.ContainerList(context.Background(), types.ContainerListOptions{
		All:    true,
		Filter: filters,
	})
	if err != nil {
		return err
	}

	log.Debugf("Found %d running or paused containers", len(containers))
	for _, ct := range containers {
		containerJSONInfo, err := c.ContainerInspect(context.Background(), ct.ID)
		if err != nil {
			log.Errorf("ContainerInspect failed for %s (err: %v)", ct.Names, err)
			continue
		}
		log.Debugf("  Mounts for %v", ct.Names)

		for _, mount := range containerJSONInfo.Mounts {
			if mount.Driver == driverName {
				r.Incr(mount.Name)
				log.Debugf("  name=%v (driver=%s source=%s) (%v)",
					mount.Name, mount.Driver, mount.Source, mount)
			}
		}
	}

	// Check that refcounts and actual mount info from Linux match
	// If they don't, unmount unneeded stuff, or yell if something is
	// not mounted but should be (it's error. we should not get there)

	r.getMountInfo()
	r.syncMountsWithRefCounters(d)

	return nil
}

// syncronize mount info with refcounts - and unmounts if needed
func (r RefCountsMap) syncMountsWithRefCounters(d drivers.VolumeDriver) {
	// Lock the RefCountsMap
	r.mtx.Lock()
	defer r.mtx.Unlock()

	for vol, cnt := range r.refMap {
		f := log.Fields{
			"name":    vol,
			"refcnt":  cnt.count,
			"mounted": cnt.mounted,
			"dev":     cnt.dev,
		}

		log.WithFields(f).Debug("Refcnt record: ")
		if cnt.mounted == true {
			if cnt.count == 0 {
				// Volume mounted but not used - UNMOUNT and DETACH !
				log.WithFields(f).Info("Initiating recovery unmount. ")
				err := d.UnmountVolume(vol)
				if err != nil {
					log.Warning("Failed to unmount - manual recovery may be needed")
				}
			}
		} else {
			if cnt.count == 0 {
				// volume unmounted AND refcount 0.  We should NEVER get here
				// since unmounted and recount==0 volumes should have no record
				// in the map. Something went seriously wrong in the code.
				log.WithFields(f).Panic("Internal failure: record should not exist. ")
			} else {
				// No mounts, but Docker tells we have refcounts.
				// It could happen when Docker runs a container with a volume
				// but not using files on the volumes, and the volume is (manually?)
				// unmounted. Unlikely but possible. Mount !
				log.WithFields(f).Warning("Initiating recovery mount. ")
				status, err := d.GetVolume(vol)
				if err != nil {
					log.Warning("Failed to mount - manual recovery may be needed")
				} else {
					//Ensure the refcount map has this disk ID
					id := ""
					exists := false
					if driverName == photonDriver {
						if id, exists = status["ID"].(string); !exists {
							log.Warning("Failed to disk ID for photon disk cannot mount in use disk")
						}
					}

					isReadOnly := false
					if access, exists := status["access"]; exists {
						if access == "read-only" {
							isReadOnly = true
						}
					}
					_, err = d.MountVolume(vol, status["fstype"].(string), id, isReadOnly, false)
					if err != nil {
						log.Warning("Failed to mount - manual recovery may be needed")
					}
				}
			}
		}
	}
}

// scans /proc/mounts and updates refcount map witn mounted volumes
func (r RefCountsMap) getMountInfo() error {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	data, err := ioutil.ReadFile(linuxMountsFile)
	if err != nil {
		log.Errorf("Can't get info from %s (%v)", linuxMountsFile, err)
		return err
	}

	for _, line := range strings.Split(string(data), "\n") {
		field := strings.Fields(line)
		if len(field) < 2 {
			continue // skip empty line and lines too short to have our mount
		}
		// fields format: [/dev/sdb /mnt/vmdk/vol1 ext2 rw,relatime 0 0]
		if filepath.Dir(field[1]) != mountRoot {
			continue
		}
		volName := filepath.Base(field[1])
		refInfo := r.refMap[volName]
		if refInfo == nil {
			refInfo = newRefCount()
		}
		refInfo.mounted = true
		refInfo.dev = field[0]
		r.refMap[volName] = refInfo
		log.Debugf("Found '%s' in /proc/mount, ref=(%#v)", volName, refInfo)
	}

	return nil
}
