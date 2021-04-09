// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2020-2021 IOTech Ltd
//
// SPDX-License-Identifier: Apache-2.0

package application

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	bootstrapContainer "github.com/edgexfoundry/go-mod-bootstrap/v2/bootstrap/container"
	"github.com/edgexfoundry/go-mod-bootstrap/v2/di"
	edgexErr "github.com/edgexfoundry/go-mod-core-contracts/v2/errors"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/v2"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/v2/dtos"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/v2/models"

	"github.com/edgexfoundry/device-sdk-go/v2/internal/cache"
	"github.com/edgexfoundry/device-sdk-go/v2/internal/common"
	"github.com/edgexfoundry/device-sdk-go/v2/internal/container"
	"github.com/edgexfoundry/device-sdk-go/v2/internal/transformer"
	dsModels "github.com/edgexfoundry/device-sdk-go/v2/pkg/models"
)

type CommandProcessor struct {
	device        models.Device
	sourceName    string
	correlationID string
	body          string
	attributes    string
	dic           *di.Container
}

func NewCommandProcessor(device models.Device, sourceName string, correlationID string, body string, attributes string, dic *di.Container) *CommandProcessor {
	return &CommandProcessor{
		device:        device,
		sourceName:    sourceName,
		correlationID: correlationID,
		body:          body,
		attributes:    attributes,
		dic:           dic,
	}
}

func CommandHandler(isRead bool, sendEvent bool, correlationID string, vars map[string]string, body string, attributes string, dic *di.Container) (res *dtos.Event, err edgexErr.EdgeX) {
	// check device service AdminState
	ds := container.DeviceServiceFrom(dic.Get)
	if ds.AdminState == models.Locked {
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServiceLocked, "service locked", nil)
	}

	// check provided device exists
	deviceKey := vars[v2.Name]
	device, ok := cache.Devices().ForName(deviceKey)
	if !ok {
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindEntityDoesNotExist, fmt.Sprintf("device %s not found", deviceKey), nil)
	}

	// check device's AdminState
	if device.AdminState == models.Locked {
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServiceLocked, fmt.Sprintf("device %s locked", device.Name), nil)
	}
	// check device's OperatingState
	if device.OperatingState == models.Down {
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServiceLocked, fmt.Sprintf("device %s OperatingState is DOWN", device.Name), nil)
	}
	// the device service will perform some operations(e.g. update LastConnected timestamp,
	// push returning event to core-data) after a device is successfully interacted with if
	// it has been configured to do so, and those operation apply to every protocol and
	// need to be finished in the end of application layer before returning to protocol layer.
	defer func() {
		if err != nil {
			return
		}
		config := container.ConfigurationFrom(dic.Get)
		if config.Device.UpdateLastConnected {
			go common.UpdateLastConnected(device.Name, bootstrapContainer.LoggingClientFrom(dic.Get), container.MetadataDeviceClientFrom(dic.Get))
		}

		if res != nil && sendEvent {
			go common.SendEvent(res, correlationID, dic)
		}
	}()

	cmd := vars[v2.Command]
	helper := NewCommandProcessor(device, cmd, correlationID, body, attributes, dic)
	_, cmdExist := cache.Profiles().DeviceCommand(device.ProfileName, cmd)
	if cmdExist {
		if isRead {
			return helper.ReadDeviceCommand()
		} else {
			return res, helper.WriteDeviceCommand()
		}
	} else {
		if isRead {
			return helper.ReadDeviceResource()
		} else {
			return res, helper.WriteDeviceResource()
		}
	}
}

func (c *CommandProcessor) ReadDeviceResource() (res *dtos.Event, e edgexErr.EdgeX) {
	dr, ok := cache.Profiles().DeviceResource(c.device.ProfileName, c.sourceName)
	if !ok {
		errMsg := fmt.Sprintf("deviceResource %s not found", c.sourceName)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindEntityDoesNotExist, errMsg, nil)
	}
	// check deviceResource is not write-only
	if dr.Properties.ReadWrite == v2.ReadWrite_W {
		errMsg := fmt.Sprintf("deviceResource %s is marked as write-only", dr.Name)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindNotAllowed, errMsg, nil)
	}

	lc := bootstrapContainer.LoggingClientFrom(c.dic.Get)
	lc.Debugf("Application - readDeviceResource: reading deviceResource: %s; %s: %s", dr.Name, common.CorrelationHeader, c.correlationID)

	var req dsModels.CommandRequest
	var reqs []dsModels.CommandRequest

	// prepare CommandRequest
	req.DeviceResourceName = dr.Name
	req.Attributes = dr.Attributes
	if c.attributes != "" {
		if len(req.Attributes) <= 0 {
			req.Attributes = make(map[string]string)
		}
		req.Attributes[common.URLRawQuery] = c.attributes
	}
	req.Type = dr.Properties.ValueType
	reqs = append(reqs, req)

	// execute protocol-specific read operation
	driver := container.ProtocolDriverFrom(c.dic.Get)
	results, err := driver.HandleReadCommands(c.device.Name, c.device.Protocols, reqs)
	if err != nil {
		errMsg := fmt.Sprintf("error reading DeviceResourece %s for %s", dr.Name, c.device.Name)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
	}

	// convert CommandValue to Event
	res, e = transformer.CommandValuesToEventDTO(results, c.device.Name, dr.Name, c.dic)
	if e != nil {
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "failed to convert CommandValue to Event", e)
	}

	return
}

func (c *CommandProcessor) ReadDeviceCommand() (res *dtos.Event, e edgexErr.EdgeX) {
	dc, ok := cache.Profiles().DeviceCommand(c.device.ProfileName, c.sourceName)
	if !ok {
		errMsg := fmt.Sprintf("deviceCommand %s not found", c.sourceName)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindEntityDoesNotExist, errMsg, nil)
	}
	// check deviceCommand is not write-only
	if dc.ReadWrite == v2.ReadWrite_W {
		errMsg := fmt.Sprintf("deviceCommand %s is marked as write-only", dc.Name)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindNotAllowed, errMsg, nil)
	}
	// check ResourceOperation count does not exceed MaxCmdOps defined in configuration
	configuration := container.ConfigurationFrom(c.dic.Get)
	if len(dc.ResourceOperations) > configuration.Device.MaxCmdOps {
		errMsg := fmt.Sprintf("GET command %s exceed device %s MaxCmdOps (%d)", dc.Name, c.device.Name, configuration.Device.MaxCmdOps)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, nil)
	}

	lc := bootstrapContainer.LoggingClientFrom(c.dic.Get)
	lc.Debugf("Application - readCmd: reading cmd: %s; %s: %s", dc.Name, common.CorrelationHeader, c.correlationID)

	// prepare CommandRequests
	reqs := make([]dsModels.CommandRequest, len(dc.ResourceOperations))
	for i, op := range dc.ResourceOperations {
		drName := op.DeviceResource
		// check the deviceResource in ResourceOperation actually exist
		dr, ok := cache.Profiles().DeviceResource(c.device.ProfileName, drName)
		if !ok {
			errMsg := fmt.Sprintf("deviceResource %s in GET commnd %s for %s not defined", drName, dc.Name, c.device.Name)
			return res, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, nil)
		}

		reqs[i].DeviceResourceName = dr.Name
		reqs[i].Attributes = dr.Attributes
		if c.attributes != "" {
			if len(reqs[i].Attributes) <= 0 {
				reqs[i].Attributes = make(map[string]string)
			}
			reqs[i].Attributes[common.URLRawQuery] = c.attributes
		}
		reqs[i].Type = dr.Properties.ValueType
	}

	// execute protocol-specific read operation
	driver := container.ProtocolDriverFrom(c.dic.Get)
	results, err := driver.HandleReadCommands(c.device.Name, c.device.Protocols, reqs)
	if err != nil {
		errMsg := fmt.Sprintf("error reading DeviceCommand %s for %s", dc.Name, c.device.Name)
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
	}

	// convert CommandValue to Event
	res, e = transformer.CommandValuesToEventDTO(results, c.device.Name, dc.Name, c.dic)
	if e != nil {
		return res, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "failed to transform CommandValue to Event", e)
	}

	return
}

func (c *CommandProcessor) WriteDeviceResource() edgexErr.EdgeX {
	dr, ok := cache.Profiles().DeviceResource(c.device.ProfileName, c.sourceName)
	if !ok {
		errMsg := fmt.Sprintf("deviceResource %s not found", c.sourceName)
		return edgexErr.NewCommonEdgeX(edgexErr.KindEntityDoesNotExist, errMsg, nil)
	}
	// check deviceResource is not read-only
	if dr.Properties.ReadWrite == v2.ReadWrite_R {
		errMsg := fmt.Sprintf("deviceResource %s is marked as read-only", dr.Name)
		return edgexErr.NewCommonEdgeX(edgexErr.KindNotAllowed, errMsg, nil)
	}

	lc := bootstrapContainer.LoggingClientFrom(c.dic.Get)
	lc.Debugf("Application - writeDeviceResource: writing deviceResource: %s; %s: %s", dr.Name, common.CorrelationHeader, c.correlationID)

	// parse request body string
	paramMap, err := parseParams(c.body)
	if err != nil {
		return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "failed to parse SET command parameters", err)
	}

	// check request body contains provided deviceResource
	v, ok := paramMap[dr.Name]
	if !ok {
		if dr.Properties.DefaultValue != "" {
			v = dr.Properties.DefaultValue
		} else {
			errMsg := fmt.Sprintf("deviceResource %s not found in request body and no default value defined", dr.Name)
			return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, nil)
		}
	}

	// create CommandValue
	cv, err := createCommandValueFromDeviceResource(dr, v)
	if err != nil {
		return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "failed to create CommandValue", err)
	}

	// prepare CommandRequest
	reqs := make([]dsModels.CommandRequest, 1)
	reqs[0].DeviceResourceName = cv.DeviceResourceName
	reqs[0].Attributes = dr.Attributes
	if c.attributes != "" {
		if len(reqs[0].Attributes) <= 0 {
			reqs[0].Attributes = make(map[string]string)
		}
		reqs[0].Attributes[common.URLRawQuery] = c.attributes
	}
	reqs[0].Type = cv.Type

	// transform write value
	configuration := container.ConfigurationFrom(c.dic.Get)
	if configuration.Device.DataTransform {
		err = transformer.TransformWriteParameter(cv, dr.Properties)
		if err != nil {
			return edgexErr.NewCommonEdgeX(edgexErr.KindContractInvalid, "failed to transform set parameter", err)
		}
	}

	// execute protocol-specific write operation
	driver := container.ProtocolDriverFrom(c.dic.Get)
	err = driver.HandleWriteCommands(c.device.Name, c.device.Protocols, reqs, []*dsModels.CommandValue{cv})
	if err != nil {
		errMsg := fmt.Sprintf("error writing DeviceResourece %s for %s", dr.Name, c.device.Name)
		return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
	}

	return nil
}

func (c *CommandProcessor) WriteDeviceCommand() edgexErr.EdgeX {
	dc, ok := cache.Profiles().DeviceCommand(c.device.ProfileName, c.sourceName)
	if !ok {
		errMsg := fmt.Sprintf("deviceCommand %s not found", c.sourceName)
		return edgexErr.NewCommonEdgeX(edgexErr.KindEntityDoesNotExist, errMsg, nil)
	}
	// check deviceCommand is not read-only
	if dc.ReadWrite == v2.ReadWrite_R {
		errMsg := fmt.Sprintf("deviceCommand %s is marked as read-only", dc.Name)
		return edgexErr.NewCommonEdgeX(edgexErr.KindNotAllowed, errMsg, nil)
	}
	// check ResourceOperation count does not exceed MaxCmdOps defined in configuration
	configuration := container.ConfigurationFrom(c.dic.Get)
	if len(dc.ResourceOperations) > configuration.Device.MaxCmdOps {
		errMsg := fmt.Sprintf("SET command %s exceed device %s MaxCmdOps (%d)", dc.Name, c.device.Name, configuration.Device.MaxCmdOps)
		return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, nil)
	}

	lc := bootstrapContainer.LoggingClientFrom(c.dic.Get)
	lc.Debugf("Application - writeCmd: writing command: %s; %s: %s", dc.Name, common.CorrelationHeader, c.correlationID)

	// parse request body
	paramMap, err := parseParams(c.body)
	if err != nil {
		return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "failed to parse SET command parameters", err)
	}

	// create CommandValues
	cvs := make([]*dsModels.CommandValue, 0, len(paramMap))
	for _, ro := range dc.ResourceOperations {
		drName := ro.DeviceResource
		// check the deviceResource in ResourceOperation actually exist
		dr, ok := cache.Profiles().DeviceResource(c.device.ProfileName, drName)
		if !ok {
			errMsg := fmt.Sprintf("deviceResource %s in SET commnd %s for %s not defined", drName, dc.Name, c.device.Name)
			return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, nil)
		}

		// check the deviceResource isn't read-only
		if dr.Properties.ReadWrite == v2.ReadWrite_R {
			errMsg := fmt.Sprintf("deviceResource %s in SET command %s is marked as read-only", drName, dc.Name)
			return edgexErr.NewCommonEdgeX(edgexErr.KindNotAllowed, errMsg, nil)
		}

		// check request body contains the deviceResource
		value, ok := paramMap[ro.DeviceResource]
		if !ok {
			if ro.DefaultValue != "" {
				value = ro.DefaultValue
			} else if dr.Properties.DefaultValue != "" {
				value = dr.Properties.DefaultValue
			} else {
				errMsg := fmt.Sprintf("deviceResource %s not found in request body and no default value defined", dr.Name)
				return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, nil)
			}
		}

		// write value mapping
		var match bool
		if len(ro.Mappings) > 0 {
			for k, v := range ro.Mappings {
				if v == value {
					value = k
					match = true
					break
				}
			}
			if !match {
				lc.Warnf("ResourceOperation %s mapping value (%s) failed with the mapping table: %v", ro.DeviceResource, value, ro.Mappings)
			}
		}

		// create CommandValue
		cv, err := createCommandValueFromDeviceResource(dr, value)
		if err == nil {
			cvs = append(cvs, cv)
		} else {
			return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "failed to create CommandValue", err)
		}
	}

	// prepare CommandRequests
	reqs := make([]dsModels.CommandRequest, len(cvs))
	for i, cv := range cvs {
		dr, _ := cache.Profiles().DeviceResource(c.device.ProfileName, cv.DeviceResourceName)

		reqs[i].DeviceResourceName = cv.DeviceResourceName
		reqs[i].Attributes = dr.Attributes
		if c.attributes != "" {
			if len(reqs[i].Attributes) <= 0 {
				reqs[i].Attributes = make(map[string]string)
			}
			reqs[i].Attributes[common.URLRawQuery] = c.attributes
		}
		reqs[i].Type = cv.Type

		// transform write value
		if configuration.Device.DataTransform {
			err = transformer.TransformWriteParameter(cv, dr.Properties)
			if err != nil {
				return edgexErr.NewCommonEdgeX(edgexErr.KindContractInvalid, "failed to transform set parameter", err)
			}
		}
	}

	// execute protocol-specific write operation
	driver := container.ProtocolDriverFrom(c.dic.Get)
	err = driver.HandleWriteCommands(c.device.Name, c.device.Protocols, reqs, cvs)
	if err != nil {
		errMsg := fmt.Sprintf("error writing DeviceCommand %s for %s", dc.Name, c.device.Name)
		return edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
	}

	return nil
}

func parseParams(params string) (paramMap map[string]string, err error) {
	err = json.Unmarshal([]byte(params), &paramMap)
	if err != nil {
		return
	}

	if len(paramMap) == 0 {
		err = fmt.Errorf("no parameters specified")
		return
	}

	return
}

func createCommandValueFromDeviceResource(dr models.DeviceResource, v string) (*dsModels.CommandValue, edgexErr.EdgeX) {
	var err edgexErr.EdgeX
	var result *dsModels.CommandValue

	switch dr.Properties.ValueType {
	case v2.ValueTypeString:
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeString, v)
	case v2.ValueTypeBool:
		value, err := strconv.ParseBool(v)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeBool, value)
	case v2.ValueTypeBoolArray:
		var arr []bool
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeBoolArray, arr)
	case v2.ValueTypeUint8:
		n, err := strconv.ParseUint(v, 10, 8)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint8, uint8(n))
	case v2.ValueTypeUint8Array:
		var arr []uint8
		strArr := strings.Split(strings.Trim(v, "[]"), ",")
		for _, u := range strArr {
			n, err := strconv.ParseUint(strings.Trim(u, " "), 10, 8)
			if err != nil {
				errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
				return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
			}
			arr = append(arr, uint8(n))
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint8Array, arr)
	case v2.ValueTypeUint16:
		n, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint16, uint16(n))
	case v2.ValueTypeUint16Array:
		var arr []uint16
		strArr := strings.Split(strings.Trim(v, "[]"), ",")
		for _, u := range strArr {
			n, err := strconv.ParseUint(strings.Trim(u, " "), 10, 16)
			if err != nil {
				errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
				return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
			}
			arr = append(arr, uint16(n))
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint16Array, arr)
	case v2.ValueTypeUint32:
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint32, uint32(n))
	case v2.ValueTypeUint32Array:
		var arr []uint32
		strArr := strings.Split(strings.Trim(v, "[]"), ",")
		for _, u := range strArr {
			n, err := strconv.ParseUint(strings.Trim(u, " "), 10, 32)
			if err != nil {
				errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
				return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
			}
			arr = append(arr, uint32(n))
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint32Array, arr)
	case v2.ValueTypeUint64:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint64, n)
	case v2.ValueTypeUint64Array:
		var arr []uint64
		strArr := strings.Split(strings.Trim(v, "[]"), ",")
		for _, u := range strArr {
			n, err := strconv.ParseUint(strings.Trim(u, " "), 10, 64)
			if err != nil {
				errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
				return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
			}
			arr = append(arr, n)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeUint64Array, arr)
	case v2.ValueTypeInt8:
		n, err := strconv.ParseInt(v, 10, 8)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt8, int8(n))
	case v2.ValueTypeInt8Array:
		var arr []int8
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt8Array, arr)
	case v2.ValueTypeInt16:
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt16, int16(n))
	case v2.ValueTypeInt16Array:
		var arr []int16
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt16Array, arr)
	case v2.ValueTypeInt32:
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt32, int32(n))
	case v2.ValueTypeInt32Array:
		var arr []int32
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt32Array, arr)
	case v2.ValueTypeInt64:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt64, n)
	case v2.ValueTypeInt64Array:
		var arr []int64
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeInt64Array, arr)
	case v2.ValueTypeFloat32:
		val, err := strconv.ParseFloat(v, 32)
		if err == nil {
			result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeFloat32, float32(val))
			break
		}
		if numError, ok := err.(*strconv.NumError); ok {
			if numError.Err == strconv.ErrRange {
				err = edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "NumError", err)
				break
			}
		}
		var decodedToBytes []byte
		decodedToBytes, err = base64.StdEncoding.DecodeString(v)
		if err == nil {
			var val float32
			val, err = float32FromBytes(decodedToBytes)
			if err != nil {
				break
			} else if math.IsNaN(float64(val)) {
				err = fmt.Errorf("fail to parse %v to float32, unexpected result %v", v, val)
			} else {
				result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeFloat32, val)
			}
		}
	case v2.ValueTypeFloat32Array:
		var arr []float32
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeFloat32Array, arr)
	case v2.ValueTypeFloat64:
		var val float64
		val, err := strconv.ParseFloat(v, 64)
		if err == nil {
			result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeFloat64, val)
			break
		}
		if numError, ok := err.(*strconv.NumError); ok {
			if numError.Err == strconv.ErrRange {
				err = edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "NumError", err)
				break
			}
		}
		var decodedToBytes []byte
		decodedToBytes, err = base64.StdEncoding.DecodeString(v)
		if err == nil {
			val, err = float64FromBytes(decodedToBytes)
			if err != nil {
				break
			} else if math.IsNaN(val) {
				err = fmt.Errorf("fail to parse %v to float64, unexpected result %v", v, val)
			} else {
				result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeFloat64, val)
			}
		}
	case v2.ValueTypeFloat64Array:
		var arr []float64
		err := json.Unmarshal([]byte(v), &arr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to convert set parameter %s to ValueType %s", v, dr.Properties.ValueType)
			return result, edgexErr.NewCommonEdgeX(edgexErr.KindServerError, errMsg, err)
		}
		result, err = dsModels.NewCommandValue(dr.Name, v2.ValueTypeFloat64Array, arr)
	default:
		err = edgexErr.NewCommonEdgeX(edgexErr.KindServerError, "unrecognized value type", nil)
	}

	return result, err
}

func float32FromBytes(numericValue []byte) (res float32, err error) {
	reader := bytes.NewReader(numericValue)
	err = binary.Read(reader, binary.BigEndian, &res)
	return
}

func float64FromBytes(numericValue []byte) (res float64, err error) {
	reader := bytes.NewReader(numericValue)
	err = binary.Read(reader, binary.BigEndian, &res)
	return
}
