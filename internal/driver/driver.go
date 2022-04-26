// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2022 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/common"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sdkModel "github.com/edgexfoundry/device-sdk-go/v2/pkg/models"
	sdk "github.com/edgexfoundry/device-sdk-go/v2/pkg/service"
	"github.com/edgexfoundry/go-mod-bootstrap/v2/bootstrap/secret"
	"github.com/edgexfoundry/go-mod-bootstrap/v2/bootstrap/startup"
	"github.com/edgexfoundry/go-mod-bootstrap/v2/config"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/clients/logger"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/errors"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/models"

	"github.com/IOTechSystems/onvif"
	"github.com/IOTechSystems/onvif/device"
	wsdiscovery "github.com/IOTechSystems/onvif/ws-discovery"
)

const (
	URLRawQuery = "urlRawQuery"
	jsonObject  = "jsonObject"

	cameraAdded   = "CameraAdded"
	cameraUpdated = "CameraUpdated"
	cameraDeleted = "CameraDeleted"
)

// Driver implements the sdkModel.ProtocolDriver interface for
// the device service
type Driver struct {
	lc           logger.LoggingClient
	asynchCh     chan<- *sdkModel.AsyncValues
	deviceCh     chan<- []sdkModel.DiscoveredDevice
	config       *configuration
	lock         *sync.RWMutex
	onvifClients map[string]*OnvifClient
	serviceName  string
}

// Initialize performs protocol-specific initialization for the device
// service.
func (d *Driver) Initialize(lc logger.LoggingClient, asyncCh chan<- *sdkModel.AsyncValues,
	deviceCh chan<- []sdkModel.DiscoveredDevice) error {
	d.lc = lc
	d.asynchCh = asyncCh
	d.deviceCh = deviceCh
	d.lock = new(sync.RWMutex)
	d.onvifClients = make(map[string]*OnvifClient)
	d.serviceName = sdk.RunningService().Name()

	camConfig, err := loadCameraConfig(sdk.DriverConfigs())
	if err != nil {
		return errors.NewCommonEdgeX(errors.KindServerError, "failed to load camera configuration", err)
	}
	d.config = camConfig

	deviceService := sdk.RunningService()

	for _, device := range deviceService.Devices() {
		if device.Name == d.serviceName {
			continue
		}

		d.lc.Infof("Initializing onvif client for '%s' camera", device.Name)

		onvifClient, err := d.newOnvifClient(device)
		if err != nil {
			d.lc.Errorf("failed to initial onvif client for '%s' camera, skipping this device.", device.Name)
			continue
		}
		d.lock.Lock()
		d.onvifClients[device.Name] = onvifClient
		d.lock.Unlock()
	}

	handler := NewRestNotificationHandler(deviceService, lc, asyncCh)
	edgexErr := handler.AddRoute()
	if edgexErr != nil {
		return errors.NewCommonEdgeXWrapper(edgexErr)
	}

	d.lc.Info("Driver initialized.")
	return nil
}

func (d *Driver) getOnvifClient(deviceName string) (*OnvifClient, errors.EdgeX) {
	d.lock.RLock()
	defer d.lock.RUnlock()
	onvifClient, ok := d.onvifClients[deviceName]
	if !ok {
		device, err := sdk.RunningService().GetDeviceByName(deviceName)
		if err != nil {
			return nil, errors.NewCommonEdgeXWrapper(err)
		}
		onvifClient, err = d.newOnvifClient(device)
		if err != nil {
			return nil, errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to initial onvif client for '%s' camera", device.Name), err)
		}
		d.onvifClients[deviceName] = onvifClient
	}
	return onvifClient, nil
}

func (d *Driver) removeOnvifClient(deviceName string) {
	d.lock.Lock()
	defer d.lock.Unlock()
	_, ok := d.onvifClients[deviceName]
	if ok {
		delete(d.onvifClients, deviceName)
	}
}

// HandleReadCommands triggers a protocol Read operation for the specified device.
func (d *Driver) HandleReadCommands(deviceName string, protocols map[string]models.ProtocolProperties, reqs []sdkModel.CommandRequest) ([]*sdkModel.CommandValue, error) {
	var edgexErr errors.EdgeX
	var responses = make([]*sdkModel.CommandValue, len(reqs))

	onvifClient, edgexErr := d.getOnvifClient(deviceName)
	if edgexErr != nil {
		return responses, errors.NewCommonEdgeXWrapper(edgexErr)
	}

	for i, req := range reqs {
		data, edgexErr := parametersFromURLRawQuery(req)
		if edgexErr != nil {
			return responses, errors.NewCommonEdgeXWrapper(edgexErr)
		}

		cv, edgexErr := onvifClient.CallOnvifFunction(req, GetFunction, data)
		if edgexErr != nil {
			return responses, errors.NewCommonEdgeX(errors.KindServerError, "failed to execute read command", edgexErr)
		}
		responses[i] = cv
	}

	return responses, nil
}

func attributeByKey(attributes map[string]interface{}, key string) (attr string, err errors.EdgeX) {
	val, ok := attributes[key]
	if !ok {
		return "", errors.NewCommonEdgeX(errors.KindContractInvalid, fmt.Sprintf("attribute %s not exists", key), nil)
	}
	attr = fmt.Sprint(val)
	return attr, nil
}

func parametersFromURLRawQuery(req sdkModel.CommandRequest) ([]byte, errors.EdgeX) {
	values, err := url.ParseQuery(fmt.Sprint(req.Attributes[URLRawQuery]))
	if err != nil {
		return nil, errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to parse get command parameter for resource '%s'", req.DeviceResourceName), err)
	}
	param, exists := values[jsonObject]
	if !exists || len(param) == 0 {
		return []byte{}, nil
	}
	data, err := base64.StdEncoding.DecodeString(param[0])
	if err != nil {
		return nil, errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to decode '%v' parameter for resource '%s', the value should be json object with base64 encoded", jsonObject, req.DeviceResourceName), err)
	}
	return data, nil
}

// HandleWriteCommands passes a slice of CommandRequest struct each representing
// a ResourceOperation for a specific device resource (aka DeviceObject).
// Since the commands are actuation commands, params provide parameters for the individual
// command.
func (d *Driver) HandleWriteCommands(deviceName string, protocols map[string]models.ProtocolProperties, reqs []sdkModel.CommandRequest, params []*sdkModel.CommandValue) error {
	var edgexErr errors.EdgeX

	onvifClient, edgexErr := d.getOnvifClient(deviceName)
	if edgexErr != nil {
		return errors.NewCommonEdgeXWrapper(edgexErr)
	}

	for i, req := range reqs {
		parameters, err := params[i].ObjectValue()
		if err != nil {
			return errors.NewCommonEdgeXWrapper(err)
		}
		data, err := json.Marshal(parameters)
		if err != nil {
			return errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to marshal set command parameter for resource '%s'", req.DeviceResourceName), err)
		}

		result, err := onvifClient.CallOnvifFunction(req, SetFunction, data)
		if err != nil {
			return errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to execute write command, %s", result), err)
		}
	}

	return nil
}

// DisconnectDevice handles protocol-specific cleanup when a device
// is removed.
func (d *Driver) DisconnectDevice(deviceName string, protocols map[string]models.ProtocolProperties) error {
	d.lc.Warn("Driver's DisconnectDevice function not implemented")
	return nil
}

// Stop the protocol-specific DS code to shutdown gracefully, or
// if the force parameter is 'true', immediately. The driver is responsible
// for closing any in-use channels, including the channel used to send async
// readings (if supported).
func (d *Driver) Stop(force bool) error {
	close(d.asynchCh)
	for _, client := range d.onvifClients {
		client.pullPointManager.UnsubscribeAll()
		client.baseNotificationManager.UnsubscribeAll()
	}

	return nil
}

func (d *Driver) publishControlPlaneEvent(deviceName, eventType string) {
	var cv *sdkModel.CommandValue
	var err error

	if eventType == cameraDeleted {
		// camera deleted event just sends the device name
		cv, err = sdkModel.NewCommandValue(eventType, common.ValueTypeString, deviceName)
		if err != nil {
			d.lc.Errorf("issue creating control plane-event %s for device %s: %v", eventType, deviceName, err)
			return
		}
	} else {
		// added and updated events send the whole device information
		dev, err := sdk.RunningService().GetDeviceByName(deviceName)
		if err != nil {
			d.lc.Errorf("issue getting device %s: %v", eventType, deviceName, err)
			return
		}

		cv, err = sdkModel.NewCommandValue(eventType, common.ValueTypeObject, dev)
		if err != nil {
			d.lc.Errorf("issue creating control-plane event %s for device %s: %v", eventType, deviceName, err)
			return
		}
	}

	asyncValues := &sdkModel.AsyncValues{
		DeviceName:    d.serviceName,
		CommandValues: []*sdkModel.CommandValue{cv},
	}
	d.asynchCh <- asyncValues
}

// AddDevice is a callback function that is invoked
// when a new Device associated with this Device Service is added
func (d *Driver) AddDevice(deviceName string, protocols map[string]models.ProtocolProperties, adminState models.AdminState) error {
	// only execute if this was not called for the control-plane device
	if deviceName != d.serviceName {
		d.publishControlPlaneEvent(deviceName, cameraAdded)
		err := d.createOnvifClient(deviceName)
		if err != nil {
			return errors.NewCommonEdgeXWrapper(err)
		}
	}
	return nil
}

// UpdateDevice is a callback function that is invoked
// when a Device associated with this Device Service is updated
func (d *Driver) UpdateDevice(deviceName string, protocols map[string]models.ProtocolProperties, adminState models.AdminState) error {
	// only execute if this was not called for the control-plane device
	if deviceName != d.serviceName {
		d.publishControlPlaneEvent(deviceName, cameraUpdated)
		// Invoke the createOnvifClient func to create new onvif client and replace the old one
		err := d.createOnvifClient(deviceName)
		if err != nil {
			return errors.NewCommonEdgeXWrapper(err)
		}
	}
	return nil
}

// RemoveDevice is a callback function that is invoked
// when a Device associated with this Device Service is removed
func (d *Driver) RemoveDevice(deviceName string, protocols map[string]models.ProtocolProperties) error {
	// only execute if this was not called for the control-plane device
	if deviceName != d.serviceName {
		d.publishControlPlaneEvent(deviceName, cameraDeleted)
		d.removeOnvifClient(deviceName)
	}
	return nil
}

// createOnvifClient create the Onvif client for specified the device
func (d *Driver) createOnvifClient(deviceName string) error {
	device, err := sdk.RunningService().GetDeviceByName(deviceName)
	if err != nil {
		return errors.NewCommonEdgeXWrapper(err)
	}
	onvifClient, err := d.newOnvifClient(device)
	if err != nil {
		return errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to initial onvif client for '%s' camera", device.Name), err)
	}

	d.lock.Lock()
	defer d.lock.Unlock()
	d.onvifClients[deviceName] = onvifClient
	return nil
}

func (d *Driver) getCredentials(secretPath string) (config.Credentials, errors.EdgeX) {
	credentials := config.Credentials{}
	deviceService := sdk.RunningService()

	timer := startup.NewTimer(d.config.CredentialsRetryTime, d.config.CredentialsRetryWait)

	var secretData map[string]string
	var err error
	for timer.HasNotElapsed() {
		secretData, err = deviceService.SecretProvider.GetSecret(secretPath, secret.UsernameKey, secret.PasswordKey)
		if err == nil {
			break
		}

		d.lc.Warnf(
			"Unable to retrieve camera credentials from SecretProvider at path '%s': %s. Retrying for %s",
			secretPath,
			err.Error(),
			timer.RemainingAsString())
		timer.SleepForInterval()
	}

	if err != nil {
		return credentials, errors.NewCommonEdgeXWrapper(err)
	}

	credentials.Username = secretData[secret.UsernameKey]
	credentials.Password = secretData[secret.PasswordKey]

	return credentials, nil
}

// Discover triggers protocol specific device discovery, which is an asynchronous operation.
// Devices found as part of this discovery operation are written to the channel devices.
func (d *Driver) Discover() {
	onvifDevices := wsdiscovery.GetAvailableDevicesAtSpecificEthernetInterface(d.config.DiscoveryEthernetInterface)
	var discoveredDevices []sdkModel.DiscoveredDevice
	for _, onvifDevice := range onvifDevices {
		if onvifDevice.GetDeviceParams().EndpointRefAddress == "" {
			d.lc.Warnf("The EndpointRefAddress is empty from the Onvif camera, unable to add the camera %s", onvifDevice.GetDeviceParams().Xaddr)
			continue
		}
		address, port := addressAndPort(onvifDevice.GetDeviceParams().Xaddr)
		dev := models.Device{
			// Using Xaddr as the temporary name
			Name: onvifDevice.GetDeviceParams().Xaddr,
			Protocols: map[string]models.ProtocolProperties{
				OnvifProtocol: {
					Address:    address,
					Port:       port,
					AuthMode:   d.config.DefaultAuthMode,
					SecretPath: d.config.DefaultSecretPath,
				},
			},
		}

		devInfo, edgexErr := d.getDeviceInformation(dev)
		endpointRef := onvifDevice.GetDeviceParams().EndpointRefAddress
		var discovered sdkModel.DiscoveredDevice
		if edgexErr != nil {
			d.lc.Warnf("failed to get the device information for the camera %s, %v", endpointRef, edgexErr)
			dev.Protocols[OnvifProtocol][SecretPath] = endpointRef
			discovered = sdkModel.DiscoveredDevice{
				Name:        endpointRef,
				Protocols:   dev.Protocols,
				Description: "Auto discovered Onvif camera",
				Labels:      []string{"auto-discovery"},
			}
			d.lc.Debugf("Discovered unknown camera from the address '%s'", onvifDevice.GetDeviceParams().Xaddr)
		} else {
			dev.Protocols[OnvifProtocol][Manufacturer] = devInfo.Manufacturer
			dev.Protocols[OnvifProtocol][Model] = devInfo.Model
			dev.Protocols[OnvifProtocol][FirmwareVersion] = devInfo.FirmwareVersion
			dev.Protocols[OnvifProtocol][SerialNumber] = devInfo.SerialNumber
			dev.Protocols[OnvifProtocol][HardwareId] = devInfo.HardwareId

			// Spaces are not allowed in the device name
			deviceName := fmt.Sprintf("%s-%s-%s",
				strings.ReplaceAll(devInfo.Manufacturer, " ", "-"),
				strings.ReplaceAll(devInfo.Model, " ", "-"),
				onvifDevice.GetDeviceParams().EndpointRefAddress)

			discovered = sdkModel.DiscoveredDevice{
				Name:        deviceName,
				Protocols:   dev.Protocols,
				Description: fmt.Sprintf("%s %s Camera", devInfo.Manufacturer, devInfo.Model),
				Labels:      []string{"auto-discovery", devInfo.Manufacturer, devInfo.Model},
			}
			d.lc.Debugf("Discovered camera from the address '%s'", onvifDevice.GetDeviceParams().Xaddr)
		}
		discoveredDevices = append(discoveredDevices, discovered)
	}

	d.deviceCh <- discoveredDevices
}

func addressAndPort(xaddr string) (string, string) {
	substrings := strings.Split(xaddr, ":")
	if len(substrings) == 1 {
		// The port the might be empty from the discovered result, for example <d:XAddrs>http://192.168.12.123/onvif/device_service</d:XAddrs>
		return substrings[0], "80"
	} else {
		return substrings[0], substrings[1]
	}
}

func (d *Driver) getDeviceInformation(dev models.Device) (devInfo *device.GetDeviceInformationResponse, edgexErr errors.EdgeX) {
	devClient, edgexErr := d.newTemporaryOnvifClient(dev)
	if edgexErr != nil {
		return nil, errors.NewCommonEdgeXWrapper(edgexErr)
	}
	devInfoResponse, edgexErr := devClient.callOnvifFunction(onvif.DeviceWebService, onvif.GetDeviceInformation, []byte{})
	if edgexErr != nil {
		return nil, errors.NewCommonEdgeXWrapper(edgexErr)
	}
	devInfo, ok := devInfoResponse.(*device.GetDeviceInformationResponse)
	if !ok {
		return nil, errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("invalid GetDeviceInformationResponse for the camera %s", dev.Name), nil)
	}
	return devInfo, nil
}

// newOnvifClient creates a temporary client for auto-discovery
func (d *Driver) newTemporaryOnvifClient(device models.Device) (*OnvifClient, errors.EdgeX) {
	cameraInfo, edgexErr := CreateCameraInfo(device.Protocols)
	if edgexErr != nil {
		return nil, errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to create cameraInfo for camera %s", device.Name), edgexErr)
	}

	var credential config.Credentials
	if cameraInfo.AuthMode != onvif.NoAuth {
		credential, edgexErr = d.getCredentials(cameraInfo.SecretPath)
		if edgexErr != nil {
			return nil, errors.NewCommonEdgeX(errors.KindServerError, fmt.Sprintf("failed to get credentials for camera %s", device.Name), edgexErr)
		}
	}

	dev, err := onvif.NewDevice(onvif.DeviceParams{
		Xaddr:    deviceAddress(cameraInfo),
		Username: credential.Username,
		Password: credential.Password,
		AuthMode: cameraInfo.AuthMode,
		HttpClient: &http.Client{
			Timeout: time.Duration(d.config.RequestTimeout) * time.Second,
		},
	})
	if err != nil {
		return nil, errors.NewCommonEdgeX(errors.KindServiceUnavailable, "failed to initial Onvif device client", err)
	}

	client := &OnvifClient{
		lc:          d.lc,
		DeviceName:  device.Name,
		cameraInfo:  cameraInfo,
		onvifDevice: dev,
	}
	return client, nil
}
