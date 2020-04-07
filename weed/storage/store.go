package storage

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/stats"
	"github.com/chrislusf/seaweedfs/weed/storage/erasure_coding"
	"github.com/chrislusf/seaweedfs/weed/storage/needle"
	"github.com/chrislusf/seaweedfs/weed/storage/super_block"
	. "github.com/chrislusf/seaweedfs/weed/storage/types"
)

const (
	MAX_TTL_VOLUME_REMOVAL_DELAY = 10 // 10 minutes
)

/*
 * A VolumeServer contains one Store
 */
type Store struct {
	MasterAddress       string
	grpcDialOption      grpc.DialOption
	volumeSizeLimit     uint64 //read from the master
	Ip                  string
	Port                int
	PublicUrl           string
	Locations           []*DiskLocation
	dataCenter          string //optional informaton, overwriting master setting if exists
	rack                string //optional information, overwriting master setting if exists
	connected           bool
	NeedleMapType       NeedleMapType
	NewVolumesChan      chan master_pb.VolumeShortInformationMessage
	DeletedVolumesChan  chan master_pb.VolumeShortInformationMessage
	NewEcShardsChan     chan master_pb.VolumeEcShardInformationMessage
	DeletedEcShardsChan chan master_pb.VolumeEcShardInformationMessage
}

func (s *Store) String() (str string) {
	str = fmt.Sprintf("Ip:%s, Port:%d, PublicUrl:%s, dataCenter:%s, rack:%s, connected:%v, volumeSizeLimit:%d", s.Ip, s.Port, s.PublicUrl, s.dataCenter, s.rack, s.connected, s.GetVolumeSizeLimit())
	return
}

func NewStore(grpcDialOption grpc.DialOption, port int, ip, publicUrl string, dirnames []string, maxVolumeCounts []int, needleMapKind NeedleMapType) (s *Store) {
	s = &Store{grpcDialOption: grpcDialOption, Port: port, Ip: ip, PublicUrl: publicUrl, NeedleMapType: needleMapKind}
	s.Locations = make([]*DiskLocation, 0)
	for i := 0; i < len(dirnames); i++ {
		location := NewDiskLocation(dirnames[i], maxVolumeCounts[i])
		location.loadExistingVolumes(needleMapKind)
		s.Locations = append(s.Locations, location)
		stats.VolumeServerMaxVolumeCounter.Add(float64(maxVolumeCounts[i]))
	}
	s.NewVolumesChan = make(chan master_pb.VolumeShortInformationMessage, 3)
	s.DeletedVolumesChan = make(chan master_pb.VolumeShortInformationMessage, 3)

	s.NewEcShardsChan = make(chan master_pb.VolumeEcShardInformationMessage, 3)
	s.DeletedEcShardsChan = make(chan master_pb.VolumeEcShardInformationMessage, 3)

	return
}
func (s *Store) AddVolume(volumeId needle.VolumeId, collection string, needleMapKind NeedleMapType, replicaPlacement string, ttlString string, preallocate int64, MemoryMapMaxSizeMb uint32) error {
	rt, e := super_block.NewReplicaPlacementFromString(replicaPlacement)
	if e != nil {
		return e
	}
	ttl, e := needle.ReadTTL(ttlString)
	if e != nil {
		return e
	}
	e = s.addVolume(volumeId, collection, needleMapKind, rt, ttl, preallocate, MemoryMapMaxSizeMb)
	return e
}
func (s *Store) DeleteCollection(collection string) (e error) {
	for _, location := range s.Locations {
		e = location.DeleteCollectionFromDiskLocation(collection)
		if e != nil {
			return
		}
		// let the heartbeat send the list of volumes, instead of sending the deleted volume ids to DeletedVolumesChan
	}
	return
}

func (s *Store) findVolume(vid needle.VolumeId) *Volume {
	for _, location := range s.Locations {
		if v, found := location.FindVolume(vid); found {
			return v
		}
	}
	return nil
}
func (s *Store) FindFreeLocation() (ret *DiskLocation) {
	max := 0
	for _, location := range s.Locations {
		currentFreeCount := location.MaxVolumeCount - location.VolumesLen()
		currentFreeCount *= erasure_coding.DataShardsCount
		currentFreeCount -= location.EcVolumesLen()
		currentFreeCount /= erasure_coding.DataShardsCount
		if currentFreeCount > max {
			max = currentFreeCount
			ret = location
		}
	}
	return ret
}
func (s *Store) addVolume(vid needle.VolumeId, collection string, needleMapKind NeedleMapType, replicaPlacement *super_block.ReplicaPlacement, ttl *needle.TTL, preallocate int64, memoryMapMaxSizeMb uint32) error {
	if s.findVolume(vid) != nil {
		return fmt.Errorf("Volume Id %d already exists!", vid)
	}
	if location := s.FindFreeLocation(); location != nil {
		glog.V(0).Infof("In dir %s adds volume:%v collection:%s replicaPlacement:%v ttl:%v",
			location.Directory, vid, collection, replicaPlacement, ttl)
		if volume, err := NewVolume(location.Directory, collection, vid, needleMapKind, replicaPlacement, ttl, preallocate, memoryMapMaxSizeMb); err == nil {
			location.SetVolume(vid, volume)
			glog.V(0).Infof("add volume %d", vid)
			s.NewVolumesChan <- master_pb.VolumeShortInformationMessage{
				Id:               uint32(vid),
				Collection:       collection,
				ReplicaPlacement: uint32(replicaPlacement.Byte()),
				Version:          uint32(volume.Version()),
				Ttl:              ttl.ToUint32(),
			}
			return nil
		} else {
			return err
		}
	}
	return fmt.Errorf("No more free space left")
}

func (s *Store) VolumeInfos() (allStats []*VolumeInfo) {
	for _, location := range s.Locations {
		stats := collectStatsForOneLocation(location)
		allStats = append(allStats, stats...)
	}
	sortVolumeInfos(allStats)
	return allStats
}

func collectStatsForOneLocation(location *DiskLocation) (stats []*VolumeInfo) {
	location.volumesLock.RLock()
	defer location.volumesLock.RUnlock()

	for k, v := range location.volumes {
		s := collectStatForOneVolume(k, v)
		stats = append(stats, s)
	}
	return stats
}

func collectStatForOneVolume(vid needle.VolumeId, v *Volume) (s *VolumeInfo) {

	s = &VolumeInfo{
		Id:               vid,
		Collection:       v.Collection,
		ReplicaPlacement: v.ReplicaPlacement,
		Version:          v.Version(),
		ReadOnly:         v.IsReadOnly(),
		Ttl:              v.Ttl,
		CompactRevision:  uint32(v.CompactionRevision),
	}
	s.RemoteStorageName, s.RemoteStorageKey = v.RemoteStorageNameKey()

	v.dataFileAccessLock.RLock()
	defer v.dataFileAccessLock.RUnlock()

	if v.nm == nil {
		return
	}

	s.FileCount = v.nm.FileCount()
	s.DeleteCount = v.nm.DeletedCount()
	s.DeletedByteCount = v.nm.DeletedSize()
	s.Size = v.nm.ContentSize()

	return
}

func (s *Store) SetDataCenter(dataCenter string) {
	s.dataCenter = dataCenter
}
func (s *Store) SetRack(rack string) {
	s.rack = rack
}

func (s *Store) CollectHeartbeat() *master_pb.Heartbeat {
	var volumeMessages []*master_pb.VolumeInformationMessage
	maxVolumeCount := 0
	var maxFileKey NeedleId
	collectionVolumeSize := make(map[string]uint64)
	for _, location := range s.Locations {
		var deleteVids []needle.VolumeId
		maxVolumeCount = maxVolumeCount + location.MaxVolumeCount
		location.volumesLock.RLock()
		for _, v := range location.volumes {
			if maxFileKey < v.MaxFileKey() {
				maxFileKey = v.MaxFileKey()
			}
			if !v.expired(s.GetVolumeSizeLimit()) {
				volumeMessages = append(volumeMessages, v.ToVolumeInformationMessage())
			} else {
				if v.expiredLongEnough(MAX_TTL_VOLUME_REMOVAL_DELAY) {
					deleteVids = append(deleteVids, v.Id)
				} else {
					glog.V(0).Infoln("volume", v.Id, "is expired.")
				}
			}
			fileSize, _, _ := v.FileStat()
			collectionVolumeSize[v.Collection] += fileSize
		}
		location.volumesLock.RUnlock()

		if len(deleteVids) > 0 {
			// delete expired volumes.
			location.volumesLock.Lock()
			for _, vid := range deleteVids {
				location.deleteVolumeById(vid)
				glog.V(0).Infoln("volume", vid, "is deleted.")
			}
			location.volumesLock.Unlock()
		}
	}

	for col, size := range collectionVolumeSize {
		stats.VolumeServerDiskSizeGauge.WithLabelValues(col, "normal").Set(float64(size))
	}

	return &master_pb.Heartbeat{
		Ip:             s.Ip,
		Port:           uint32(s.Port),
		PublicUrl:      s.PublicUrl,
		MaxVolumeCount: uint32(maxVolumeCount),
		MaxFileKey:     NeedleIdToUint64(maxFileKey),
		DataCenter:     s.dataCenter,
		Rack:           s.rack,
		Volumes:        volumeMessages,
		HasNoVolumes:   len(volumeMessages) == 0,
	}

}

func (s *Store) Close() {
	for _, location := range s.Locations {
		location.Close()
	}
}

func (s *Store) WriteVolumeNeedle(i needle.VolumeId, n *needle.Needle) (isUnchanged bool, err error) {
	if v := s.findVolume(i); v != nil {
		if v.IsReadOnly() {
			err = fmt.Errorf("volume %d is read only", i)
			return
		}
		// using len(n.Data) here instead of n.Size before n.Size is populated in v.writeNeedle(n)
		if MaxPossibleVolumeSize >= v.ContentSize()+uint64(needle.GetActualSize(uint32(len(n.Data)), v.Version())) {
			_, _, isUnchanged, err = v.writeNeedle(n)
		} else {
			err = fmt.Errorf("volume size limit %d exceeded! current size is %d", s.GetVolumeSizeLimit(), v.ContentSize())
		}
		return
	}
	glog.V(0).Infoln("volume", i, "not found!")
	err = fmt.Errorf("volume %d not found on %s:%d", i, s.Ip, s.Port)
	return
}

func (s *Store) DeleteVolumeNeedle(i needle.VolumeId, n *needle.Needle) (uint32, error) {
	if v := s.findVolume(i); v != nil {
		if v.noWriteOrDelete {
			return 0, fmt.Errorf("volume %d is read only", i)
		}
		if MaxPossibleVolumeSize >= v.ContentSize()+uint64(needle.GetActualSize(0, v.Version())) {
			return v.deleteNeedle(n)
		} else {
			return 0, fmt.Errorf("volume size limit %d exceeded! current size is %d", s.GetVolumeSizeLimit(), v.ContentSize())
		}
	}
	return 0, fmt.Errorf("volume %d not found on %s:%d", i, s.Ip, s.Port)
}

func (s *Store) ReadVolumeNeedle(i needle.VolumeId, n *needle.Needle) (int, error) {
	if v := s.findVolume(i); v != nil {
		return v.readNeedle(n)
	}
	return 0, fmt.Errorf("volume %d not found", i)
}
func (s *Store) GetVolume(i needle.VolumeId) *Volume {
	return s.findVolume(i)
}

func (s *Store) HasVolume(i needle.VolumeId) bool {
	v := s.findVolume(i)
	return v != nil
}

func (s *Store) MarkVolumeReadonly(i needle.VolumeId) error {
	v := s.findVolume(i)
	if v == nil {
		return fmt.Errorf("volume %d not found", i)
	}
	v.noWriteOrDelete = true
	return nil
}

func (s *Store) MountVolume(i needle.VolumeId) error {
	for _, location := range s.Locations {
		if found := location.LoadVolume(i, s.NeedleMapType); found == true {
			glog.V(0).Infof("mount volume %d", i)
			v := s.findVolume(i)
			s.NewVolumesChan <- master_pb.VolumeShortInformationMessage{
				Id:               uint32(v.Id),
				Collection:       v.Collection,
				ReplicaPlacement: uint32(v.ReplicaPlacement.Byte()),
				Version:          uint32(v.Version()),
				Ttl:              v.Ttl.ToUint32(),
			}
			return nil
		}
	}

	return fmt.Errorf("volume %d not found on disk", i)
}

func (s *Store) UnmountVolume(i needle.VolumeId) error {
	v := s.findVolume(i)
	if v == nil {
		return nil
	}
	message := master_pb.VolumeShortInformationMessage{
		Id:               uint32(v.Id),
		Collection:       v.Collection,
		ReplicaPlacement: uint32(v.ReplicaPlacement.Byte()),
		Version:          uint32(v.Version()),
		Ttl:              v.Ttl.ToUint32(),
	}

	for _, location := range s.Locations {
		if err := location.UnloadVolume(i); err == nil {
			glog.V(0).Infof("UnmountVolume %d", i)
			s.DeletedVolumesChan <- message
			return nil
		}
	}

	return fmt.Errorf("volume %d not found on disk", i)
}

func (s *Store) DeleteVolume(i needle.VolumeId) error {
	v := s.findVolume(i)
	if v == nil {
		return nil
	}
	message := master_pb.VolumeShortInformationMessage{
		Id:               uint32(v.Id),
		Collection:       v.Collection,
		ReplicaPlacement: uint32(v.ReplicaPlacement.Byte()),
		Version:          uint32(v.Version()),
		Ttl:              v.Ttl.ToUint32(),
	}
	for _, location := range s.Locations {
		if error := location.deleteVolumeById(i); error == nil {
			glog.V(0).Infof("DeleteVolume %d", i)
			s.DeletedVolumesChan <- message
			return nil
		}
	}

	return fmt.Errorf("volume %d not found on disk", i)
}

func (s *Store) ConfigureVolume(i needle.VolumeId, replication string) error {

	for _, location := range s.Locations {
		fileInfo, found := location.LocateVolume(i)
		if !found {
			continue
		}
		// load, modify, save
		baseFileName := strings.TrimSuffix(fileInfo.Name(), filepath.Ext(fileInfo.Name()))
		vifFile := filepath.Join(location.Directory, baseFileName+".vif")
		volumeInfo, _, err := pb.MaybeLoadVolumeInfo(vifFile)
		if err != nil {
			return fmt.Errorf("volume %d fail to load vif", i)
		}
		volumeInfo.Replication = replication
		err = pb.SaveVolumeInfo(vifFile, volumeInfo)
		if err != nil {
			return fmt.Errorf("volume %d fail to save vif", i)
		}
		return nil
	}

	return fmt.Errorf("volume %d not found on disk", i)
}

func (s *Store) SetVolumeSizeLimit(x uint64) {
	atomic.StoreUint64(&s.volumeSizeLimit, x)
}

func (s *Store) GetVolumeSizeLimit() uint64 {
	return atomic.LoadUint64(&s.volumeSizeLimit)
}

func (s *Store) MaybeAdjustVolumeMax() (hasChanges bool) {
	volumeSizeLimit := s.GetVolumeSizeLimit()
	for _, diskLocation := range s.Locations {
		if diskLocation.MaxVolumeCount == 0 {
			diskStatus := stats.NewDiskStatus(diskLocation.Directory)
			unusedSpace := diskLocation.UnUsedSpace(volumeSizeLimit)
			unclaimedSpaces := int64(diskStatus.Free) - int64(unusedSpace)
			volCount := diskLocation.VolumesLen()
			maxVolumeCount := volCount
			if unclaimedSpaces > int64(volumeSizeLimit) {
				maxVolumeCount += int(uint64(unclaimedSpaces)/volumeSizeLimit) - 1
			}
			diskLocation.MaxVolumeCount = maxVolumeCount
			glog.V(0).Infof("disk %s max %d unclaimedSpace:%dMB, unused:%dMB volumeSizeLimit:%d/MB",
				diskLocation.Directory, maxVolumeCount, unclaimedSpaces/1024/1024, unusedSpace/1024/1024, volumeSizeLimit/1024/1024)
			hasChanges = true
		}
	}
	return
}
