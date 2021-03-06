// Copyright (c) 2017 Uber Technologies, Inc.
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

package history

import (
	"os"
	"testing"
	"time"

	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-common/bark"
	"github.com/uber-go/tally"
	"github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/messaging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/common/service/dynamicconfig"
)

type (
	timerQueueStandbyProcessorSuite struct {
		suite.Suite

		mockShardManager *mocks.ShardManager
		logger           bark.Logger

		mockHistoryEngine    *historyEngineImpl
		mockMetadataMgr      *mocks.MetadataManager
		mockVisibilityMgr    *mocks.VisibilityManager
		mockExecutionMgr     *mocks.ExecutionManager
		mockHistoryMgr       *mocks.HistoryManager
		mockShard            ShardContext
		mockClusterMetadata  *mocks.ClusterMetadata
		mockProducer         *mocks.KafkaProducer
		mockMessagingClient  messaging.Client
		mocktimerQueueAckMgr *MockTimerQueueAckMgr
		mockService          service.Service
		clusterName          string

		timerQueueStandbyProcessor *timerQueueStandbyProcessorImpl
	}
)

func TestTimerQueueStandbyProcessorSuite(t *testing.T) {
	s := new(timerQueueStandbyProcessorSuite)
	suite.Run(t, s)
}

func (s *timerQueueStandbyProcessorSuite) SetupSuite() {
	if testing.Verbose() {
		log.SetOutput(os.Stdout)
	}
}

func (s *timerQueueStandbyProcessorSuite) SetupTest() {
	shardID := 0
	log2 := log.New()
	log2.Level = log.DebugLevel
	s.logger = bark.NewLoggerFromLogrus(log2)
	s.mockShardManager = &mocks.ShardManager{}
	s.mockExecutionMgr = &mocks.ExecutionManager{}
	s.mockHistoryMgr = &mocks.HistoryManager{}
	s.mockVisibilityMgr = &mocks.VisibilityManager{}
	s.mockMetadataMgr = &mocks.MetadataManager{}
	s.mockClusterMetadata = &mocks.ClusterMetadata{}
	// ack manager will use the domain information
	s.mockMetadataMgr.On("GetDomain", mock.Anything).Return(&persistence.GetDomainResponse{
		// only thing used is the replication config
		Config:         &persistence.DomainConfig{Retention: 1},
		IsGlobalDomain: true,
		ReplicationConfig: &persistence.DomainReplicationConfig{
			ActiveClusterName: cluster.TestAlternativeClusterName,
			// Clusters attr is not used.
		},
	}, nil).Once()
	s.mockClusterMetadata.On("GetCurrentClusterName").Return(cluster.TestCurrentClusterName)
	s.mockProducer = &mocks.KafkaProducer{}
	metricsClient := metrics.NewClient(tally.NoopScope, metrics.History)
	s.mockMessagingClient = mocks.NewMockMessagingClient(s.mockProducer, nil)
	s.mockService = service.NewTestService(s.mockClusterMetadata, s.mockMessagingClient, metricsClient, s.logger)

	s.mockShard = &shardContextImpl{
		service:                   s.mockService,
		shardInfo:                 &persistence.ShardInfo{ShardID: shardID, RangeID: 1, TransferAckLevel: 0},
		transferSequenceNumber:    1,
		executionManager:          s.mockExecutionMgr,
		shardManager:              s.mockShardManager,
		historyMgr:                s.mockHistoryMgr,
		maxTransferSequenceNumber: 100000,
		closeCh:                   make(chan int, 100),
		config:                    NewConfig(dynamicconfig.NewNopCollection(), 1),
		logger:                    s.logger,
		domainCache:               cache.NewDomainCache(s.mockMetadataMgr, s.mockClusterMetadata, s.logger),
		metricsClient:             metrics.NewClient(tally.NoopScope, metrics.History),
	}

	historyCache := newHistoryCache(s.mockShard, s.logger)
	h := &historyEngineImpl{
		currentclusterName: s.mockShard.GetService().GetClusterMetadata().GetCurrentClusterName(),
		shard:              s.mockShard,
		historyMgr:         s.mockHistoryMgr,
		executionManager:   s.mockExecutionMgr,
		historyCache:       historyCache,
		logger:             s.logger,
		tokenSerializer:    common.NewJSONTaskTokenSerializer(),
		hSerializerFactory: persistence.NewHistorySerializerFactory(),
		metricsClient:      s.mockShard.GetMetricsClient(),
	}
	s.mockHistoryEngine = h
	s.clusterName = cluster.TestAlternativeClusterName
	s.timerQueueStandbyProcessor = newTimerQueueStandbyProcessor(s.mockShard, h, s.clusterName, s.logger)
	s.mocktimerQueueAckMgr = &MockTimerQueueAckMgr{}
	s.timerQueueStandbyProcessor.timerQueueAckMgr = s.mocktimerQueueAckMgr
}

func (s *timerQueueStandbyProcessorSuite) TearDownTest() {
	s.mockShardManager.AssertExpectations(s.T())
	s.mockExecutionMgr.AssertExpectations(s.T())
	s.mockHistoryMgr.AssertExpectations(s.T())
	s.mockVisibilityMgr.AssertExpectations(s.T())
	s.mockClusterMetadata.AssertExpectations(s.T())
	s.mockProducer.AssertExpectations(s.T())
	s.mocktimerQueueAckMgr.AssertExpectations(s.T())
}

func (s *timerQueueStandbyProcessorSuite) TestProcessExpiredUserTimer_Pending() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID := "timer"
	timerTimeout := 2 * time.Second
	_, timerInfo := addTimerStartedEvent(msBuilder, event.GetEventId(), timerID, int64(timerTimeout.Seconds()))

	tBuilder := newTimerBuilder(s.mockShard.GetConfig(), s.logger, common.NewRealTimeSource())
	tBuilder.AddUserTimer(timerInfo, msBuilder)
	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeUserTimer, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: tBuilder.GetUserTimerTaskIfNeeded(msBuilder).(*persistence.UserTimerTask).GetVisibilityTimestamp(),
		EventID:             di.ScheduleID,
	}

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil)

	s.Equal(ErrTaskRetry, s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessExpiredUserTimer_Success() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID := "timer"
	timerTimeout := 2 * time.Second
	event, timerInfo := addTimerStartedEvent(msBuilder, event.GetEventId(), timerID, int64(timerTimeout.Seconds()))

	tBuilder := newTimerBuilder(s.mockShard.GetConfig(), s.logger, common.NewRealTimeSource())
	tBuilder.AddUserTimer(timerInfo, msBuilder)
	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeUserTimer, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: tBuilder.GetUserTimerTaskIfNeeded(msBuilder).(*persistence.UserTimerTask).GetVisibilityTimestamp(),
		EventID:             di.ScheduleID,
	}

	addTimerFiredEvent(msBuilder, event.GetEventId(), timerID)

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mocktimerQueueAckMgr.On("completeTimerTask", timerTask).Return(nil).Once()

	s.Nil(s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessExpiredUserTimer_Multiple() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID1 := "timer-1"
	timerTimeout1 := 2 * time.Second
	timerEvent1, timerInfo1 := addTimerStartedEvent(msBuilder, event.GetEventId(), timerID1, int64(timerTimeout1.Seconds()))

	timerID2 := "timer-2"
	timerTimeout2 := 50 * time.Second
	_, timerInfo2 := addTimerStartedEvent(msBuilder, event.GetEventId(), timerID2, int64(timerTimeout2.Seconds()))

	tBuilder := newTimerBuilder(s.mockShard.GetConfig(), s.logger, common.NewRealTimeSource())
	tBuilder.AddUserTimer(timerInfo1, msBuilder)
	tBuilder.AddUserTimer(timerInfo2, msBuilder)

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeUserTimer, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: tBuilder.GetUserTimerTaskIfNeeded(msBuilder).(*persistence.UserTimerTask).GetVisibilityTimestamp(),
		EventID:             di.ScheduleID,
	}

	addTimerFiredEvent(msBuilder, timerEvent1.GetEventId(), timerID1)

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mocktimerQueueAckMgr.On("completeTimerTask", timerTask).Return(nil).Once()

	s.Nil(s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessActivityTimeout_Pending() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	addActivityTaskScheduledEvent(msBuilder, event.GetEventId(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()))

	tBuilder := newTimerBuilder(s.mockShard.GetConfig(), s.logger, common.NewRealTimeSource())

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeActivityTimeout, TimeoutType: int(workflow.TimeoutTypeScheduleToClose),
		VisibilityTimestamp: tBuilder.GetActivityTimerTaskIfNeeded(msBuilder).(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp(),
		EventID:             di.ScheduleID,
	}

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.Equal(ErrTaskRetry, s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessActivityTimeout_Success() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	scheduleEvent, timerInfo := addActivityTaskScheduledEvent(msBuilder, event.GetEventId(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()))
	startedEvent := addActivityTaskStartedEvent(msBuilder, scheduleEvent.GetEventId(), tasklist, identity)

	tBuilder := newTimerBuilder(s.mockShard.GetConfig(), s.logger, common.NewRealTimeSource())
	tBuilder.AddStartToCloseActivityTimeout(timerInfo)

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeActivityTimeout, TimeoutType: int(workflow.TimeoutTypeScheduleToClose),
		VisibilityTimestamp: tBuilder.GetActivityTimerTaskIfNeeded(msBuilder).(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp(),
		EventID:             di.ScheduleID,
	}

	addActivityTaskCompletedEvent(msBuilder, scheduleEvent.GetEventId(), startedEvent.GetEventId(), []byte(nil), identity)

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mocktimerQueueAckMgr.On("completeTimerTask", timerTask).Return(nil).Once()

	s.Nil(s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessActivityTimeout_Multiple() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID1 := "activity 1"
	activityType1 := "activity type 1"
	timerTimeout1 := 2 * time.Second
	scheduleEvent1, timerInfo1 := addActivityTaskScheduledEvent(msBuilder, event.GetEventId(), activityID1, activityType1, tasklist, []byte(nil),
		int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()))
	startedEvent1 := addActivityTaskStartedEvent(msBuilder, scheduleEvent1.GetEventId(), tasklist, identity)

	activityID2 := "activity 2"
	activityType2 := "activity type 2"
	timerTimeout2 := 20 * time.Second
	_, timerInfo2 := addActivityTaskScheduledEvent(msBuilder, event.GetEventId(), activityID2, activityType2, tasklist, []byte(nil),
		int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()))

	tBuilder := newTimerBuilder(s.mockShard.GetConfig(), s.logger, common.NewRealTimeSource())
	tBuilder.AddStartToCloseActivityTimeout(timerInfo1)
	tBuilder.AddScheduleToCloseActivityTimeout(timerInfo2)

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeActivityTimeout, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: tBuilder.GetActivityTimerTaskIfNeeded(msBuilder).(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp(),
		EventID:             di.ScheduleID,
	}

	addActivityTaskCompletedEvent(msBuilder, scheduleEvent1.GetEventId(), startedEvent1.GetEventId(), []byte(nil), identity)

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mocktimerQueueAckMgr.On("completeTimerTask", timerTask).Return(nil).Once()

	s.Nil(s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessDecisionTimeout_Pending() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeDecisionTimeout, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: time.Now(),
		EventID:             di.ScheduleID,
	}

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.Equal(ErrTaskRetry, s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessDecisionTimeout_Success() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeDecisionTimeout, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: time.Now(),
		EventID:             di.ScheduleID,
	}

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mocktimerQueueAckMgr.On("completeTimerTask", timerTask).Return(nil).Once()

	s.Nil(s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessWorkflowTimeout_Pending() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeWorkflowTimeout, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: time.Now(),
	}

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.Equal(ErrTaskRetry, s.timerQueueStandbyProcessor.process(timerTask))
}

func (s *timerQueueStandbyProcessorSuite) TestProcessWorkflowTimeout_Success() {
	domainID := "some random domain ID"
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("some random workflow ID"),
		RunId:      common.StringPtr(uuid.New()),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	msBuilder := newMutableStateBuilder(s.mockShard.GetConfig(), s.logger)
	msBuilder.AddWorkflowExecutionStartedEvent(
		execution,
		&history.StartWorkflowExecutionRequest{
			DomainUUID: common.StringPtr(domainID),
			StartRequest: &workflow.StartWorkflowExecutionRequest{
				WorkflowType: &workflow.WorkflowType{Name: common.StringPtr(workflowType)},
				TaskList:     &workflow.TaskList{Name: common.StringPtr(taskListName)},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)

	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, nil, "some random identity")
	addCompleteWorkflowEvent(msBuilder, event.GetEventId(), nil)

	timerTask := &persistence.TimerTaskInfo{
		DomainID:   domainID,
		WorkflowID: execution.GetWorkflowId(),
		RunID:      execution.GetRunId(),
		TaskID:     int64(100),
		TaskType:   persistence.TaskTypeWorkflowTimeout, TimeoutType: int(workflow.TimeoutTypeStartToClose),
		VisibilityTimestamp: time.Now(),
	}

	persistenceMutableState := createMutableState(msBuilder)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mocktimerQueueAckMgr.On("completeTimerTask", timerTask).Return(nil).Once()

	s.Nil(s.timerQueueStandbyProcessor.process(timerTask))
}
