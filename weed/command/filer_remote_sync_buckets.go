package command

import (
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/pb/remote_pb"
	"github.com/chrislusf/seaweedfs/weed/remote_storage"
	"github.com/chrislusf/seaweedfs/weed/replication/source"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/golang/protobuf/proto"
	"math"
	"strings"
	"time"
)

func (option *RemoteSyncOptions) followBucketUpdatesAndUploadToRemote(filerSource *source.FilerSource) error {

	// read filer remote storage mount mappings
	if detectErr := option.collectRemoteStorageConf(); detectErr != nil {
		return fmt.Errorf("read mount info: %v", detectErr)
	}

	eachEntryFunc, err := option.makeBucketedEventProcessor(filerSource)
	if err != nil {
		return err
	}

	processEventFnWithOffset := pb.AddOffsetFunc(eachEntryFunc, 3*time.Second, func(counter int64, lastTsNs int64) error {
		lastTime := time.Unix(0, lastTsNs)
		glog.V(0).Infof("remote sync %s progressed to %v %0.2f/sec", *option.filerAddress, lastTime, float64(counter)/float64(3))
		return remote_storage.SetSyncOffset(option.grpcDialOption, *option.filerAddress, option.bucketsDir, lastTsNs)
	})

	lastOffsetTs := collectLastSyncOffset(option, option.bucketsDir)

	return pb.FollowMetadata(*option.filerAddress, option.grpcDialOption, "filer.remote.sync",
		option.bucketsDir, []string{filer.DirectoryEtcRemote}, lastOffsetTs.UnixNano(), 0, processEventFnWithOffset, false)
}

func (option *RemoteSyncOptions) makeBucketedEventProcessor(filerSource *source.FilerSource) (pb.ProcessMetadataFunc, error) {

	handleCreateBucket := func(entry *filer_pb.Entry) error {
		if !entry.IsDirectory {
			return nil
		}
		remoteConf, found := option.remoteConfs[*option.createBucketAt]
		if !found {
			return fmt.Errorf("un-configured remote storage %s", *option.createBucketAt)
		}

		client, err := remote_storage.GetRemoteStorage(remoteConf)
		if err != nil {
			return err
		}

		glog.V(0).Infof("create bucket %s", entry.Name)
		if err := client.CreateBucket(entry.Name); err != nil {
			return err
		}

		bucketPath := util.FullPath(option.bucketsDir).Child(entry.Name)
		remoteLocation := &remote_pb.RemoteStorageLocation{
			Name:   *option.createBucketAt,
			Bucket: entry.Name,
			Path:   "/",
		}

		return filer.InsertMountMapping(option, string(bucketPath), remoteLocation)

	}
	handleDeleteBucket := func(entry *filer_pb.Entry) error {
		if !entry.IsDirectory {
			return nil
		}

		client, err := option.findRemoteStorageClient(entry.Name)
		if err != nil {
			return err
		}

		glog.V(0).Infof("delete bucket %s", entry.Name)
		if err := client.DeleteBucket(entry.Name); err != nil {
			return err
		}

		bucketPath := util.FullPath(option.bucketsDir).Child(entry.Name)

		return filer.DeleteMountMapping(option, string(bucketPath))
	}

	handleEtcRemoteChanges := func(resp *filer_pb.SubscribeMetadataResponse) error {
		message := resp.EventNotification
		if message.NewEntry != nil {
			// update
			if message.NewEntry.Name == filer.REMOTE_STORAGE_MOUNT_FILE {
				newMappings, readErr := filer.UnmarshalRemoteStorageMappings(message.NewEntry.Content)
				if readErr != nil {
					return fmt.Errorf("unmarshal mappings: %v", readErr)
				}
				option.mappings = newMappings
			}
			if strings.HasSuffix(message.NewEntry.Name, filer.REMOTE_STORAGE_CONF_SUFFIX) {
				conf := &remote_pb.RemoteConf{}
				if err := proto.Unmarshal(message.NewEntry.Content, conf); err != nil {
					return fmt.Errorf("unmarshal %s/%s: %v", filer.DirectoryEtcRemote, message.NewEntry.Name, err)
				}
				option.remoteConfs[conf.Name] = conf
			}
		} else if message.OldEntry != nil {
			// deletion
			if strings.HasSuffix(message.OldEntry.Name, filer.REMOTE_STORAGE_CONF_SUFFIX) {
				conf := &remote_pb.RemoteConf{}
				if err := proto.Unmarshal(message.OldEntry.Content, conf); err != nil {
					return fmt.Errorf("unmarshal %s/%s: %v", filer.DirectoryEtcRemote, message.OldEntry.Name, err)
				}
				delete(option.remoteConfs, conf.Name)
			}
		}

		return nil
	}

	eachEntryFunc := func(resp *filer_pb.SubscribeMetadataResponse) error {
		message := resp.EventNotification
		if strings.HasPrefix(resp.Directory, filer.DirectoryEtcRemote) {
			return handleEtcRemoteChanges(resp)
		}

		if message.OldEntry == nil && message.NewEntry == nil {
			return nil
		}
		if message.OldEntry == nil && message.NewEntry != nil {
			if message.NewParentPath == option.bucketsDir {
				return handleCreateBucket(message.NewEntry)
			}
			if !filer.HasData(message.NewEntry) {
				return nil
			}
			bucket, remoteStorageMountLocation, remoteStorage, ok := option.detectBucketInfo(message.NewParentPath)
			if !ok {
				return nil
			}
			client, err := remote_storage.GetRemoteStorage(remoteStorage)
			if err != nil {
				return err
			}
			glog.V(2).Infof("create: %+v", resp)
			if !shouldSendToRemote(message.NewEntry) {
				glog.V(2).Infof("skipping creating: %+v", resp)
				return nil
			}
			dest := toRemoteStorageLocation(bucket, util.NewFullPath(message.NewParentPath, message.NewEntry.Name), remoteStorageMountLocation)
			if message.NewEntry.IsDirectory {
				glog.V(0).Infof("mkdir  %s", remote_storage.FormatLocation(dest))
				return client.WriteDirectory(dest, message.NewEntry)
			}
			glog.V(0).Infof("create %s", remote_storage.FormatLocation(dest))
			reader := filer.NewFileReader(filerSource, message.NewEntry)
			remoteEntry, writeErr := client.WriteFile(dest, message.NewEntry, reader)
			if writeErr != nil {
				return writeErr
			}
			return updateLocalEntry(&remoteSyncOptions, message.NewParentPath, message.NewEntry, remoteEntry)
		}
		if message.OldEntry != nil && message.NewEntry == nil {
			if resp.Directory == option.bucketsDir {
				return handleDeleteBucket(message.OldEntry)
			}
			bucket, remoteStorageMountLocation, remoteStorage, ok := option.detectBucketInfo(resp.Directory)
			if !ok {
				return nil
			}
			client, err := remote_storage.GetRemoteStorage(remoteStorage)
			if err != nil {
				return err
			}
			glog.V(2).Infof("delete: %+v", resp)
			dest := toRemoteStorageLocation(bucket, util.NewFullPath(resp.Directory, message.OldEntry.Name), remoteStorageMountLocation)
			if message.OldEntry.IsDirectory {
				glog.V(0).Infof("rmdir  %s", remote_storage.FormatLocation(dest))
				return client.RemoveDirectory(dest)
			}
			glog.V(0).Infof("delete %s", remote_storage.FormatLocation(dest))
			return client.DeleteFile(dest)
		}
		if message.OldEntry != nil && message.NewEntry != nil {
			if resp.Directory == option.bucketsDir {
				if message.NewParentPath == option.bucketsDir {
					if message.OldEntry.Name == message.NewEntry.Name {
						return nil
					}
					if err := handleCreateBucket(message.NewEntry); err != nil {
						return err
					}
					if err := handleDeleteBucket(message.OldEntry); err != nil {
						return err
					}
				}
			}
			oldBucket, oldRemoteStorageMountLocation, oldRemoteStorage, oldOk := option.detectBucketInfo(resp.Directory)
			newBucket, newRemoteStorageMountLocation, newRemoteStorage, newOk := option.detectBucketInfo(message.NewParentPath)
			if oldOk && newOk {
				if !shouldSendToRemote(message.NewEntry) {
					glog.V(2).Infof("skipping updating: %+v", resp)
					return nil
				}
				client, err := remote_storage.GetRemoteStorage(oldRemoteStorage)
				if err != nil {
					return err
				}
				if resp.Directory == message.NewParentPath && message.OldEntry.Name == message.NewEntry.Name {
					// update the same entry
					if message.NewEntry.IsDirectory {
						// update directory property
						return nil
					}
					if filer.IsSameData(message.OldEntry, message.NewEntry) {
						glog.V(2).Infof("update meta: %+v", resp)
						oldDest := toRemoteStorageLocation(oldBucket, util.NewFullPath(resp.Directory, message.OldEntry.Name), oldRemoteStorageMountLocation)
						return client.UpdateFileMetadata(oldDest, message.OldEntry, message.NewEntry)
					} else {
						newDest := toRemoteStorageLocation(newBucket, util.NewFullPath(message.NewParentPath, message.NewEntry.Name), newRemoteStorageMountLocation)
						reader := filer.NewFileReader(filerSource, message.NewEntry)
						glog.V(0).Infof("create %s", remote_storage.FormatLocation(newDest))
						remoteEntry, writeErr := client.WriteFile(newDest, message.NewEntry, reader)
						if writeErr != nil {
							return writeErr
						}
						return updateLocalEntry(&remoteSyncOptions, message.NewParentPath, message.NewEntry, remoteEntry)
					}
				}
			}

			// the following is entry rename
			if oldOk {
				client, err := remote_storage.GetRemoteStorage(oldRemoteStorage)
				if err != nil {
					return err
				}
				oldDest := toRemoteStorageLocation(oldBucket, util.NewFullPath(resp.Directory, message.OldEntry.Name), oldRemoteStorageMountLocation)
				if message.OldEntry.IsDirectory {
					return client.RemoveDirectory(oldDest)
				}
				glog.V(0).Infof("delete %s", remote_storage.FormatLocation(oldDest))
				if err := client.DeleteFile(oldDest); err != nil {
					return err
				}
			}
			if newOk {
				if !shouldSendToRemote(message.NewEntry) {
					glog.V(2).Infof("skipping updating: %+v", resp)
					return nil
				}
				client, err := remote_storage.GetRemoteStorage(newRemoteStorage)
				if err != nil {
					return err
				}
				newDest := toRemoteStorageLocation(newBucket, util.NewFullPath(message.NewParentPath, message.NewEntry.Name), newRemoteStorageMountLocation)
				if message.NewEntry.IsDirectory {
					return client.WriteDirectory(newDest, message.NewEntry)
				}
				reader := filer.NewFileReader(filerSource, message.NewEntry)
				glog.V(0).Infof("create %s", remote_storage.FormatLocation(newDest))
				remoteEntry, writeErr := client.WriteFile(newDest, message.NewEntry, reader)
				if writeErr != nil {
					return writeErr
				}
				return updateLocalEntry(&remoteSyncOptions, message.NewParentPath, message.NewEntry, remoteEntry)
			}
		}

		return nil
	}
	return eachEntryFunc, nil
}

func (option *RemoteSyncOptions)findRemoteStorageClient(bucketName string) (remote_storage.RemoteStorageClient, error) {
	bucket := util.FullPath(option.bucketsDir).Child(bucketName)

	remoteStorageMountLocation, isMounted := option.mappings.Mappings[string(bucket)]
	if !isMounted {
		return nil, fmt.Errorf("%s is not mounted", bucket)
	}
	remoteConf, hasClient := option.remoteConfs[remoteStorageMountLocation.Name]
	if !hasClient {
		return nil, fmt.Errorf("%s mounted to un-configured %+v", bucket, remoteStorageMountLocation)
	}

	client, err := remote_storage.GetRemoteStorage(remoteConf)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (option *RemoteSyncOptions) detectBucketInfo(actualDir string) (bucket util.FullPath, remoteStorageMountLocation *remote_pb.RemoteStorageLocation, remoteConf *remote_pb.RemoteConf, ok bool) {
	bucket, ok = extractBucketPath(option.bucketsDir, actualDir)
	if !ok {
		return "", nil, nil, false
	}
	var isMounted bool
	remoteStorageMountLocation, isMounted = option.mappings.Mappings[string(bucket)]
	if !isMounted {
		glog.Warningf("%s is not mounted", bucket)
		return "", nil, nil, false
	}
	var hasClient bool
	remoteConf, hasClient = option.remoteConfs[remoteStorageMountLocation.Name]
	if !hasClient {
		glog.Warningf("%s mounted to un-configured %+v", bucket, remoteStorageMountLocation)
		return "", nil, nil, false
	}
	return bucket, remoteStorageMountLocation, remoteConf, true
}

func extractBucketPath(bucketsDir, dir string) (util.FullPath, bool) {
	if !strings.HasPrefix(dir, bucketsDir+"/") {
		return "", false
	}
	parts := strings.SplitN(dir[len(bucketsDir)+1:], "/", 2)
	return util.FullPath(bucketsDir).Child(parts[0]), true
}

func (option *RemoteSyncOptions) collectRemoteStorageConf() (err error) {

	if mappings, err := filer.ReadMountMappings(option.grpcDialOption, *option.filerAddress); err != nil {
		return err
	} else {
		option.mappings = mappings
	}

	option.remoteConfs = make(map[string]*remote_pb.RemoteConf)
	err = filer_pb.List(option, filer.DirectoryEtcRemote, "", func(entry *filer_pb.Entry, isLast bool) error {
		if !strings.HasSuffix(entry.Name, filer.REMOTE_STORAGE_CONF_SUFFIX) {
			return nil
		}
		conf := &remote_pb.RemoteConf{}
		if err := proto.Unmarshal(entry.Content, conf); err != nil {
			return fmt.Errorf("unmarshal %s/%s: %v", filer.DirectoryEtcRemote, entry.Name, err)
		}
		option.remoteConfs[conf.Name] = conf
		return nil
	}, "", false, math.MaxUint32)

	return
}