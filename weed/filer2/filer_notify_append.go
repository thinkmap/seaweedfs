package filer2

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
)

func (f *Filer) appendToFile(targetFile string, data []byte) error {

	// assign a volume location
	assignRequest := &operation.VolumeAssignRequest{
		Count: 1,
	}
	assignResult, err := operation.Assign(f.GetMaster(), f.GrpcDialOption, assignRequest)
	if err != nil {
		return fmt.Errorf("AssignVolume: %v", err)
	}
	if assignResult.Error != "" {
		return fmt.Errorf("AssignVolume error: %v", assignResult.Error)
	}

	// upload data
	targetUrl := "http://" + assignResult.Url + "/" + assignResult.Fid
	uploadResult, err := operation.UploadData(targetUrl, "", false, data, false, "", nil, assignResult.Auth)
	if err != nil {
		return fmt.Errorf("upload data %s: %v", targetUrl, err)
	}
	// println("uploaded to", targetUrl)

	// find out existing entry
	fullpath := util.FullPath(targetFile)
	entry, err := f.FindEntry(context.Background(), fullpath)
	var offset int64 = 0
	if err == filer_pb.ErrNotFound {
		entry = &Entry{
			FullPath: fullpath,
			Attr: Attr{
				Crtime: time.Now(),
				Mtime:  time.Now(),
				Mode:   os.FileMode(0644),
				Uid:    OS_UID,
				Gid:    OS_GID,
			},
		}
	} else {
		offset = int64(TotalSize(entry.Chunks))
	}

	// append to existing chunks
	chunk := &filer_pb.FileChunk{
		FileId:    assignResult.Fid,
		Offset:    offset,
		Size:      uint64(uploadResult.Size),
		Mtime:     time.Now().UnixNano(),
		ETag:      uploadResult.ETag,
		IsGzipped: uploadResult.Gzip > 0,
	}
	entry.Chunks = append(entry.Chunks, chunk)

	// update the entry
	err = f.CreateEntry(context.Background(), entry, false)

	return err
}
