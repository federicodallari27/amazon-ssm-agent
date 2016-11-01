// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package parser contains utilities for parsing and encoding MDS/SSM messages.
package parser

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/association/model"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	messageContracts "github.com/aws/amazon-ssm-agent/agent/message/contracts"
	messageParser "github.com/aws/amazon-ssm-agent/agent/message/parser"
	"github.com/aws/amazon-ssm-agent/agent/parameters"
	stateModel "github.com/aws/amazon-ssm-agent/agent/statemanager/model"
	"github.com/aws/amazon-ssm-agent/agent/times"
)

// ParseDocumentWithParams parses an document and replaces the parameters where needed.
func ParseDocumentWithParams(log log.T,
	rawData *model.InstanceAssociation) (*messageContracts.SendCommandPayload, error) {

	rawDataContent, err := jsonutil.Marshal(rawData)
	if err != nil {
		log.Error("Could not marshal association! ", err)
		return nil, err
	}
	log.Debug("Processing assocation ", jsonutil.Indent(rawDataContent))

	payload := &messageContracts.SendCommandPayload{}

	if err = json.Unmarshal([]byte(*rawData.Document), &payload.DocumentContent); err != nil {
		return nil, err
	}
	payload.DocumentName = *rawData.Association.Name
	payload.CommandID = *rawData.Association.AssociationId

	if rawData.Association.OutputLocation != nil && rawData.Association.OutputLocation.S3Location != nil {
		if rawData.Association.OutputLocation.S3Location.OutputS3KeyPrefix != nil {
			payload.OutputS3KeyPrefix = *rawData.Association.OutputLocation.S3Location.OutputS3KeyPrefix
		}
		if rawData.Association.OutputLocation.S3Location.OutputS3BucketName != nil {
			payload.OutputS3BucketName = *rawData.Association.OutputLocation.S3Location.OutputS3BucketName
		}
	}

	payload.Parameters = parseParameters(log, rawData.Association.Parameters, payload.DocumentContent.Parameters)

	var parametersContent string
	if parametersContent, err = jsonutil.Marshal(payload.Parameters); err != nil {
		log.Error("Could not marshal parameters ", err)
		return nil, err
	}
	log.Debug("After marshal parameters ", jsonutil.Indent(parametersContent))

	validParams := parameters.ValidParameters(log, payload.Parameters)
	// add default values for missing parameters
	for k, v := range payload.DocumentContent.Parameters {
		if _, ok := validParams[k]; !ok {
			validParams[k] = v.DefaultVal
		}
	}

	err = messageParser.ReplacePluginParameters(payload, validParams, log)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// InitializeDocumentState - an interim state that is used around during an execution of a document
func InitializeDocumentState(context context.T,
	payload *messageContracts.SendCommandPayload,
	rawData *model.InstanceAssociation) stateModel.DocumentState {

	//initialize document information with relevant values extracted from msg
	documentInfo := newDocumentInfo(rawData, payload)
	// adapt plugin configuration format from MDS to plugin expected format
	s3KeyPrefix := path.Join(payload.OutputS3KeyPrefix, payload.CommandID, documentInfo.InstanceID)

	orchestrationRootDir := filepath.Join(
		appconfig.DefaultDataStorePath,
		documentInfo.InstanceID,
		appconfig.DefaultDocumentRootDirName,
		context.AppConfig().Agent.OrchestrationRootDir)

	orchestrationDir := filepath.Join(orchestrationRootDir, documentInfo.DocumentID)
	docState := stateModel.DocumentState{
		DocumentInformation: documentInfo,
		DocumentType:        stateModel.Association,
		SchemaVersion:       payload.DocumentContent.SchemaVersion,
	}

	buildPluginsInfo(payload, documentInfo, s3KeyPrefix, orchestrationDir, &docState)

	return docState
}

// newDocumentInfo initializes new DocumentInfo object
func newDocumentInfo(rawData *model.InstanceAssociation, payload *messageContracts.SendCommandPayload) stateModel.DocumentInfo {

	documentInfo := new(stateModel.DocumentInfo)

	documentInfo.AssociationID = *(rawData.Association.AssociationId)
	documentInfo.InstanceID = *(rawData.Association.InstanceId)
	documentInfo.MessageID = fmt.Sprintf("aws.ssm.%v.%v", documentInfo.AssociationID, documentInfo.InstanceID)
	documentInfo.RunID = times.ToIsoDashUTC(times.DefaultClock.Now())
	documentInfo.DocumentID = *(rawData.Association.AssociationId) + "." + documentInfo.RunID
	rawData.DocumentID = documentInfo.DocumentID
	documentInfo.CreatedDate = times.ToIso8601UTC(rawData.CreateDate)
	documentInfo.DocumentName = payload.DocumentName
	documentInfo.IsCommand = false
	documentInfo.DocumentStatus = contracts.ResultStatusInProgress
	documentInfo.RunOnce = rawData.RunOnce
	documentInfo.DocumentTraceOutput = ""

	return *documentInfo
}

func parseParameters(log log.T, params map[string][]*string, paramsDef map[string]*contracts.Parameter) map[string]interface{} {
	result := make(map[string]interface{})

	for name, param := range params {

		if definition, ok := paramsDef[name]; ok {
			switch definition.ParamType {
			case contracts.ParamTypeString:
				result[name] = param[0]
			case contracts.ParamTypeStringList:
				result[name] = param
			default:
				log.Debug("unknown parameter type ", definition.ParamType)
			}
		}
	}
	return result
}

// buildPluginsInfo builds the PluginsInfo for document state
func buildPluginsInfo(
	payload *messageContracts.SendCommandPayload,
	documentInfo stateModel.DocumentInfo,
	s3KeyPrefix string,
	orchestrationDir string,
	docState *stateModel.DocumentState) {

	if payload.DocumentContent.RuntimeConfig != nil && len(payload.DocumentContent.RuntimeConfig) != 0 {
		//initialize plugin states as map
		pluginsInfo := make(map[string]stateModel.PluginState)

		// getPluginConfigurations converts from PluginConfig (structure from the MDS message) to plugin.Configuration (structure expected by the plugin)
		pluginConfigurations := make(map[string]*contracts.Configuration)
		for pluginName, pluginConfig := range payload.DocumentContent.RuntimeConfig {
			config := contracts.Configuration{
				Settings:               pluginConfig.Settings,
				Properties:             pluginConfig.Properties,
				OutputS3BucketName:     payload.OutputS3BucketName,
				OutputS3KeyPrefix:      fileutil.BuildS3Path(s3KeyPrefix, pluginName),
				OrchestrationDirectory: fileutil.BuildPath(orchestrationDir, pluginName),
				MessageId:              documentInfo.MessageID,
				BookKeepingFileName:    documentInfo.DocumentID,
			}
			pluginConfigurations[pluginName] = &config
		}

		for key, value := range pluginConfigurations {
			var plugin stateModel.PluginState
			plugin.Configuration = *value
			plugin.HasExecuted = false
			plugin.Id = key
			plugin.Name = key
			pluginsInfo[key] = plugin
		}

		docState.PluginsInformation = pluginsInfo
		return
	}

	if payload.DocumentContent.MainSteps != nil && len(payload.DocumentContent.MainSteps) != 0 {
		//initialize plugin states as array
		instancePluginsInfo := make([]stateModel.PluginState, len(payload.DocumentContent.MainSteps))

		// getPluginConfigurations converts from PluginConfig (structure from the MDS message) to plugin.Configuration (structure expected by the plugin)

		for index, instancePluginConfig := range payload.DocumentContent.MainSteps {

			pluginName := instancePluginConfig.Action
			config := contracts.Configuration{
				Settings:               instancePluginConfig.Settings,
				Properties:             instancePluginConfig.Inputs,
				OutputS3BucketName:     payload.OutputS3BucketName,
				OutputS3KeyPrefix:      fileutil.BuildS3Path(s3KeyPrefix, pluginName),
				OrchestrationDirectory: fileutil.BuildPath(orchestrationDir, pluginName),
				MessageId:              documentInfo.MessageID,
				//BookKeepingFileName:    payload.CommandID,
				BookKeepingFileName: documentInfo.DocumentID,
			}

			var plugin stateModel.PluginState
			plugin.Configuration = config
			plugin.HasExecuted = false
			plugin.Id = instancePluginConfig.Name
			plugin.Name = pluginName
			instancePluginsInfo[index] = plugin
		}
		docState.InstancePluginsInformation = instancePluginsInfo
		return
	}

	return
}