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

package host

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/temporal"
	commonpb "go.temporal.io/temporal-proto/common/v1"
	decisionpb "go.temporal.io/temporal-proto/decision/v1"
	enumspb "go.temporal.io/temporal-proto/enums/v1"
	failurepb "go.temporal.io/temporal-proto/failure/v1"
	historypb "go.temporal.io/temporal-proto/history/v1"
	querypb "go.temporal.io/temporal-proto/query/v1"
	tasklistpb "go.temporal.io/temporal-proto/tasklist/v1"
	"go.temporal.io/temporal-proto/workflowservice/v1"

	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/payloads"
	"github.com/temporalio/temporal/service/history"
	"github.com/temporalio/temporal/service/matching"
)

type (
	decisionTaskHandler func(execution *commonpb.WorkflowExecution, wt *commonpb.WorkflowType,
		previousStartedEventID, startedEventID int64, history *historypb.History) ([]*decisionpb.Decision, error)
	activityTaskHandler func(execution *commonpb.WorkflowExecution, activityType *commonpb.ActivityType,
		activityID string, input *commonpb.Payloads, takeToken []byte) (*commonpb.Payloads, bool, error)

	queryHandler func(task *workflowservice.PollForDecisionTaskResponse) (*commonpb.Payloads, error)

	// TaskPoller is used in integration tests to poll decision or activity tasks
	TaskPoller struct {
		Engine                              FrontendClient
		Namespace                           string
		TaskList                            *tasklistpb.TaskList
		StickyTaskList                      *tasklistpb.TaskList
		StickyScheduleToStartTimeoutSeconds int32
		Identity                            string
		DecisionHandler                     decisionTaskHandler
		ActivityHandler                     activityTaskHandler
		QueryHandler                        queryHandler
		Logger                              log.Logger
		T                                   *testing.T
	}
)

// PollAndProcessDecisionTask for decision tasks
func (p *TaskPoller) PollAndProcessDecisionTask(dumpHistory bool, dropTask bool) (isQueryTask bool, err error) {
	return p.PollAndProcessDecisionTaskWithAttempt(dumpHistory, dropTask, false, false, int64(0))
}

// PollAndProcessDecisionTaskWithSticky for decision tasks
func (p *TaskPoller) PollAndProcessDecisionTaskWithSticky(dumpHistory bool, dropTask bool) (isQueryTask bool, err error) {
	return p.PollAndProcessDecisionTaskWithAttempt(dumpHistory, dropTask, true, true, int64(0))
}

// PollAndProcessDecisionTaskWithoutRetry for decision tasks
func (p *TaskPoller) PollAndProcessDecisionTaskWithoutRetry(dumpHistory bool, dropTask bool) (isQueryTask bool, err error) {
	return p.PollAndProcessDecisionTaskWithAttemptAndRetry(dumpHistory, dropTask, false, false, int64(0), 1)
}

// PollAndProcessDecisionTaskWithAttempt for decision tasks
func (p *TaskPoller) PollAndProcessDecisionTaskWithAttempt(
	dumpHistory bool,
	dropTask bool,
	pollStickyTaskList bool,
	respondStickyTaskList bool,
	decisionAttempt int64,
) (isQueryTask bool, err error) {

	return p.PollAndProcessDecisionTaskWithAttemptAndRetry(
		dumpHistory,
		dropTask,
		pollStickyTaskList,
		respondStickyTaskList,
		decisionAttempt,
		5)
}

// PollAndProcessDecisionTaskWithAttemptAndRetry for decision tasks
func (p *TaskPoller) PollAndProcessDecisionTaskWithAttemptAndRetry(
	dumpHistory bool,
	dropTask bool,
	pollStickyTaskList bool,
	respondStickyTaskList bool,
	decisionAttempt int64,
	retryCount int,
) (isQueryTask bool, err error) {

	isQueryTask, _, err = p.PollAndProcessDecisionTaskWithAttemptAndRetryAndForceNewDecision(
		dumpHistory,
		dropTask,
		pollStickyTaskList,
		respondStickyTaskList,
		decisionAttempt,
		retryCount,
		false,
		nil)
	return isQueryTask, err
}

// PollAndProcessDecisionTaskWithAttemptAndRetryAndForceNewDecision for decision tasks
func (p *TaskPoller) PollAndProcessDecisionTaskWithAttemptAndRetryAndForceNewDecision(
	dumpHistory bool,
	dropTask bool,
	pollStickyTaskList bool,
	respondStickyTaskList bool,
	decisionAttempt int64,
	retryCount int,
	forceCreateNewDecision bool,
	queryResult *querypb.WorkflowQueryResult,
) (isQueryTask bool, newTask *workflowservice.RespondDecisionTaskCompletedResponse, err error) {
Loop:
	for attempt := 0; attempt < retryCount; attempt++ {

		taskList := p.TaskList
		if pollStickyTaskList {
			taskList = p.StickyTaskList
		}
		response, err1 := p.Engine.PollForDecisionTask(NewContext(), &workflowservice.PollForDecisionTaskRequest{
			Namespace: p.Namespace,
			TaskList:  taskList,
			Identity:  p.Identity,
		})

		if err1 == history.ErrDuplicate {
			p.Logger.Info("Duplicate Decision task: Polling again")
			continue Loop
		}

		if err1 != nil {
			return false, nil, err1
		}

		if response == nil || len(response.TaskToken) == 0 {
			p.Logger.Info("Empty Decision task: Polling again")
			continue Loop
		}

		var events []*historypb.HistoryEvent
		if response.Query == nil || !pollStickyTaskList {
			// if not query task, should have some history events
			// for non sticky query, there should be events returned
			history := response.History
			if history == nil {
				p.Logger.Fatal("History is nil")
			}

			events = history.Events
			if events == nil || len(events) == 0 {
				p.Logger.Fatal("History Events are empty")
			}

			nextPageToken := response.NextPageToken
			for nextPageToken != nil {
				resp, err2 := p.Engine.GetWorkflowExecutionHistory(NewContext(), &workflowservice.GetWorkflowExecutionHistoryRequest{
					Namespace:     p.Namespace,
					Execution:     response.WorkflowExecution,
					NextPageToken: nextPageToken,
				})

				if err2 != nil {
					return false, nil, err2
				}

				events = append(events, resp.History.Events...)
				nextPageToken = resp.NextPageToken
			}
		} else {
			// for sticky query, there should be NO events returned
			// since worker side already has the state machine and we do not intend to update that.
			history := response.History
			nextPageToken := response.NextPageToken
			if !(history == nil || (len(history.Events) == 0 && nextPageToken == nil)) {
				// if history is not nil, and contains events or next token
				p.Logger.Fatal("History is not empty for sticky query")
			}
		}

		if dropTask {
			p.Logger.Info("Dropping Decision task: ")
			return false, nil, nil
		}

		if dumpHistory {
			common.PrettyPrintHistory(response.History, p.Logger)
		}

		// handle query task response
		if response.Query != nil {
			blob, err := p.QueryHandler(response)

			completeRequest := &workflowservice.RespondQueryTaskCompletedRequest{TaskToken: response.TaskToken}
			if err != nil {
				completeType := enumspb.QUERY_RESULT_TYPE_FAILED
				completeRequest.CompletedType = completeType
				completeRequest.ErrorMessage = err.Error()
			} else {
				completeType := enumspb.QUERY_RESULT_TYPE_ANSWERED
				completeRequest.CompletedType = completeType
				completeRequest.QueryResult = blob
			}

			_, err = p.Engine.RespondQueryTaskCompleted(NewContext(), completeRequest)
			return true, nil, err
		}

		// handle normal decision task / non query task response
		var lastDecisionScheduleEvent *historypb.HistoryEvent
		for _, e := range events {
			if e.GetEventType() == enumspb.EVENT_TYPE_DECISION_TASK_SCHEDULED {
				lastDecisionScheduleEvent = e
			}
		}
		if lastDecisionScheduleEvent != nil && decisionAttempt > 0 {
			require.Equal(p.T, decisionAttempt, lastDecisionScheduleEvent.GetDecisionTaskScheduledEventAttributes().GetAttempt())
		}

		decisions, err := p.DecisionHandler(response.WorkflowExecution, response.WorkflowType, response.PreviousStartedEventId, response.StartedEventId, response.History)
		if err != nil {
			p.Logger.Error("Failing Decision. Decision handler failed with error", tag.Error(err))
			_, err = p.Engine.RespondDecisionTaskFailed(NewContext(), &workflowservice.RespondDecisionTaskFailedRequest{
				TaskToken: response.TaskToken,
				Cause:     enumspb.DECISION_TASK_FAILED_CAUSE_WORKFLOW_WORKER_UNHANDLED_FAILURE,
				Failure:   newApplicationFailure(err, false, nil),
				Identity:  p.Identity,
			})
			return isQueryTask, nil, err
		}

		p.Logger.Info("Completing Decision.  Decisions", tag.Value(decisions))
		if !respondStickyTaskList {
			// non sticky tasklist
			newTask, err := p.Engine.RespondDecisionTaskCompleted(NewContext(), &workflowservice.RespondDecisionTaskCompletedRequest{
				TaskToken:                  response.TaskToken,
				Identity:                   p.Identity,
				Decisions:                  decisions,
				ReturnNewDecisionTask:      forceCreateNewDecision,
				ForceCreateNewDecisionTask: forceCreateNewDecision,
				QueryResults:               getQueryResults(response.GetQueries(), queryResult),
			})
			return false, newTask, err
		}
		// sticky tasklist
		newTask, err := p.Engine.RespondDecisionTaskCompleted(
			NewContext(),
			&workflowservice.RespondDecisionTaskCompletedRequest{
				TaskToken: response.TaskToken,
				Identity:  p.Identity,
				Decisions: decisions,
				StickyAttributes: &tasklistpb.StickyExecutionAttributes{
					WorkerTaskList:                p.StickyTaskList,
					ScheduleToStartTimeoutSeconds: p.StickyScheduleToStartTimeoutSeconds,
				},
				ReturnNewDecisionTask:      forceCreateNewDecision,
				ForceCreateNewDecisionTask: forceCreateNewDecision,
				QueryResults:               getQueryResults(response.GetQueries(), queryResult),
			},
		)

		return false, newTask, err
	}

	return false, nil, matching.ErrNoTasks
}

// HandlePartialDecision for decision task
func (p *TaskPoller) HandlePartialDecision(response *workflowservice.PollForDecisionTaskResponse) (
	*workflowservice.RespondDecisionTaskCompletedResponse, error) {
	if response == nil || len(response.TaskToken) == 0 {
		p.Logger.Info("Empty Decision task: Polling again")
		return nil, nil
	}

	var events []*historypb.HistoryEvent
	history := response.History
	if history == nil {
		p.Logger.Fatal("History is nil")
	}

	events = history.Events
	if events == nil || len(events) == 0 {
		p.Logger.Fatal("History Events are empty")
	}

	decisions, err := p.DecisionHandler(response.WorkflowExecution, response.WorkflowType,
		response.PreviousStartedEventId, response.StartedEventId, response.History)
	if err != nil {
		p.Logger.Error("Failing Decision. Decision handler failed with error", tag.Error(err))
		_, err = p.Engine.RespondDecisionTaskFailed(NewContext(), &workflowservice.RespondDecisionTaskFailedRequest{
			TaskToken: response.TaskToken,
			Cause:     enumspb.DECISION_TASK_FAILED_CAUSE_WORKFLOW_WORKER_UNHANDLED_FAILURE,
			Failure:   newApplicationFailure(err, false, nil),
			Identity:  p.Identity,
		})
		return nil, err
	}

	p.Logger.Info("Completing Decision", tag.Value(decisions))

	// sticky tasklist
	newTask, err := p.Engine.RespondDecisionTaskCompleted(
		NewContext(),
		&workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: response.TaskToken,
			Identity:  p.Identity,
			Decisions: decisions,
			StickyAttributes: &tasklistpb.StickyExecutionAttributes{
				WorkerTaskList:                p.StickyTaskList,
				ScheduleToStartTimeoutSeconds: p.StickyScheduleToStartTimeoutSeconds,
			},
			ReturnNewDecisionTask:      true,
			ForceCreateNewDecisionTask: true,
		},
	)

	return newTask, err
}

// PollAndProcessActivityTask for activity tasks
func (p *TaskPoller) PollAndProcessActivityTask(dropTask bool) error {
retry:
	for attempt := 0; attempt < 5; attempt++ {
		response, err := p.Engine.PollForActivityTask(NewContext(), &workflowservice.PollForActivityTaskRequest{
			Namespace: p.Namespace,
			TaskList:  p.TaskList,
			Identity:  p.Identity,
		})

		if err == history.ErrDuplicate {
			p.Logger.Info("Duplicate Activity task: Polling again")
			continue retry
		}

		if err != nil {
			return err
		}

		if response == nil || len(response.TaskToken) == 0 {
			p.Logger.Info("Empty Activity task: Polling again")
			return nil
		}

		if dropTask {
			p.Logger.Info("Dropping Activity task: ")
			return nil
		}
		p.Logger.Debug("Received Activity task", tag.Value(response))

		result, cancel, err2 := p.ActivityHandler(response.WorkflowExecution, response.ActivityType, response.ActivityId,
			response.Input, response.TaskToken)
		if cancel {
			p.Logger.Info("Executing RespondActivityTaskCanceled")
			_, err := p.Engine.RespondActivityTaskCanceled(NewContext(), &workflowservice.RespondActivityTaskCanceledRequest{
				TaskToken: response.TaskToken,
				Details:   payloads.EncodeString("details"),
				Identity:  p.Identity,
			})
			return err
		}

		if err2 != nil {
			_, err := p.Engine.RespondActivityTaskFailed(NewContext(), &workflowservice.RespondActivityTaskFailedRequest{
				TaskToken: response.TaskToken,
				Failure:   newApplicationFailure(err2, false, nil),
				Identity:  p.Identity,
			})
			return err
		}

		_, err = p.Engine.RespondActivityTaskCompleted(NewContext(), &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: response.TaskToken,
			Identity:  p.Identity,
			Result:    result,
		})
		return err
	}

	return matching.ErrNoTasks
}

// PollAndProcessActivityTaskWithID is similar to PollAndProcessActivityTask but using RespondActivityTask...ByID
func (p *TaskPoller) PollAndProcessActivityTaskWithID(dropTask bool) error {
retry:
	for attempt := 0; attempt < 5; attempt++ {
		response, err1 := p.Engine.PollForActivityTask(NewContext(), &workflowservice.PollForActivityTaskRequest{
			Namespace: p.Namespace,
			TaskList:  p.TaskList,
			Identity:  p.Identity,
		})

		if err1 == history.ErrDuplicate {
			p.Logger.Info("Duplicate Activity task: Polling again")
			continue retry
		}

		if err1 != nil {
			return err1
		}

		if response == nil || len(response.TaskToken) == 0 {
			p.Logger.Info("Empty Activity task: Polling again")
			return nil
		}

		if response.GetActivityId() == "" {
			p.Logger.Info("Empty ActivityID")
			return nil
		}

		if dropTask {
			p.Logger.Info("Dropping Activity task: ")
			return nil
		}
		p.Logger.Debug("Received Activity task", tag.Value(response))

		result, cancel, err2 := p.ActivityHandler(response.WorkflowExecution, response.ActivityType, response.ActivityId,
			response.Input, response.TaskToken)
		if cancel {
			p.Logger.Info("Executing RespondActivityTaskCanceled")
			_, err := p.Engine.RespondActivityTaskCanceledById(NewContext(), &workflowservice.RespondActivityTaskCanceledByIdRequest{
				Namespace:  p.Namespace,
				WorkflowId: response.WorkflowExecution.GetWorkflowId(),
				RunId:      response.WorkflowExecution.GetRunId(),
				ActivityId: response.GetActivityId(),
				Details:    payloads.EncodeString("details"),
				Identity:   p.Identity,
			})
			return err
		}

		if err2 != nil {
			_, err := p.Engine.RespondActivityTaskFailedById(NewContext(), &workflowservice.RespondActivityTaskFailedByIdRequest{
				Namespace:  p.Namespace,
				WorkflowId: response.WorkflowExecution.GetWorkflowId(),
				RunId:      response.WorkflowExecution.GetRunId(),
				ActivityId: response.GetActivityId(),
				Failure:    newApplicationFailure(err2, false, nil),
				Identity:   p.Identity,
			})
			return err
		}

		_, err := p.Engine.RespondActivityTaskCompletedById(NewContext(), &workflowservice.RespondActivityTaskCompletedByIdRequest{
			Namespace:  p.Namespace,
			WorkflowId: response.WorkflowExecution.GetWorkflowId(),
			RunId:      response.WorkflowExecution.GetRunId(),
			ActivityId: response.GetActivityId(),
			Identity:   p.Identity,
			Result:     result,
		})
		return err
	}

	return matching.ErrNoTasks
}

func getQueryResults(queries map[string]*querypb.WorkflowQuery, queryResult *querypb.WorkflowQueryResult) map[string]*querypb.WorkflowQueryResult {
	result := make(map[string]*querypb.WorkflowQueryResult)
	for k := range queries {
		result[k] = queryResult
	}
	return result
}

func newApplicationFailure(err error, nonRetryable bool, details *commonpb.Payloads) *failurepb.Failure {
	var applicationErr *temporal.ApplicationError
	if errors.As(err, &applicationErr) {
		nonRetryable = applicationErr.NonRetryable()
	}

	f := &failurepb.Failure{
		Message: err.Error(),
		Source:  "IntegrationTests",
		FailureInfo: &failurepb.Failure_ApplicationFailureInfo{ApplicationFailureInfo: &failurepb.ApplicationFailureInfo{
			Type:         getErrorType(err),
			NonRetryable: nonRetryable,
			Details:      details,
		}},
	}

	return f
}

func getErrorType(err error) string {
	var t reflect.Type
	for t = reflect.TypeOf(err); t.Kind() == reflect.Ptr; t = t.Elem() {
	}

	return t.Name()
}
