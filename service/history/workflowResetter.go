// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

//go:generate mockgen -copyright_file ../../LICENSE -package $GOPACKAGE -source $GOFILE -destination workflowResetter_mock.go

package history

import (
	"context"
	"fmt"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"

	"go.temporal.io/server/common"
	"go.temporal.io/server/common/cache"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/collection"
	"go.temporal.io/server/common/convert"
	"go.temporal.io/server/common/definition"
	"go.temporal.io/server/common/failure"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/primitives/timestamp"
)

type (
	workflowResetter interface {
		// resetWorkflow is the new NDC compatible workflow reset logic
		resetWorkflow(
			ctx context.Context,
			namespaceID string,
			workflowID string,
			baseRunID string,
			baseBranchToken []byte,
			baseRebuildLastEventID int64,
			baseRebuildLastEventVersion int64,
			baseNextEventID int64,
			resetRunID string,
			resetRequestID string,
			currentWorkflow nDCWorkflow,
			resetReason string,
			additionalReapplyEvents []*historypb.HistoryEvent,
		) error
	}

	nDCStateRebuilderProvider func() nDCStateRebuilder

	workflowResetterImpl struct {
		shard             ShardContext
		namespaceCache    cache.NamespaceCache
		clusterMetadata   cluster.Metadata
		historyV2Mgr      persistence.HistoryManager
		historyCache      *historyCache
		newStateRebuilder nDCStateRebuilderProvider
		logger            log.Logger
	}
)

var _ workflowResetter = (*workflowResetterImpl)(nil)

func newWorkflowResetter(
	shard ShardContext,
	historyCache *historyCache,
	logger log.Logger,
) *workflowResetterImpl {
	return &workflowResetterImpl{
		shard:           shard,
		namespaceCache:  shard.GetNamespaceCache(),
		clusterMetadata: shard.GetClusterMetadata(),
		historyV2Mgr:    shard.GetHistoryManager(),
		historyCache:    historyCache,
		newStateRebuilder: func() nDCStateRebuilder {
			return newNDCStateRebuilder(shard, logger)
		},
		logger: logger,
	}
}

func (r *workflowResetterImpl) resetWorkflow(
	ctx context.Context,
	namespaceID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildLastEventID int64,
	baseRebuildLastEventVersion int64,
	baseNextEventID int64,
	resetRunID string,
	resetRequestID string,
	currentWorkflow nDCWorkflow,
	resetReason string,
	additionalReapplyEvents []*historypb.HistoryEvent,
) (retError error) {

	namespaceEntry, err := r.namespaceCache.GetNamespaceByID(namespaceID)
	if err != nil {
		return err
	}
	resetWorkflowVersion := namespaceEntry.GetFailoverVersion()

	currentMutableState := currentWorkflow.getMutableState()
	currentWorkflowTerminated := false
	if currentMutableState.IsWorkflowExecutionRunning() {
		if err := r.terminateWorkflow(
			currentMutableState,
			resetReason,
		); err != nil {
			return err
		}
		resetWorkflowVersion = currentMutableState.GetCurrentVersion()
		currentWorkflowTerminated = true
	}

	resetWorkflow, err := r.prepareResetWorkflow(
		ctx,
		namespaceID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		baseNextEventID,
		resetRunID,
		resetRequestID,
		resetWorkflowVersion,
		resetReason,
		additionalReapplyEvents,
	)
	if err != nil {
		return err
	}
	defer resetWorkflow.getReleaseFn()(retError)

	return r.persistToDB(
		currentWorkflowTerminated,
		currentWorkflow,
		resetWorkflow,
	)
}

func (r *workflowResetterImpl) prepareResetWorkflow(
	ctx context.Context,
	namespaceID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildLastEventID int64,
	baseRebuildLastEventVersion int64,
	baseNextEventID int64,
	resetRunID string,
	resetRequestID string,
	resetWorkflowVersion int64,
	resetReason string,
	additionalReapplyEvents []*historypb.HistoryEvent,
) (nDCWorkflow, error) {

	resetWorkflow, err := r.replayResetWorkflow(
		ctx,
		namespaceID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		resetRunID,
		resetRequestID,
	)
	if err != nil {
		return nil, err
	}

	// Reset expiration time
	resetMutableState := resetWorkflow.getMutableState()
	executionInfo := resetMutableState.GetExecutionInfo()

	weTimeout := timestamp.DurationValue(executionInfo.WorkflowExecutionTimeout)
	if weTimeout > 0 {
		executionInfo.WorkflowExpirationTime = timestamp.TimeNowPtrUtcAddDuration(weTimeout)
	}

	baseLastEventVersion := resetMutableState.GetCurrentVersion()
	if baseLastEventVersion > resetWorkflowVersion {
		return nil, serviceerror.NewInternal("workflowResetter encounter version mismatch.")
	}
	if err := resetMutableState.UpdateCurrentVersion(
		resetWorkflowVersion,
		false,
	); err != nil {
		return nil, err
	}

	// TODO add checking of reset until event ID == workflow task started ID + 1
	workflowTask, ok := resetMutableState.GetInFlightWorkflowTask()
	if !ok || workflowTask.StartedID+1 != resetMutableState.GetNextEventID() {
		return nil, serviceerror.NewInvalidArgument(fmt.Sprintf("Can only reset workflow to WorkflowTaskStarted + 1: %v", baseRebuildLastEventID+1))
	}
	if len(resetMutableState.GetPendingChildExecutionInfos()) > 0 {
		return nil, serviceerror.NewInvalidArgument(fmt.Sprintf("Can only reset workflow with pending child workflows"))
	}

	resetFailure := failure.NewResetWorkflowFailure(resetReason, nil)
	_, err = resetMutableState.AddWorkflowTaskFailedEvent(
		workflowTask.ScheduleID,
		workflowTask.StartedID, enumspb.WORKFLOW_TASK_FAILED_CAUSE_RESET_WORKFLOW,
		resetFailure,
		identityHistoryService,
		"",
		baseRunID,
		resetRunID,
		baseLastEventVersion,
	)
	if err != nil {
		return nil, err
	}

	if err := r.failInflightActivity(resetMutableState, resetReason); err != nil {
		return nil, err
	}

	if err := r.reapplyContinueAsNewWorkflowEvents(
		ctx,
		resetMutableState,
		namespaceID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseRebuildLastEventID+1,
		baseNextEventID,
	); err != nil {
		return nil, err
	}

	if err := r.reapplyEvents(resetMutableState, additionalReapplyEvents); err != nil {
		return nil, err
	}

	if err := scheduleWorkflowTask(resetMutableState); err != nil {
		return nil, err
	}

	return resetWorkflow, nil
}

func (r *workflowResetterImpl) persistToDB(
	currentWorkflowTerminated bool,
	currentWorkflow nDCWorkflow,
	resetWorkflow nDCWorkflow,
) error {

	if currentWorkflowTerminated {
		return currentWorkflow.getContext().updateWorkflowExecutionWithNewAsActive(
			r.shard.GetTimeSource().Now(),
			resetWorkflow.getContext(),
			resetWorkflow.getMutableState(),
		)
	}

	currentMutableState := currentWorkflow.getMutableState()
	currentRunID := currentMutableState.GetExecutionInfo().GetRunId()
	currentLastWriteVersion, err := currentMutableState.GetLastWriteVersion()
	if err != nil {
		return err
	}

	now := r.shard.GetTimeSource().Now()
	resetWorkflowSnapshot, resetWorkflowEventsSeq, err := resetWorkflow.getMutableState().CloseTransactionAsSnapshot(
		now,
		transactionPolicyActive,
	)
	if err != nil {
		return err
	}
	resetHistorySize, err := resetWorkflow.getContext().persistNonFirstWorkflowEvents(resetWorkflowEventsSeq[0])
	if err != nil {
		return err
	}

	return resetWorkflow.getContext().createWorkflowExecution(
		resetWorkflowSnapshot,
		resetHistorySize,
		now,
		persistence.CreateWorkflowModeContinueAsNew,
		currentRunID,
		currentLastWriteVersion,
	)
}

func (r *workflowResetterImpl) replayResetWorkflow(
	ctx context.Context,
	namespaceID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildLastEventID int64,
	baseRebuildLastEventVersion int64,
	resetRunID string,
	resetRequestID string,
) (nDCWorkflow, error) {

	resetBranchToken, err := r.forkAndGenerateBranchToken(
		namespaceID,
		workflowID,
		baseBranchToken,
		baseRebuildLastEventID+1,
		resetRunID,
	)
	if err != nil {
		return nil, err
	}

	resetContext := newWorkflowExecutionContext(
		namespaceID,
		commonpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      resetRunID,
		},
		r.shard,
		r.shard.GetExecutionManager(),
		r.logger,
	)
	resetMutableState, resetHistorySize, err := r.newStateRebuilder().rebuild(
		ctx,
		timestamp.TimePtr(r.shard.GetTimeSource().Now()),
		definition.NewWorkflowIdentifier(
			namespaceID,
			workflowID,
			baseRunID,
		),
		baseBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		definition.NewWorkflowIdentifier(
			namespaceID,
			workflowID,
			resetRunID,
		),
		resetBranchToken,
		resetRequestID,
	)
	if err != nil {
		return nil, err
	}

	resetContext.setHistorySize(resetHistorySize)
	return newNDCWorkflow(
		ctx,
		r.namespaceCache,
		r.clusterMetadata,
		resetContext,
		resetMutableState,
		noopReleaseFn,
	), nil
}

func (r *workflowResetterImpl) failInflightActivity(
	mutableState mutableState,
	terminateReason string,
) error {

	for _, ai := range mutableState.GetPendingActivityInfos() {
		switch ai.StartedId {
		case common.EmptyEventID:
			// activity not started, noop
		case common.TransientEventID:
			// activity is started (with retry policy)
			// should not encounter this case when rebuilding mutable state
			return serviceerror.NewInternal("workflowResetter encounter transient activity")
		default:
			if _, err := mutableState.AddActivityTaskFailedEvent(
				ai.ScheduleId,
				ai.StartedId,
				failure.NewResetWorkflowFailure(terminateReason, ai.LastHeartbeatDetails),
				enumspb.RETRY_STATE_NON_RETRYABLE_FAILURE,
				ai.StartedIdentity,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *workflowResetterImpl) forkAndGenerateBranchToken(
	namespaceID string,
	workflowID string,
	forkBranchToken []byte,
	forkNodeID int64,
	resetRunID string,
) ([]byte, error) {
	// fork a new history branch
	shardID := r.shard.GetShardID()
	resp, err := r.historyV2Mgr.ForkHistoryBranch(&persistence.ForkHistoryBranchRequest{
		ForkBranchToken: forkBranchToken,
		ForkNodeID:      forkNodeID,
		Info:            persistence.BuildHistoryGarbageCleanupInfo(namespaceID, workflowID, resetRunID),
		ShardID:         convert.IntPtr(shardID),
	})
	if err != nil {
		return nil, err
	}

	return resp.NewBranchToken, nil
}

func (r *workflowResetterImpl) terminateWorkflow(
	mutableState mutableState,
	terminateReason string,
) error {

	eventBatchFirstEventID := mutableState.GetNextEventID()
	return terminateWorkflow(
		mutableState,
		eventBatchFirstEventID,
		terminateReason,
		nil,
		identityHistoryService,
	)
}

func (r *workflowResetterImpl) reapplyContinueAsNewWorkflowEvents(
	ctx context.Context,
	resetMutableState mutableState,
	namespaceID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildNextEventID int64,
	baseNextEventID int64,
) error {

	// TODO change this logic to fetching all workflow [baseWorkflow, currentWorkflow]
	//  from visibility for better coverage of events eligible for re-application.

	var nextRunID string
	var err error

	// first special handling the remaining events for base workflow
	if nextRunID, err = r.reapplyWorkflowEvents(
		resetMutableState,
		baseRebuildNextEventID,
		baseNextEventID,
		baseBranchToken,
	); err != nil {
		return err
	}

	getNextEventIDBranchToken := func(runID string) (nextEventID int64, branchToken []byte, retError error) {
		context, release, err := r.historyCache.getOrCreateWorkflowExecution(
			ctx,
			namespaceID,
			commonpb.WorkflowExecution{
				WorkflowId: workflowID,
				RunId:      runID,
			},
		)
		if err != nil {
			return 0, nil, err
		}
		defer func() { release(retError) }()

		mutableState, err := context.loadWorkflowExecution()
		if err != nil {
			// no matter what error happen, we need to retry
			return 0, nil, err
		}

		nextEventID = mutableState.GetNextEventID()
		branchToken, err = mutableState.GetCurrentBranchToken()
		if err != nil {
			return 0, nil, err
		}
		return nextEventID, branchToken, nil
	}

	// second for remaining continue as new workflow, reapply eligible events
	for len(nextRunID) != 0 {
		nextWorkflowNextEventID, nextWorkflowBranchToken, err := getNextEventIDBranchToken(nextRunID)
		if err != nil {
			return err
		}

		if nextRunID, err = r.reapplyWorkflowEvents(
			resetMutableState,
			common.FirstEventID,
			nextWorkflowNextEventID,
			nextWorkflowBranchToken,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *workflowResetterImpl) reapplyWorkflowEvents(
	mutableState mutableState,
	firstEventID int64,
	nextEventID int64,
	branchToken []byte,
) (string, error) {

	// TODO change this logic to fetching all workflow [baseWorkflow, currentWorkflow]
	//  from visibility for better coverage of events eligible for re-application.
	//  after the above change, this API do not have to return the continue as new run ID

	iter := collection.NewPagingIterator(r.getPaginationFn(
		firstEventID,
		nextEventID,
		branchToken,
	))

	var nextRunID string
	var lastEvents []*historypb.HistoryEvent

	for iter.HasNext() {
		batch, err := iter.Next()
		if err != nil {
			return "", err
		}
		lastEvents = batch.(*historypb.History).Events
		if err := r.reapplyEvents(mutableState, lastEvents); err != nil {
			return "", err
		}
	}

	if len(lastEvents) > 0 {
		lastEvent := lastEvents[len(lastEvents)-1]
		if lastEvent.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW {
			nextRunID = lastEvent.GetWorkflowExecutionContinuedAsNewEventAttributes().GetNewExecutionRunId()
		}
	}
	return nextRunID, nil
}

func (r *workflowResetterImpl) reapplyEvents(
	mutableState mutableState,
	events []*historypb.HistoryEvent,
) error {

	for _, event := range events {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
			attr := event.GetWorkflowExecutionSignaledEventAttributes()
			if _, err := mutableState.AddWorkflowExecutionSignaled(
				attr.GetSignalName(),
				attr.GetInput(),
				attr.GetIdentity(),
			); err != nil {
				return err
			}
		default:
			// events other than signal will be ignored
		}
	}
	return nil
}

func (r *workflowResetterImpl) getPaginationFn(
	firstEventID int64,
	nextEventID int64,
	branchToken []byte,
) collection.PaginationFn {

	return func(paginationToken []byte) ([]interface{}, []byte, error) {

		_, historyBatches, token, _, err := PaginateHistory(
			r.historyV2Mgr,
			true,
			branchToken,
			firstEventID,
			nextEventID,
			paginationToken,
			nDCDefaultPageSize,
			convert.IntPtr(r.shard.GetShardID()),
		)
		if err != nil {
			return nil, nil, err
		}

		var paginateItems []interface{}
		for _, history := range historyBatches {
			paginateItems = append(paginateItems, history)
		}
		return paginateItems, token, nil
	}
}
