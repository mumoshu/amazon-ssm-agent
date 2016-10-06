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

// Package processor manage polling of associations, dispatching association to processor
package processor

import (
	"fmt"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/association/executer"
	"github.com/aws/amazon-ssm-agent/agent/association/model"
	"github.com/aws/amazon-ssm-agent/agent/association/recorder"
	assocScheduler "github.com/aws/amazon-ssm-agent/agent/association/scheduler"
	"github.com/aws/amazon-ssm-agent/agent/association/service"
	"github.com/aws/amazon-ssm-agent/agent/association/taskpool"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	stateModel "github.com/aws/amazon-ssm-agent/agent/statemanager/model"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/carlescere/scheduler"
)

const (
	name                          = "Association"
	documentWorkersLimit          = 1
	cancelWaitDurationMillisecond = 10000
)

// Processor contains the logic for processing association
type Processor struct {
	pollJob    *scheduler.Job
	assocSvc   service.T
	executer   executer.DocumentExecuter
	context    context.T
	taskPool   taskpool.T
	agentInfo  *contracts.AgentInfo
	stopSignal chan bool
}

// NewAssociationProcessor returns a new Processor with the given context.
func NewAssociationProcessor(context context.T, instanceID string) *Processor {
	assocContext := context.With("[" + name + "]")

	log := assocContext.Log()
	config := assocContext.AppConfig()

	taskPool := taskpool.NewTaskPool(log, documentWorkersLimit, cancelWaitDurationMillisecond)

	agentInfo := contracts.AgentInfo{
		Lang:      config.Os.Lang,
		Name:      config.Agent.Name,
		Version:   config.Agent.Version,
		Os:        config.Os.Name,
		OsVersion: config.Os.Version,
	}

	assocSvc := service.NewAssociationService(name)
	executer := executer.NewAssociationExecuter(assocSvc, &agentInfo)

	return &Processor{
		context:    assocContext,
		assocSvc:   assocSvc,
		taskPool:   taskPool,
		executer:   executer,
		agentInfo:  &agentInfo,
		stopSignal: make(chan bool),
	}
}

// SetPollJob represents setter for PollJob
func (p *Processor) SetPollJob(job *scheduler.Job) {
	p.pollJob = job
}

// ProcessAssociation poll and process all the associations
func (p *Processor) ProcessAssociation() {
	log := p.context.Log()
	var assocRawData *model.AssociationRawData

	if p.isStopped() {
		log.Debug("Stopping association processor...")
		return
	}

	instanceID, err := sys.InstanceID()
	if err != nil {
		log.Error("Unable to retrieve instance id", err)
		return
	}

	p.assocSvc.CreateNewServiceIfUnHealthy(log)

	if assocRawData, err = p.assocSvc.ListAssociations(log, instanceID); err != nil {
		log.Errorf("Failed to retrieve associations, %v", err)
		return
	}

	// check if association response is empty
	if assocRawData == nil {
		log.Infof("No documents are associated with instance %v", instanceID)
		return
	}

	// check if the association has been executed
	assoName := assocRawData.Association.Name

	if recorder.HasExecuted(instanceID, *assoName) {
		log.Debugf("DocumentId %v hasn't changed. Skipping as it has been processed already.", *assoName)
		return
	} else {
		log.Debugf("New document associated %v.", *assoName)
	}

	if err = p.assocSvc.LoadAssociationDetail(log, assocRawData); err != nil {
		message := fmt.Sprintf("Unable to load association details, %v", err)
		log.Error(message)
		p.updateAssocStatus(assocRawData.Association, ssm.AssociationStatusNameFailed, message)
		return
	}
	var docState *stateModel.DocumentState
	if docState, err = p.parseAssociation(assocRawData); err != nil {
		message := fmt.Sprintf("Unable to parse association, %v", err)
		log.Error(message)
		p.updateAssocStatus(assocRawData.Association, ssm.AssociationStatusNameFailed, message)
		return
	}

	if err = p.persistAssociationForExecution(log, docState); err != nil {
		message := fmt.Sprintf("Unable to submit association for exectution, %v", err)
		log.Error(message)
		p.updateAssocStatus(assocRawData.Association, ssm.AssociationStatusNameFailed, message)
		return
	}
}

// ExecutePendingDocument wraps ExecutePendingDocument from document executer
func (p *Processor) ExecutePendingDocument(docState *stateModel.DocumentState) {
	log := p.context.Log()
	if err := p.executer.ExecutePendingDocument(p.context, p.taskPool, docState); err != nil {
		log.Error("Failed to execute pending documents ", err)
	}
}

// ExecuteInProgressDocument wraps ExecuteInProgressDocument from document executer
func (p *Processor) ExecuteInProgressDocument(docState *stateModel.DocumentState, cancelFlag task.CancelFlag) {
	p.executer.ExecuteInProgressDocument(p.context, docState, cancelFlag)
}

// SubmitTask wraps SubmitTask for taskpool
func (p *Processor) SubmitTask(log log.T, jobID string, job task.Job) error {
	return p.taskPool.Submit(log, jobID, job)
}

// ShutdownAndWait wraps the ShutdownAndWait for task pool
func (p *Processor) ShutdownAndWait(timeout time.Duration) {
	p.taskPool.ShutdownAndWait(timeout)
}

// Stop stops the association processor and stops the poll job
func (p *Processor) Stop() {
	// close channel; subsequent calls to isDone will return true
	if !p.isStopped() {
		close(p.stopSignal)
	}

	assocScheduler.Stop(p.pollJob)
}

// isStopped returns if the association processor has been stopped
func (p *Processor) isStopped() bool {
	select {
	case <-p.stopSignal:
		// received signal or channel already closed
		return true
	default:
		return false
	}
}

// parseAssociation parses the association to the document state
func (p *Processor) parseAssociation(rawData *model.AssociationRawData) (*stateModel.DocumentState, error) {
	// create separate logger that includes messageID with every log message
	context := p.context.With("[associationName=" + *rawData.Association.Name + "]")
	log := context.Log()
	docState := stateModel.DocumentState{}

	log.Debug("Processing association")

	document, err := assocParser.ParseDocumentWithParams(log, rawData)
	if err != nil {
		return &docState, fmt.Errorf("failed to parse association, %v", err)
	}

	var parsedMessageContent string
	if parsedMessageContent, err = jsonutil.Marshal(document); err != nil {
		return &docState, fmt.Errorf("failed to parse document, %v", err)
	}
	log.Debug("ParsedAssociation is ", jsonutil.Indent(parsedMessageContent))

	//Data format persisted in Current Folder is defined by the struct - DocumentState
	docState = assocParser.InitializeDocumentState(context, document, rawData)

	isMI, err := sys.IsManagedInstance()
	if err != nil {
		return &docState, fmt.Errorf("error determining managed instance, %v", err)
	}

	if isMI {
		log.Debugf("Running Incompatible AWS SSM Document %v on managed instance", docState.DocumentInformation.DocumentName)
		if err = stateModel.RemoveDependencyOnInstanceMetadata(context, &docState); err != nil {
			return &docState, err
		}
	}

	return &docState, nil
}

// persistAssociationForExecution saves the document to pending folder and submit it to the task pool
func (p *Processor) persistAssociationForExecution(log log.T, docState *stateModel.DocumentState) error {
	log.Debug("Persisting interim state in current execution folder")
	assocBookkeeping.PersistData(log,
		docState.DocumentInformation.CommandID,
		docState.DocumentInformation.Destination,
		appconfig.DefaultLocationOfPending,
		docState)

	// record the last executed association file
	if err := recorder.UpdateAssociatedDocument(docState.DocumentInformation.Destination, docState.DocumentInformation.DocumentName); err != nil {
		log.Errorf("Failed to persist last executed association document, %v", err)
	}

	return p.executer.ExecutePendingDocument(p.context, p.taskPool, docState)
}

// updateAssociationStatus provides wrapper for calling update association service
func (p *Processor) updateAssocStatus(
	assoc *ssm.Association,
	status string,
	message string) {
	log := p.context.Log()

	p.assocSvc.UpdateAssociationStatus(
		log,
		*assoc.InstanceId,
		*assoc.Name,
		status,
		message,
		p.agentInfo)
}