package filesys

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/karlseguin/ccache"
	"google.golang.org/grpc"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/pb/pb_cache"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

type Option struct {
	FilerGrpcAddress     string
	GrpcDialOption       grpc.DialOption
	FilerMountRootPath   string
	Collection           string
	Replication          string
	TtlSec               int32
	ChunkSizeLimit       int64
	ChunkCacheCountLimit int64
	DataCenter           string
	DirListCacheLimit    int64
	EntryCacheTtl        time.Duration
	Umask                os.FileMode

	MountUid   uint32
	MountGid   uint32
	MountMode  os.FileMode
	MountCtime time.Time
	MountMtime time.Time

	OutsideContainerClusterMode bool // whether the mount runs outside SeaweedFS containers
	Cipher                      bool // whether encrypt data on volume server

}

var _ = fs.FS(&WFS{})
var _ = fs.FSStatfser(&WFS{})

type WFS struct {
	option                    *Option
	listDirectoryEntriesCache *ccache.Cache

	// contains all open handles, protected by handlesLock
	handlesLock       sync.Mutex
	handles           []*FileHandle
	pathToHandleIndex map[util.FullPath]int

	bufPool sync.Pool

	stats statsCache

	root        fs.Node
	fsNodeCache *FsCache

	chunkCache *pb_cache.ChunkCache
}
type statsCache struct {
	filer_pb.StatisticsResponse
	lastChecked int64 // unix time in seconds
}

func NewSeaweedFileSystem(option *Option) *WFS {
	wfs := &WFS{
		option:                    option,
		listDirectoryEntriesCache: ccache.New(ccache.Configure().MaxSize(option.DirListCacheLimit * 3).ItemsToPrune(100)),
		pathToHandleIndex:         make(map[util.FullPath]int),
		bufPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, option.ChunkSizeLimit)
			},
		},
		chunkCache: pb_cache.NewChunkCache(option.ChunkCacheCountLimit),
	}

	wfs.root = &Dir{name: wfs.option.FilerMountRootPath, wfs: wfs}
	wfs.fsNodeCache = newFsCache(wfs.root)

	return wfs
}

func (wfs *WFS) Root() (fs.Node, error) {
	return wfs.root, nil
}

func (wfs *WFS) WithFilerClient(fn func(filer_pb.SeaweedFilerClient) error) error {

	err := pb.WithCachedGrpcClient(func(grpcConnection *grpc.ClientConn) error {
		client := filer_pb.NewSeaweedFilerClient(grpcConnection)
		return fn(client)
	}, wfs.option.FilerGrpcAddress, wfs.option.GrpcDialOption)

	if err == nil {
		return nil
	}
	return err

}

func (wfs *WFS) AcquireHandle(file *File, uid, gid uint32) (fileHandle *FileHandle) {

	fullpath := file.fullpath()
	glog.V(4).Infof("%s AcquireHandle uid=%d gid=%d", fullpath, uid, gid)

	wfs.handlesLock.Lock()
	defer wfs.handlesLock.Unlock()

	index, found := wfs.pathToHandleIndex[fullpath]
	if found && wfs.handles[index] != nil {
		glog.V(2).Infoln(fullpath, "found fileHandle id", index)
		return wfs.handles[index]
	}

	fileHandle = newFileHandle(file, uid, gid)
	for i, h := range wfs.handles {
		if h == nil {
			wfs.handles[i] = fileHandle
			fileHandle.handle = uint64(i)
			wfs.pathToHandleIndex[fullpath] = i
			glog.V(4).Infof("%s reuse fh %d", fullpath, fileHandle.handle)
			return
		}
	}

	wfs.handles = append(wfs.handles, fileHandle)
	fileHandle.handle = uint64(len(wfs.handles) - 1)
	wfs.pathToHandleIndex[fullpath] = int(fileHandle.handle)
	glog.V(4).Infof("%s new fh %d", fullpath, fileHandle.handle)

	return
}

func (wfs *WFS) ReleaseHandle(fullpath util.FullPath, handleId fuse.HandleID) {
	wfs.handlesLock.Lock()
	defer wfs.handlesLock.Unlock()

	glog.V(4).Infof("%s ReleaseHandle id %d current handles length %d", fullpath, handleId, len(wfs.handles))
	delete(wfs.pathToHandleIndex, fullpath)
	if int(handleId) < len(wfs.handles) {
		wfs.handles[int(handleId)] = nil
	}

	return
}

// Statfs is called to obtain file system metadata. Implements fuse.FSStatfser
func (wfs *WFS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {

	glog.V(4).Infof("reading fs stats: %+v", req)

	if wfs.stats.lastChecked < time.Now().Unix()-20 {

		err := wfs.WithFilerClient(func(client filer_pb.SeaweedFilerClient) error {

			request := &filer_pb.StatisticsRequest{
				Collection:  wfs.option.Collection,
				Replication: wfs.option.Replication,
				Ttl:         fmt.Sprintf("%ds", wfs.option.TtlSec),
			}

			glog.V(4).Infof("reading filer stats: %+v", request)
			resp, err := client.Statistics(context.Background(), request)
			if err != nil {
				glog.V(0).Infof("reading filer stats %v: %v", request, err)
				return err
			}
			glog.V(4).Infof("read filer stats: %+v", resp)

			wfs.stats.TotalSize = resp.TotalSize
			wfs.stats.UsedSize = resp.UsedSize
			wfs.stats.FileCount = resp.FileCount
			wfs.stats.lastChecked = time.Now().Unix()

			return nil
		})
		if err != nil {
			glog.V(0).Infof("filer Statistics: %v", err)
			return err
		}
	}

	totalDiskSize := wfs.stats.TotalSize
	usedDiskSize := wfs.stats.UsedSize
	actualFileCount := wfs.stats.FileCount

	// Compute the total number of available blocks
	resp.Blocks = totalDiskSize / blockSize

	// Compute the number of used blocks
	numBlocks := uint64(usedDiskSize / blockSize)

	// Report the number of free and available blocks for the block size
	resp.Bfree = resp.Blocks - numBlocks
	resp.Bavail = resp.Blocks - numBlocks
	resp.Bsize = uint32(blockSize)

	// Report the total number of possible files in the file system (and those free)
	resp.Files = math.MaxInt64
	resp.Ffree = math.MaxInt64 - actualFileCount

	// Report the maximum length of a name and the minimum fragment size
	resp.Namelen = 1024
	resp.Frsize = uint32(blockSize)

	return nil
}

func (wfs *WFS) cacheGet(path util.FullPath) *filer_pb.Entry {
	item := wfs.listDirectoryEntriesCache.Get(string(path))
	if item != nil && !item.Expired() {
		return item.Value().(*filer_pb.Entry)
	}
	return nil
}
func (wfs *WFS) cacheSet(path util.FullPath, entry *filer_pb.Entry, ttl time.Duration) {
	if entry == nil {
		wfs.listDirectoryEntriesCache.Delete(string(path))
	} else {
		wfs.listDirectoryEntriesCache.Set(string(path), entry, ttl)
	}
}
func (wfs *WFS) cacheDelete(path util.FullPath) {
	wfs.listDirectoryEntriesCache.Delete(string(path))
}

func (wfs *WFS) AdjustedUrl(hostAndPort string) string {
	if !wfs.option.OutsideContainerClusterMode {
		return hostAndPort
	}
	commaIndex := strings.Index(hostAndPort, ":")
	if commaIndex < 0 {
		return hostAndPort
	}
	filerCommaIndex := strings.Index(wfs.option.FilerGrpcAddress, ":")
	return fmt.Sprintf("%s:%s", wfs.option.FilerGrpcAddress[:filerCommaIndex], hostAndPort[commaIndex+1:])

}
