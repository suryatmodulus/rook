/*
Copyright 2016 The Rook Authors. All rights reserved.

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

package client

import (
	ctx "context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/util/exec"
	"k8s.io/apimachinery/pkg/util/wait"
)

type MDSDump struct {
	Standbys    []MDSStandBy `json:"standbys"`
	FileSystems []MDSMap     `json:"filesystems"`
}

type MDSStandBy struct {
	Name string `json:"name"`
	Rank int    `json:"rank"`
}

// CephFilesystem is a representation of the json structure returned by 'ceph fs ls'
type CephFilesystem struct {
	Name           string   `json:"name"`
	MetadataPool   string   `json:"metadata_pool"`
	MetadataPoolID int      `json:"metadata_pool_id"`
	DataPools      []string `json:"data_pools"`
	DataPoolIDs    []int    `json:"data_pool_ids"`
}

// CephFilesystemDetails is a representation of the main json structure returned by 'ceph fs get'
type CephFilesystemDetails struct {
	ID     int    `json:"id"`
	MDSMap MDSMap `json:"mdsmap"`
}

// MDSMap is a representation of the mds map sub-structure returned by 'ceph fs get'
type MDSMap struct {
	FilesystemName string             `json:"fs_name"`
	Enabled        bool               `json:"enabled"`
	Root           int                `json:"root"`
	TableServer    int                `json:"tableserver"`
	MaxMDS         int                `json:"max_mds"`
	In             []int              `json:"in"`
	Up             map[string]int     `json:"up"`
	MetadataPool   int                `json:"metadata_pool"`
	DataPools      []int              `json:"data_pools"`
	Failed         []int              `json:"failed"`
	Damaged        []int              `json:"damaged"`
	Stopped        []int              `json:"stopped"`
	Info           map[string]MDSInfo `json:"info"`
}

// MDSInfo is a representation of the individual mds daemon sub-sub-structure returned by 'ceph fs get'
type MDSInfo struct {
	GID     int    `json:"gid"`
	Name    string `json:"name"`
	Rank    int    `json:"rank"`
	State   string `json:"state"`
	Address string `json:"addr"`
}

// ListFilesystems lists all filesystems provided by the Ceph cluster.
func ListFilesystems(context *clusterd.Context, clusterInfo *ClusterInfo) ([]CephFilesystem, error) {
	args := []string{"fs", "ls"}
	buf, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list filesystems")
	}

	var filesystems []CephFilesystem
	err = json.Unmarshal(buf, &filesystems)
	if err != nil {
		return nil, errors.Wrapf(err, "unmarshal failed raw buffer response %s", string(buf))
	}

	return filesystems, nil
}

var GetFilesystem = getFilesystem

// getFilesystem gets detailed status information about a Ceph filesystem.
func getFilesystem(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string) (*CephFilesystemDetails, error) {
	args := []string{"fs", "get", fsName}
	buf, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return nil, err
	}

	var fs CephFilesystemDetails
	err = json.Unmarshal(buf, &fs)
	if err != nil {
		return nil, errors.Wrapf(err, "unmarshal failed raw buffer response %s", string(buf))
	}

	return &fs, nil
}

// AllowStandbyReplay gets detailed status information about a Ceph filesystem.
func AllowStandbyReplay(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string, allowStandbyReplay bool) error {
	logger.Infof("setting allow_standby_replay to %t for filesystem %q", allowStandbyReplay, fsName)
	args := []string{"fs", "set", fsName, "allow_standby_replay", strconv.FormatBool(allowStandbyReplay)}
	_, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrapf(err, "failed to set allow_standby_replay to filesystem %s", fsName)
	}

	return nil
}

// CreateFilesystem performs software configuration steps for Ceph to provide a new filesystem.
func CreateFilesystem(context *clusterd.Context, clusterInfo *ClusterInfo, name, metadataPool string, dataPools []string) error {
	if len(dataPools) == 0 {
		return errors.New("at least one data pool is required")
	}

	logger.Infof("creating filesystem %q with metadata pool %q and data pools %v", name, metadataPool, dataPools)
	var err error

	// enable multiple file systems in case this is not the first
	args := []string{"fs", "flag", "set", "enable_multiple", "true", confirmFlag}
	_, err = NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrap(err, "failed to enable multiple file systems")
	}

	// create the filesystem
	args = []string{"fs", "new", name, metadataPool, dataPools[0]}

	_, err = NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrapf(err, "failed enabling ceph fs %q", name)
	}

	// add each additional pool
	for i := 1; i < len(dataPools); i++ {
		err = AddDataPoolToFilesystem(context, clusterInfo, name, dataPools[i])
		if err != nil {
			logger.Errorf("%v", err)
		}
	}

	return nil
}

// AddDataPoolToFilesystem associates the provided data pool with the filesystem.
func AddDataPoolToFilesystem(context *clusterd.Context, clusterInfo *ClusterInfo, name, poolName string) error {
	args := []string{"fs", "add_data_pool", name, poolName}
	_, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		// Reef disallows calling add_data_pool for a pool that has already
		// been added, so ignore the error code.
		// Previous releases do not return an error when an existing data pool is added.
		if clusterInfo.CephVersion.IsAtLeastReef() {
			if code, ok := exec.ExitStatus(err); ok && code == int(syscall.EINVAL) {
				return nil
			}
		}
		return errors.Wrapf(err, "failed to add pool %q to file system %q. (%v)", poolName, name, err)
	}
	return nil
}

// SetNumMDSRanks sets the number of mds ranks (max_mds) for a Ceph filesystem.
func SetNumMDSRanks(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string, activeMDSCount int32) error {
	// Always tell Ceph to set the new max_mds value
	args := []string{"fs", "set", fsName, "max_mds", strconv.Itoa(int(activeMDSCount))}
	if _, err := NewCephCommand(context, clusterInfo, args).Run(); err != nil {
		return errors.Wrapf(err, "failed to set filesystem %s num mds ranks (max_mds) to %d", fsName, activeMDSCount)
	}
	return nil
}

// FailAllStandbyReplayMDS: fail all mds in up:standby-replay state
func FailAllStandbyReplayMDS(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string) error {
	fs, err := getFilesystem(context, clusterInfo, fsName)
	if err != nil {
		return errors.Wrapf(err, "failed to fail standby-replay MDSes for fs %q", fsName)
	}
	for _, info := range fs.MDSMap.Info {
		if info.State == "up:standby-replay" {
			if err := failMDS(context, clusterInfo, info.GID); err != nil {
				return errors.Wrapf(err, "failed to fail MDS %q for filesystem %q in up:standby-replay state", info.Name, fsName)
			}
		}
	}
	return nil
}

// GetMdsIdByRank get mds ID from the given rank
func GetMdsIdByRank(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string, rank int32) (string, error) {
	fs, err := getFilesystem(context, clusterInfo, fsName)
	if err != nil {
		return "", errors.Wrap(err, "failed to get ceph fs dump")
	}
	gid, ok := fs.MDSMap.Up[fmt.Sprintf("mds_%d", rank)]
	if !ok {
		return "", errors.Errorf("failed to get mds gid from rank %d", rank)
	}
	info, ok := fs.MDSMap.Info[fmt.Sprintf("gid_%d", gid)]
	if !ok {
		return "", errors.Errorf("failed to get mds info for rank %d", rank)
	}
	return info.Name, nil
}

// WaitForActiveRanks waits for the filesystem's number of active ranks to equal the desired count.
// It times out with an error if the number of active ranks does not become desired in time.
// Param 'moreIsOkay' will allow success condition if num of ranks is more than active count given.
func WaitForActiveRanks(
	context *clusterd.Context,
	clusterInfo *ClusterInfo, fsName string,
	desiredActiveRanks int32, moreIsOkay bool, timeout time.Duration,
) error {
	countText := fmt.Sprintf("%d", desiredActiveRanks)
	if moreIsOkay {
		// If it's okay to have more active ranks than desired, indicate so in log messages
		countText = fmt.Sprintf("%d or more", desiredActiveRanks)
	}
	logger.Infof("waiting %.2f second(s) for number of active mds daemons for fs %s to become %s",
		float64(timeout/time.Second), fsName, countText)
	err := wait.PollUntilContextTimeout(clusterInfo.Context, 3*time.Second, timeout, true, func(ctx ctx.Context) (bool, error) {
		fs, err := getFilesystem(context, clusterInfo, fsName)
		if err != nil {
			logger.Errorf(
				"Error getting filesystem %q details while waiting for num mds ranks to become %d. %v",
				fsName, desiredActiveRanks, err)
		} else if fs.MDSMap.MaxMDS == int(desiredActiveRanks) &&
			activeRanksSuccess(len(fs.MDSMap.Up), int(desiredActiveRanks), moreIsOkay) {
			// Both max_mds and number of up MDS daemons must equal desired number of ranks to
			// prevent a false positive when Ceph has got the correct number of mdses up but is
			// trying to change the number of mdses up to an undesired number.
			logger.Debugf("mds ranks for filesystem %q successfully became %d", fsName, desiredActiveRanks)
			return true, nil
			// continue to inf loop after send ready; only return when get quit signal to
			// prevent deadlock
		}
		return false, nil
	})
	if err != nil {
		return errors.Errorf("timeout waiting for number active mds daemons for filesystem %q to become %q",
			fsName, countText)
	}
	return nil
}

func activeRanksSuccess(upCount, desiredRanks int, moreIsOkay bool) bool {
	if moreIsOkay {
		return upCount >= desiredRanks
	}
	return upCount == desiredRanks
}

// MarkFilesystemAsDown marks a Ceph filesystem as down.
func MarkFilesystemAsDown(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string) error {
	args := []string{"fs", "set", fsName, "cluster_down", "true"}
	_, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrapf(err, "failed to set file system %s to cluster_down", fsName)
	}
	return nil
}

// failMDS instructs Ceph to fail an mds daemon.
func failMDS(context *clusterd.Context, clusterInfo *ClusterInfo, gid int) error {
	args := []string{"mds", "fail", strconv.Itoa(gid)}
	_, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrapf(err, "failed to fail mds %d", gid)
	}
	return nil
}

// FailFilesystem efficiently brings down the filesystem by marking the filesystem as down
// and failing the MDSes using a single Ceph command.
func FailFilesystem(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string) error {
	args := []string{"fs", "fail", fsName}
	_, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrapf(err, "failed to fail filesystem %s", fsName)
	}
	return nil
}

// RemoveFilesystem performs software configuration steps to remove a Ceph filesystem and its
// backing pools.
func RemoveFilesystem(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string, preservePoolsOnDelete bool) error {
	fs, err := getFilesystem(context, clusterInfo, fsName)
	if err != nil {
		return errors.Wrapf(err, "filesystem %s not found", fsName)
	}

	args := []string{"fs", "rm", fsName, confirmFlag}
	_, err = NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return errors.Wrapf(err, "Failed to delete ceph fs %s", fsName)
	}

	if !preservePoolsOnDelete {
		err = deleteFSPools(context, clusterInfo, fs)
		if err != nil {
			return errors.Wrapf(err, "failed to delete fs %s pools", fsName)
		}
	} else {
		logger.Infof("PreservePoolsOnDelete is set in filesystem %s. Pools not deleted", fsName)
	}

	return nil
}

func deleteFSPools(context *clusterd.Context, clusterInfo *ClusterInfo, fs *CephFilesystemDetails) error {
	poolNames, err := GetPoolNamesByID(context, clusterInfo)
	if err != nil {
		return errors.Wrap(err, "failed to get pool names")
	}

	var lastErr error = nil

	// delete the metadata pool
	if err := deleteFSPool(context, clusterInfo, poolNames, fs.MDSMap.MetadataPool); err != nil {
		lastErr = err
	}

	// delete the data pools
	for _, poolID := range fs.MDSMap.DataPools {
		if err := deleteFSPool(context, clusterInfo, poolNames, poolID); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

func deleteFSPool(context *clusterd.Context, clusterInfo *ClusterInfo, poolNames map[int]string, id int) error {
	name, ok := poolNames[id]
	if !ok {
		return errors.Errorf("pool %d not found", id)
	}
	return DeletePool(context, clusterInfo, name)
}

// WaitForNoStandbys waits for all standbys go away
func WaitForNoStandbys(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string, retryInterval, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(clusterInfo.Context, retryInterval, timeout, true, func(ctx ctx.Context) (bool, error) {
		mdsDump, err := GetMDSDump(context, clusterInfo)
		if err != nil {
			logger.Errorf("failed to get fs dump. %v", err)
			return false, nil
		}
		return !filesystemHasStandby(mdsDump, fsName), nil
	})
	if err != nil {
		return errors.Wrap(err, "timeout waiting for no standbys")
	}
	return nil
}

func filesystemHasStandby(dump *MDSDump, fsName string) bool {
	for _, standby := range dump.Standbys {
		// The mds dump does not explicitly return the name of the filesystem that the
		// daemon belongs to, so the matching to the filesystem name is based on the mds daemon name
		// with a regular expression comparison with the expected suffix.
		// For example, if the filesystem is "myfs", the standby name may be "myfs-a" or "myfs-b".
		matchString := fmt.Sprintf("^%s-[a-z]{1}$", fsName)
		matched, _ := regexp.MatchString(matchString, standby.Name)
		if matched {
			return true
		}
	}
	return false
}

func GetMDSDump(context *clusterd.Context, clusterInfo *ClusterInfo) (*MDSDump, error) {
	args := []string{"fs", "dump"}
	cmd := NewCephCommand(context, clusterInfo, args)
	buf, err := cmd.Run()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dump fs info")
	}
	var dump MDSDump
	if err := json.Unmarshal(buf, &dump); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal fs dump. %s", buf)
	}
	return &dump, nil
}

// SubvolumeGroup is a representation of a Ceph filesystem subvolume group.
type SubvolumeGroup struct {
	Name string `json:"name"`
}

// SubvolumeGroupList is the representation Ceph returns when listing Ceph filesystem subvolume groups.
type SubvolumeGroupList []SubvolumeGroup

// ListSubvolumeGroups lists all subvolume groups in a given filesystem by name.
// Times out after 5 seconds.
var ListSubvolumeGroups = listSubvolumeGroups

// with above, allow this to be overridden for unit testing
func listSubvolumeGroups(context *clusterd.Context, clusterInfo *ClusterInfo, fsName string) (SubvolumeGroupList, error) {
	svgs := SubvolumeGroupList{}

	args := []string{"fs", "subvolumegroup", "ls", fsName}
	cmd := NewCephCommand(context, clusterInfo, args)
	buf, err := cmd.RunWithTimeout(exec.CephCommandsTimeout)
	if err != nil {
		return svgs, errors.Wrapf(err, "failed to list subvolumegroups in filesystem %q", fsName)
	}

	if err := json.Unmarshal(buf, &svgs); err != nil {
		return svgs, errors.Wrapf(err, "failed to unmarshal subvolumegroup list for filesystem %q", fsName)
	}

	return svgs, nil
}

// Subvolume is a representation of a Ceph filesystem subvolume.
type Subvolume struct {
	Name string `json:"name"`
}

// SubvolumeList is the representation Ceph returns when listing Ceph filesystem subvolumes.
type SubvolumeList []Subvolume

// NoSubvolumeGroup can be passed to commands that operate on filesystem subvolume groups to
// indicate the case where there is no subvolume group.
const NoSubvolumeGroup = ""

// ListSubvolumesInGroup lists all subvolumes present in the given filesystem's subvolume group by
// name. If groupName is empty, list subvolumes that are not in any group.
var ListSubvolumesInGroup = listSubvolumesInGroup

// with above, allow this to be overridden for unit testing
func listSubvolumesInGroup(context *clusterd.Context, clusterInfo *ClusterInfo, fsName, groupName string) (SubvolumeList, error) {
	svs := SubvolumeList{}

	args := []string{"fs", "subvolume", "ls", fsName}
	if groupName != NoSubvolumeGroup {
		args = append(args, groupName)
	}
	cmd := NewCephCommand(context, clusterInfo, args)
	// if the command takes a long time to run, this is a good indication that there are *many*
	// subvolumes present
	buf, err := cmd.RunWithTimeout(exec.CephCommandsTimeout)
	if err != nil {
		return svs, errors.Wrapf(err, "failed to list subvolumes in filesystem %q subvolume group %q", fsName, groupName)
	}
	if err := json.Unmarshal(buf, &svs); err != nil {
		return svs, errors.Wrapf(err, "failed to unmarshal subvolume list for filesystem %q subvolume group %q", fsName, groupName)
	}
	return svs, nil
}

// SubVolumeSnapshot represents snapshot of a cephFS subvolume
type SubVolumeSnapshot struct {
	Name string `json:"name"`
}

// SubVolumeSnapshots is the list of snapshots in a CephFS subvolume
type SubVolumeSnapshots []SubVolumeSnapshot

// ListSubVolumeSnaphots lists all the subvolume snapshots present in the subvolume in the given filesystem's subvolume group.
var ListSubVolumeSnapshots = listSubVolumeSnapshots

func listSubVolumeSnapshots(context *clusterd.Context, clusterInfo *ClusterInfo, fsName, subVolumeName, groupName string) (SubVolumeSnapshots, error) {
	svs := SubVolumeSnapshots{}
	args := []string{"fs", "subvolume", "snapshot", "ls", fsName, subVolumeName, "--group_name", groupName}
	cmd := NewCephCommand(context, clusterInfo, args)
	buf, err := cmd.RunWithTimeout(exec.CephCommandsTimeout)
	if err != nil {
		return svs, errors.Wrapf(err, "failed to list subvolumes in filesystem %q subvolume group %q", fsName, groupName)
	}

	if err := json.Unmarshal(buf, &svs); err != nil {
		return svs, errors.Wrapf(err, "failed to unmarshal snapshots for subvolume %q for filesystem %q subvolume group %q", subVolumeName, fsName, groupName)
	}

	return svs, nil
}

// SubVolumeSnapshotPendingClones refers to all the pending clones available in a cephFS subvolume snapshot
type SubVolumeSnapshotPendingClones struct {
	Clones []struct {
		Name string `json:"name"`
	} `json:"pending_clones"`
}

var ListSubVolumeSnapshotPendingClones = listSubVolumeSnapshotPendingClones

// listSubVolumeSnapshotPendingClones lists all the pending clones available in a cephFS subvolume snapshot
func listSubVolumeSnapshotPendingClones(context *clusterd.Context, clusterInfo *ClusterInfo, fsName, subVolumeName, snap, groupName string) (SubVolumeSnapshotPendingClones, error) {
	pendingClones := SubVolumeSnapshotPendingClones{}
	args := []string{"fs", "subvolume", "snapshot", "info", fsName, subVolumeName, snap, "--group_name", groupName}
	cmd := NewCephCommand(context, clusterInfo, args)
	buf, err := cmd.RunWithTimeout(exec.CephCommandsTimeout)
	if err != nil {
		return pendingClones, errors.Wrapf(err, "failed to list pending clones available for snapshot %q in filesystem %q in subvolume group %q", snap, fsName, groupName)
	}

	if err := json.Unmarshal(buf, &pendingClones); err != nil {
		return pendingClones, errors.Wrapf(err, "failed to unmarshal pending clones list for for snapshot %q in filesystem %q subvolume group %q", snap, fsName, groupName)
	}

	return pendingClones, nil
}
