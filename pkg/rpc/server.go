package rpc

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/fscrypt/filesystem"
	"github.com/longhorn/types/pkg/generated/smrpc"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	grpccodes "google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/mount-utils"

	lhexec "github.com/longhorn/go-common-libs/exec"
	lhtypes "github.com/longhorn/go-common-libs/types"

	"github.com/longhorn/longhorn-share-manager/pkg/crypto"
	"github.com/longhorn/longhorn-share-manager/pkg/server"
	"github.com/longhorn/longhorn-share-manager/pkg/server/nfs"
	"github.com/longhorn/longhorn-share-manager/pkg/types"
	"github.com/longhorn/longhorn-share-manager/pkg/util"
	"github.com/longhorn/longhorn-share-manager/pkg/volume"
)

const (
	configPath = "/tmp/vfs.conf"

	unmountRetryCount    = 30
	unmountRetryInterval = 1
)

type ShareManagerServer struct {
	smrpc.UnimplementedShareManagerServiceServer
	sync.RWMutex

	logger  logrus.FieldLogger
	manager *server.ShareManager
}

func NewShareManagerServer(manager *server.ShareManager) *ShareManagerServer {
	return &ShareManagerServer{
		logger:  util.NewLogger(),
		manager: manager,
	}
}

func (s *ShareManagerServer) FilesystemTrim(ctx context.Context, req *smrpc.FilesystemTrimRequest) (resp *emptypb.Empty, err error) {
	s.Lock()
	defer s.Unlock()

	vol := s.manager.GetVolume()
	if vol.Name == "" {
		s.logger.Warn("Volume name is missing")
		return &emptypb.Empty{}, nil
	}

	log := s.logger.WithField("volume", vol.Name)

	defer func() {
		if err != nil {
			log.WithError(err).Errorf("Failed to trim mounted filesystem on volume")
		}
	}()

	devicePath := types.GetVolumeDevicePath(vol.Name, vol.DataEngine, req.EncryptedDevice)
	if !volume.CheckDeviceValid(devicePath) {
		return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.FailedPrecondition, "volume %v is not valid", vol.Name)
	}

	mountPath := types.GetMountPath(vol.Name)

	mnt, err := filesystem.GetMount(mountPath)
	if err != nil {
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	deviceNumber, err := util.GetDeviceNumber(devicePath)
	if err != nil {
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	if uint64(mnt.DeviceNumber) != uint64(deviceNumber) {
		return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.InvalidArgument, "the device of mount point %v is not expected", mountPath)
	}

	log.Infof("Trimming mounted filesystem %v", mountPath)

	mounter := mount.New("")
	isMountPoint, err := mounter.IsMountPoint(mountPath)
	if !isMountPoint {
		return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.InvalidArgument, "%v is not a mount point", mountPath)
	}
	if err != nil {
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	if _, err := os.ReadDir(mountPath); err != nil {
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	execute := lhexec.NewExecutor().Execute
	_, err = execute([]string{}, lhtypes.BinaryFstrim, []string{mountPath}, lhtypes.ExecuteDefaultTimeout)
	if err != nil {
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	log.Infof("Finished trimming mounted filesystem %v", mountPath)

	return &emptypb.Empty{}, nil
}

func (s *ShareManagerServer) FilesystemResize(ctx context.Context, req *emptypb.Empty) (resp *emptypb.Empty, err error) {
	s.Lock()
	defer s.Unlock()

	vol := s.manager.GetVolume()
	if vol.Name == "" {
		s.logger.Warn("Volume name is missing")
		return &emptypb.Empty{}, nil
	}

	log := s.logger.WithField("volume", vol.Name)

	defer func() {
		if err != nil {
			log.WithError(err).Errorf("Failed to resize mounted filesystem on volume")
		}
	}()

	devicePath := types.GetVolumeDevicePath(vol.Name, vol.DataEngine, vol.IsEncrypted())
	if !volume.CheckDeviceValid(devicePath) {
		return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.FailedPrecondition, "volume %v is not valid", vol.Name)
	}

	mountPath := types.GetMountPath(vol.Name)
	log = log.WithField("filesystem", mountPath)

	_, err = filesystem.GetMount(mountPath)
	if err != nil {
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	log.Infof("Resizing mounted volume")

	// Note that cryptsetup resize is only necessary for volumes resized while online.  For offline, it will happen automatically during 'open'.
	if vol.IsEncrypted() {
		rawDevicePath := types.GetRawVolumeDevicePath(vol.Name)
		diskFormat, err := volume.GetDiskFormat(rawDevicePath)
		if err != nil {
			return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.Internal, "failed to determine disk format of volume %v: %v", vol.Name, err)
		}
		log.WithField("mappedDevice", devicePath).Infof("Encrypted volume device %v contains filesystem of format %v", rawDevicePath, diskFormat)

		if diskFormat != "crypto_LUKS" {
			return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.InvalidArgument, "unsupported disk encryption format %v", diskFormat)
		}

		if err = crypto.ResizeEncryptoDevice(vol.Name, vol.DataEngine, vol.Passphrase); err != nil {
			return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.Internal, "failed to resize crypto device %v for volume %v node expansion: %v", devicePath, vol.Name, err)
		}
	}

	if resized, err := volume.ResizeVolume(devicePath, mountPath); err != nil {
		log.WithError(err).Errorf("Failed to resize filesystem")
		return &emptypb.Empty{}, grpcstatus.Error(grpccodes.Internal, err.Error())
	} else if resized {
		log.Infof("Resized filesystem")
	} else {
		log.Infof("No resize needed for filesystem")
	}

	return &emptypb.Empty{}, nil
}

func (s *ShareManagerServer) unexport(vol volume.Volume) error {
	exporter, err := nfs.NewExporter(configPath, types.ExportPath)
	if err != nil {
		return errors.Wrap(err, "failed to create nfs exporter")
	}

	if err := exporter.DeleteExport(vol.Name); err != nil {
		return errors.Wrap(err, "failed to delete nfs export")
	}

	if err := exporter.ReloadExport(); err != nil {
		return errors.Wrap(err, "failed to reload nfs export")
	}

	return nil
}

func (s *ShareManagerServer) unmount(vol volume.Volume) error {
	mountPath := types.GetMountPath(vol.Name)

	mounter := mount.New("")
	isMountPoint, err := mounter.IsMountPoint(mountPath)
	if err != nil {
		return errors.Wrapf(err, "failed to check mount point %v", mountPath)
	}
	if !isMountPoint {
		return nil
	}

	return volume.UnmountVolume(mountPath)
}

func (s *ShareManagerServer) Unmount(ctx context.Context, req *emptypb.Empty) (resp *emptypb.Empty, err error) {
	s.Lock()
	defer s.Unlock()

	vol := s.manager.GetVolume()
	if vol.Name == "" {
		s.logger.Warn("Volume name is missing")
		return &emptypb.Empty{}, nil
	}

	log := s.logger.WithField("volume", vol.Name)

	if !nfsServerIsRunning() {
		log.Info("NFS server is not running, skip unexporting and unmounting volume")
		return &emptypb.Empty{}, nil
	}

	// Blindly mark the volume as unexported, even if the unmount fails.
	// Mount() will re-export the volume and mark it as exported if needed.
	s.manager.SetShareExported(false)

	defer func() {
		if err != nil {
			log.WithError(err).Errorf("Failed to unexport and unmount volume")
		}
	}()

	log.Info("Unexporting volume")
	err = s.unexport(vol)
	if err != nil {
		return nil, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	log.Info("Unmounting volume")
	for i := 0; i < unmountRetryCount; i++ {
		err = s.unmount(vol)
		if err != nil && strings.Contains(err.Error(), "target is busy") {
			time.Sleep(unmountRetryInterval)
			continue
		}
		break
	}
	if err != nil {
		return nil, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	log.Info("Volume is unexported and unmounted")

	return &emptypb.Empty{}, nil
}

func (s *ShareManagerServer) mount(vol volume.Volume, devicePath, mountPath string) error {
	if err := s.manager.MountVolume(s.manager.GetVolume(), devicePath, mountPath); err != nil {
		return errors.Wrapf(err, "failed to mount volume %v", vol.Name)
	}

	return nil
}

func (s *ShareManagerServer) export(vol volume.Volume) error {
	exporter, err := nfs.NewExporter(configPath, types.ExportPath)
	if err != nil {
		return errors.Wrap(err, "failed to create nfs exporter")
	}

	if _, err := exporter.CreateExport(vol.Name); err != nil {
		return errors.Wrap(err, "failed to delete nfs export")
	}

	if err := exporter.ReloadExport(); err != nil {
		return errors.Wrap(err, "failed to reload nfs export")
	}

	return nil
}

func (s *ShareManagerServer) Mount(ctx context.Context, req *emptypb.Empty) (resp *emptypb.Empty, err error) {
	s.Lock()
	defer s.Unlock()

	vol := s.manager.GetVolume()
	if vol.Name == "" {
		s.logger.Warn("Volume name is missing")
		return &emptypb.Empty{}, nil
	}

	log := s.logger.WithField("volume", vol.Name)

	if !nfsServerIsRunning() {
		log.Info("NFS server is not running, skip mounting and exporting volume")
		return &emptypb.Empty{}, nil
	}

	if s.manager.ShareIsExported() {
		return &emptypb.Empty{}, nil
	}

	log.Info("Mounting and exporting volume")

	devicePath := types.GetVolumeDevicePath(vol.Name, vol.DataEngine, false)
	mountPath := types.GetMountPath(vol.Name)

	defer func() {
		if err != nil {
			log.WithError(err).Errorf("Failed to mount and export volume")
		}
	}()

	mounter := mount.New("")
	isMountPoint, err := mounter.IsMountPoint(mountPath)
	if err != nil {
		err = errors.Wrapf(err, "failed to check mount point %v", mountPath)
		return &emptypb.Empty{}, grpcstatus.Errorf(grpccodes.Internal, "%v", err)
	}
	if !isMountPoint {
		log.Info("Mounting volume")
		err = s.mount(vol, devicePath, mountPath)
		if err != nil {
			return nil, grpcstatus.Error(grpccodes.Internal, err.Error())
		}
	}

	log.Info("Exporting volume")
	err = s.export(vol)
	if err != nil {
		return nil, grpcstatus.Error(grpccodes.Internal, err.Error())
	}

	log.Info("Volume is mounted and exported")
	s.manager.SetShareExported(true)

	return &emptypb.Empty{}, nil
}

type ShareManagerHealthCheckServer struct {
	srv *ShareManagerServer
}

func NewShareManagerHealthCheckServer(srv *ShareManagerServer) *ShareManagerHealthCheckServer {
	return &ShareManagerHealthCheckServer{
		srv: srv,
	}
}

func (s *ShareManagerHealthCheckServer) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if s.srv != nil {
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_SERVING,
		}, nil
	}

	return &healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_NOT_SERVING,
	}, fmt.Errorf("share manager gRPC server is not running")
}

func (s *ShareManagerHealthCheckServer) Watch(req *healthpb.HealthCheckRequest, ws healthpb.Health_WatchServer) error {
	for {
		if s.srv != nil {
			if err := ws.Send(&healthpb.HealthCheckResponse{
				Status: healthpb.HealthCheckResponse_SERVING,
			}); err != nil {
				logrus.WithError(err).Errorf("Failed to send health check result %v for share manager gRPC server",
					healthpb.HealthCheckResponse_SERVING)
			}
		} else {
			if err := ws.Send(&healthpb.HealthCheckResponse{
				Status: healthpb.HealthCheckResponse_NOT_SERVING,
			}); err != nil {
				logrus.WithError(err).Errorf("Failed to send health check result %v for share manager gRPC server",
					healthpb.HealthCheckResponse_NOT_SERVING)
			}

		}
		time.Sleep(time.Second)
	}
}

func (s *ShareManagerHealthCheckServer) List(context.Context, *healthpb.HealthListRequest) (*healthpb.HealthListResponse, error) {
	return &healthpb.HealthListResponse{
		Statuses: map[string]*healthpb.HealthCheckResponse{
			"grpc": {
				Status: healthpb.HealthCheckResponse_SERVING,
			},
		},
	}, nil
}

func nfsServerIsRunning() bool {
	_, err := util.FindProcessByName("ganesha.nfsd")
	return err == nil
}
