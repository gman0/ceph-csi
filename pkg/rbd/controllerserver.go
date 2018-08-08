/*
Copyright 2018 The Kubernetes Authors.

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

package rbd

import (
	"fmt"
	"path"

	"github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/golang/glog"
	"github.com/kubernetes-csi/drivers/pkg/csi-common"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	oneGB = 1073741824
)

type controllerServer struct {
	*csicommon.DefaultControllerServer
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		msg := fmt.Sprintf("invalid create volume req: %v", req)
		glog.Error(msg)
		return nil, status.Error(codes.InvalidArgument, msg)
	}
	// Check sanity of request Name, Volume Capabilities
	if len(req.Name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume Name cannot be empty")
	}
	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities cannot be empty")
	}

	// Need to check for already existing volume name, and if found
	// check for the requested capacity and already allocated capacity
	if exVol, err := getRBDVolumeByName(req.GetName()); err == nil {
		// Since err is nil, it means the volume with the same name already exists
		// need to check if the size of exisiting volume is the same as in new
		// request
		if exVol.VolSize >= int64(req.GetCapacityRange().GetRequiredBytes()) {
			// exisiting volume is compatible with new request and should be reused.
			// TODO (sbezverk) Do I need to make sure that RBD volume still exists?
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					Id:            exVol.VolID,
					CapacityBytes: int64(exVol.VolSize),
					Attributes:    req.GetParameters(),
				},
			}, nil
		}
		return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Volume with the same name: %s but with different size already exist", req.GetName()))
	}

	// TODO (sbezverk) Last check for not exceeding total storage capacity

	rbdVol, err := getRBDVolumeOptions(req.GetParameters())
	if err != nil {
		glog.Errorf("failed to get RBD volume options: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Generating Volume Name and Volume ID, as accoeding to CSI spec they MUST be different
	volName := req.GetName()
	uniqueID := uuid.NewUUID().String()
	if len(volName) == 0 {
		volName = rbdVol.Pool + "-dynamic-pvc-" + uniqueID
	}
	rbdVol.VolName = volName
	volumeID := "csi-rbd-" + uniqueID
	rbdVol.VolID = volumeID
	// Volume Size - Default is 1 GiB
	volSizeBytes := int64(oneGB)
	if req.GetCapacityRange() != nil {
		volSizeBytes = int64(req.GetCapacityRange().GetRequiredBytes())
	}
	rbdVol.VolSize = volSizeBytes
	volSizeGB := int(volSizeBytes / 1024 / 1024 / 1024)

	// Check if there is already RBD image with requested name
	found, _, _ := rbdStatus(rbdVol, req.GetControllerCreateSecrets())
	if !found {
		if err := createRBDImage(rbdVol, volSizeGB, req.GetControllerCreateSecrets()); err != nil {
			if err != nil {
				glog.Errorf("failed to create volume: %v", err)
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
		glog.V(4).Infof("create volume %s", volName)
	}
	// Storing volInfo into a persistent file.
	if err := persistVolInfo(volumeID, path.Join(PluginFolder, "controller"), rbdVol); err != nil {
		glog.Warningf("rbd: failed to store volInfo with error: %v", err)
	}
	rbdVolumes[volumeID] = *rbdVol
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            volumeID,
			CapacityBytes: int64(volSizeBytes),
			Attributes:    req.GetParameters(),
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		msg := fmt.Sprintf("invalid delete volume req: %v", req)
		glog.Error(msg)
		return nil, status.Error(codes.InvalidArgument, msg)
	}
	// For now the image get unconditionally deleted, but here retention policy can be checked
	volumeID := req.GetVolumeId()
	rbdVol := &rbdVolume{}
	if err := loadVolInfo(volumeID, path.Join(PluginFolder, "controller"), rbdVol); err != nil {
		glog.Errorf("failed to load volume info for volume %s: %v", req.GetVolumeId(), err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	volName := rbdVol.VolName
	// Deleting rbd image
	glog.V(4).Infof("deleting volume %s", volName)
	if err := deleteRBDImage(rbdVol, req.GetControllerDeleteSecrets()); err != nil {
		glog.Errorf("failed to delete rbd image: %s/%s with error: %v", rbdVol.Pool, volName, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Removing persistent storage file for the unmapped volume
	if err := deleteVolInfo(volumeID, path.Join(PluginFolder, "controller")); err != nil {
		glog.Errorf("failed to delete volume info for volume %s: %v", req.GetVolumeId(), err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	delete(rbdVolumes, volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{Supported: false, Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Supported: true, Message: ""}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return &csi.ControllerPublishVolumeResponse{}, nil
}
