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

//go:generate mockgen -copyright_file ../../LICENSE -package $GOPACKAGE -source $GOFILE -destination stateBuilder_mock.go

package history

import (
	"time"

	"github.com/pborman/uuid"
	commonpb "go.temporal.io/temporal-proto/common/v1"
	enumspb "go.temporal.io/temporal-proto/enums/v1"
	historypb "go.temporal.io/temporal-proto/history/v1"
	"go.temporal.io/temporal-proto/serviceerror"

	"github.com/temporalio/temporal/common/cache"
	"github.com/temporalio/temporal/common/cluster"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/persistence"
)

type (
	taskGeneratorProvider func(mutableState) mutableStateTaskGenerator

	stateBuilder interface {
		applyEvents(
			namespaceID string,
			requestID string,
			execution commonpb.WorkflowExecution,
			history []*historypb.HistoryEvent,
			newRunHistory []*historypb.HistoryEvent,
			newRunNDC bool,
		) (mutableState, error)
	}

	stateBuilderImpl struct {
		shard           ShardContext
		clusterMetadata cluster.Metadata
		namespaceCache  cache.NamespaceCache
		logger          log.Logger

		mutableState          mutableState
		taskGeneratorProvider taskGeneratorProvider
	}
)

const (
	// ErrMessageHistorySizeZero indicate that history is empty
	ErrMessageHistorySizeZero = "encounter history size being zero"
)

var _ stateBuilder = (*stateBuilderImpl)(nil)

func newStateBuilder(
	shard ShardContext,
	logger log.Logger,
	mutableState mutableState,
	taskGeneratorProvider taskGeneratorProvider,
) *stateBuilderImpl {

	return &stateBuilderImpl{
		shard:                 shard,
		clusterMetadata:       shard.GetService().GetClusterMetadata(),
		namespaceCache:        shard.GetNamespaceCache(),
		logger:                logger,
		mutableState:          mutableState,
		taskGeneratorProvider: taskGeneratorProvider,
	}
}

func (b *stateBuilderImpl) applyEvents(
	namespaceID string,
	requestID string,
	execution commonpb.WorkflowExecution,
	history []*historypb.HistoryEvent,
	newRunHistory []*historypb.HistoryEvent,
	newRunNDC bool,
) (mutableState, error) {

	if len(history) == 0 {
		return nil, serviceerror.NewInternal(ErrMessageHistorySizeZero)
	}
	firstEvent := history[0]
	lastEvent := history[len(history)-1]
	var newRunMutableStateBuilder mutableState

	taskGenerator := b.taskGeneratorProvider(b.mutableState)

	// need to clear the stickiness since workflow turned to passive
	b.mutableState.ClearStickyness()

	for _, event := range history {
		// NOTE: stateBuilder is also being used in the active side
		if b.mutableState.GetReplicationState() != nil {
			// this function must be called within the for loop, in case
			// history event version changed during for loop
			b.mutableState.UpdateReplicationStateVersion(event.GetVersion(), true)
			b.mutableState.UpdateReplicationStateLastEventID(lastEvent.GetVersion(), lastEvent.GetEventId())
		} else if b.mutableState.GetVersionHistories() != nil {
			if err := b.mutableState.UpdateCurrentVersion(event.GetVersion(), true); err != nil {
				return nil, err
			}
			versionHistories := b.mutableState.GetVersionHistories()
			versionHistory, err := versionHistories.GetCurrentVersionHistory()
			if err != nil {
				return nil, err
			}
			if err := versionHistory.AddOrUpdateItem(persistence.NewVersionHistoryItem(
				event.GetEventId(),
				event.GetVersion(),
			)); err != nil {
				return nil, err
			}
		}
		b.mutableState.GetExecutionInfo().LastEventTaskID = event.GetTaskId()

		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			attributes := event.GetWorkflowExecutionStartedEventAttributes()
			var parentNamespaceID string
			if attributes.GetParentWorkflowNamespace() != "" {
				parentNamespaceEntry, err := b.namespaceCache.GetNamespace(
					attributes.GetParentWorkflowNamespace(),
				)
				if err != nil {
					return nil, err
				}
				parentNamespaceID = parentNamespaceEntry.GetInfo().Id
			}

			if err := b.mutableState.ReplicateWorkflowExecutionStartedEvent(
				parentNamespaceID,
				execution,
				requestID,
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateRecordWorkflowStartedTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowStartTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				event,
			); err != nil {
				return nil, err
			}

			if attributes.GetFirstDecisionTaskBackoffSeconds() > 0 {
				if err := taskGenerator.generateDelayedDecisionTasks(
					b.unixNanoToTime(event.GetTimestamp()),
					event,
				); err != nil {
					return nil, err
				}
			}

			if err := b.mutableState.SetHistoryTree(
				execution.GetRunId(),
			); err != nil {
				return nil, err
			}

			// TODO remove after NDC is fully migrated
			if b.mutableState.GetReplicationState() != nil {
				b.mutableState.GetReplicationState().StartVersion = event.GetVersion()
			}

		case enumspb.EVENT_TYPE_DECISION_TASK_SCHEDULED:
			attributes := event.GetDecisionTaskScheduledEventAttributes()
			// use event.GetTimestamp() as DecisionOriginalScheduledTimestamp, because the heartbeat is not happening here.
			decision, err := b.mutableState.ReplicateDecisionTaskScheduledEvent(
				event.GetVersion(),
				event.GetEventId(),
				attributes.TaskList.GetName(),
				attributes.GetStartToCloseTimeoutSeconds(),
				attributes.GetAttempt(),
				event.GetTimestamp(),
				event.GetTimestamp(),
			)
			if err != nil {
				return nil, err
			}

			// since we do not use stickiness on the standby side
			// there shall be no decision schedule to start timeout
			// NOTE: at the beginning of the loop, stickyness is cleared
			if err := taskGenerator.generateDecisionScheduleTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				decision.ScheduleID,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_DECISION_TASK_STARTED:
			attributes := event.GetDecisionTaskStartedEventAttributes()
			decision, err := b.mutableState.ReplicateDecisionTaskStartedEvent(
				nil,
				event.GetVersion(),
				attributes.GetScheduledEventId(),
				event.GetEventId(),
				attributes.GetRequestId(),
				event.GetTimestamp(),
			)
			if err != nil {
				return nil, err
			}

			if err := taskGenerator.generateDecisionStartTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				decision.ScheduleID,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_DECISION_TASK_COMPLETED:
			if err := b.mutableState.ReplicateDecisionTaskCompletedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_DECISION_TASK_TIMED_OUT:
			if err := b.mutableState.ReplicateDecisionTaskTimedOutEvent(
				event.GetDecisionTaskTimedOutEventAttributes().GetTimeoutType(),
			); err != nil {
				return nil, err
			}

			// this is for transient decision
			decision, err := b.mutableState.ReplicateTransientDecisionTaskScheduled()
			if err != nil {
				return nil, err
			}

			if decision != nil {
				// since we do not use stickiness on the standby side
				// there shall be no decision schedule to start timeout
				// NOTE: at the beginning of the loop, stickyness is cleared
				if err := taskGenerator.generateDecisionScheduleTasks(
					b.unixNanoToTime(event.GetTimestamp()),
					decision.ScheduleID,
				); err != nil {
					return nil, err
				}
			}

		case enumspb.EVENT_TYPE_DECISION_TASK_FAILED:
			if err := b.mutableState.ReplicateDecisionTaskFailedEvent(); err != nil {
				return nil, err
			}

			// this is for transient decision
			decision, err := b.mutableState.ReplicateTransientDecisionTaskScheduled()
			if err != nil {
				return nil, err
			}

			if decision != nil {
				// since we do not use stickiness on the standby side
				// there shall be no decision schedule to start timeout
				// NOTE: at the beginning of the loop, stickyness is cleared
				if err := taskGenerator.generateDecisionScheduleTasks(
					b.unixNanoToTime(event.GetTimestamp()),
					decision.ScheduleID,
				); err != nil {
					return nil, err
				}
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			if _, err := b.mutableState.ReplicateActivityTaskScheduledEvent(
				firstEvent.GetEventId(),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateActivityTransferTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
			if err := b.mutableState.ReplicateActivityTaskStartedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			if err := b.mutableState.ReplicateActivityTaskCompletedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			if err := b.mutableState.ReplicateActivityTaskFailedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
			if err := b.mutableState.ReplicateActivityTaskTimedOutEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_CANCEL_REQUESTED:
			if err := b.mutableState.ReplicateActivityTaskCancelRequestedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_ACTIVITY_TASK_CANCELED:
			if err := b.mutableState.ReplicateActivityTaskCanceledEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_REQUEST_CANCEL_ACTIVITY_TASK_FAILED:
			// No mutable state action is needed

		case enumspb.EVENT_TYPE_TIMER_STARTED:
			if _, err := b.mutableState.ReplicateTimerStartedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_TIMER_FIRED:
			if err := b.mutableState.ReplicateTimerFiredEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_TIMER_CANCELED:
			if err := b.mutableState.ReplicateTimerCanceledEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CANCEL_TIMER_FAILED:
			// no mutable state action is needed

		case enumspb.EVENT_TYPE_START_CHILD_WORKFLOW_EXECUTION_INITIATED:
			if _, err := b.mutableState.ReplicateStartChildWorkflowExecutionInitiatedEvent(
				firstEvent.GetEventId(),
				event,
				// create a new request ID which is used by transfer queue processor
				// if namespace is failed over at this point
				uuid.New(),
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateChildWorkflowTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_START_CHILD_WORKFLOW_EXECUTION_FAILED:
			if err := b.mutableState.ReplicateStartChildWorkflowExecutionFailedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_STARTED:
			if err := b.mutableState.ReplicateChildWorkflowExecutionStartedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_COMPLETED:
			if err := b.mutableState.ReplicateChildWorkflowExecutionCompletedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_FAILED:
			if err := b.mutableState.ReplicateChildWorkflowExecutionFailedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_CANCELED:
			if err := b.mutableState.ReplicateChildWorkflowExecutionCanceledEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_TIMED_OUT:
			if err := b.mutableState.ReplicateChildWorkflowExecutionTimedOutEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_CHILD_WORKFLOW_EXECUTION_TERMINATED:
			if err := b.mutableState.ReplicateChildWorkflowExecutionTerminatedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_REQUEST_CANCEL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED:
			if _, err := b.mutableState.ReplicateRequestCancelExternalWorkflowExecutionInitiatedEvent(
				firstEvent.GetEventId(),
				event,
				// create a new request ID which is used by transfer queue processor
				// if namespace is failed over at this point
				uuid.New(),
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateRequestCancelExternalTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_REQUEST_CANCEL_EXTERNAL_WORKFLOW_EXECUTION_FAILED:
			if err := b.mutableState.ReplicateRequestCancelExternalWorkflowExecutionFailedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_EXTERNAL_WORKFLOW_EXECUTION_CANCEL_REQUESTED:
			if err := b.mutableState.ReplicateExternalWorkflowExecutionCancelRequested(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_SIGNAL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED:
			// Create a new request ID which is used by transfer queue processor if namespace is failed over at this point
			signalRequestID := uuid.New()
			if _, err := b.mutableState.ReplicateSignalExternalWorkflowExecutionInitiatedEvent(
				firstEvent.GetEventId(),
				event,
				signalRequestID,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateSignalExternalTasks(
				b.unixNanoToTime(event.GetTimestamp()),
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_SIGNAL_EXTERNAL_WORKFLOW_EXECUTION_FAILED:
			if err := b.mutableState.ReplicateSignalExternalWorkflowExecutionFailedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_EXTERNAL_WORKFLOW_EXECUTION_SIGNALED:
			if err := b.mutableState.ReplicateExternalWorkflowExecutionSignaled(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_MARKER_RECORDED:
			// No mutable state action is needed

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
			if err := b.mutableState.ReplicateWorkflowExecutionSignaled(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCEL_REQUESTED:
			if err := b.mutableState.ReplicateWorkflowExecutionCancelRequestedEvent(
				event,
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES:
			b.mutableState.ReplicateUpsertWorkflowSearchAttributesEvent(event)
			if err := taskGenerator.generateWorkflowSearchAttrTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
			if err := b.mutableState.ReplicateWorkflowExecutionCompletedEvent(
				firstEvent.GetEventId(),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowCloseTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED:
			if err := b.mutableState.ReplicateWorkflowExecutionFailedEvent(
				firstEvent.GetEventId(),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowCloseTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
			if err := b.mutableState.ReplicateWorkflowExecutionTimedoutEvent(
				firstEvent.GetEventId(),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowCloseTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED:
			if err := b.mutableState.ReplicateWorkflowExecutionCanceledEvent(
				firstEvent.GetEventId(),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowCloseTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
			if err := b.mutableState.ReplicateWorkflowExecutionTerminatedEvent(
				firstEvent.GetEventId(),
				event,
			); err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowCloseTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW:

			// The length of newRunHistory can be zero in resend case
			if len(newRunHistory) != 0 {
				if newRunNDC {
					newRunMutableStateBuilder = newMutableStateBuilderWithVersionHistories(
						b.shard,
						b.shard.GetEventsCache(),
						b.logger,
						b.mutableState.GetNamespaceEntry(),
					)
				} else {
					newRunMutableStateBuilder = newMutableStateBuilderWithReplicationState(
						b.shard,
						b.shard.GetEventsCache(),
						b.logger,
						b.mutableState.GetNamespaceEntry(),
					)
				}
				newRunStateBuilder := newStateBuilder(b.shard, b.logger, newRunMutableStateBuilder, b.taskGeneratorProvider)

				newRunID := event.GetWorkflowExecutionContinuedAsNewEventAttributes().GetNewExecutionRunId()
				newExecution := commonpb.WorkflowExecution{
					WorkflowId: execution.WorkflowId,
					RunId:      newRunID,
				}
				_, err := newRunStateBuilder.applyEvents(
					namespaceID,
					uuid.New(),
					newExecution,
					newRunHistory,
					nil,
					false,
				)
				if err != nil {
					return nil, err
				}
			}

			err := b.mutableState.ReplicateWorkflowExecutionContinuedAsNewEvent(
				firstEvent.GetEventId(),
				namespaceID,
				event,
			)
			if err != nil {
				return nil, err
			}

			if err := taskGenerator.generateWorkflowCloseTasks(
				b.unixNanoToTime(event.GetTimestamp()),
			); err != nil {
				return nil, err
			}

		default:
			return nil, serviceerror.NewInvalidArgument("Unknown event type: %v").MessageArgs(event.GetEventType())
		}
	}

	// must generate the activity timer / user timer at the very end
	if err := taskGenerator.generateActivityTimerTasks(
		b.unixNanoToTime(lastEvent.GetTimestamp()),
	); err != nil {
		return nil, err
	}
	if err := taskGenerator.generateUserTimerTasks(
		b.unixNanoToTime(lastEvent.GetTimestamp()),
	); err != nil {
		return nil, err
	}

	b.mutableState.GetExecutionInfo().SetLastFirstEventID(firstEvent.GetEventId())
	b.mutableState.GetExecutionInfo().SetNextEventID(lastEvent.GetEventId() + 1)

	b.mutableState.SetHistoryBuilder(newHistoryBuilderFromEvents(history, b.logger))

	return newRunMutableStateBuilder, nil
}

func (b *stateBuilderImpl) unixNanoToTime(
	unixNano int64,
) time.Time {

	return time.Unix(0, unixNano)
}
