// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package taskexecutor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/go-units"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/disttask/framework/proto"
	"github.com/pingcap/tidb/pkg/disttask/framework/storage"
	"github.com/pingcap/tidb/pkg/metrics"
	tidbutil "github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/cpu"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/memory"
	"go.uber.org/zap"
)

var (
	// DefaultCheckInterval is the default interval to check whether there are tasks
	// or subtasks to run.
	// exported for testing.
	DefaultCheckInterval = 300 * time.Millisecond
	// MaxCheckInterval is the max interval to check whether there are subtasks to run.
	// exported for testing.
	MaxCheckInterval        = 2 * time.Second
	maxChecksWhenNoSubtask  = 7
	recoverMetaInterval     = 90 * time.Second
	retrySQLTimes           = 30
	retrySQLInterval        = 500 * time.Millisecond
	unfinishedSubtaskStates = []proto.SubtaskState{
		proto.SubtaskStatePending,
		proto.SubtaskStateRunning,
	}
)

// Manager monitors the task table and manages the taskExecutors.
type Manager struct {
	taskTable TaskTable
	mu        struct {
		sync.RWMutex
		// taskID -> TaskExecutor.
		taskExecutors map[int64]TaskExecutor
	}
	// id, it's the same as server id now, i.e. host:port.
	id          string
	wg          tidbutil.WaitGroupWrapper
	executorWG  tidbutil.WaitGroupWrapper
	ctx         context.Context
	cancel      context.CancelFunc
	logger      *zap.Logger
	slotManager *slotManager

	totalCPU int
	totalMem int64
}

// NewManager creates a new task executor Manager.
func NewManager(ctx context.Context, id string, taskTable TaskTable) (*Manager, error) {
	logger := log.L()
	if intest.InTest {
		logger = logger.With(zap.String("server-id", id))
	}
	totalMem, err := memory.MemTotal()
	if err != nil {
		// should not happen normally, as in main function of tidb-server, we assert
		// that memory.MemTotal() will not fail.
		return nil, err
	}
	totalCPU := cpu.GetCPUCount()
	if totalCPU <= 0 || totalMem <= 0 {
		return nil, errors.Errorf("invalid cpu or memory, cpu: %d, memory: %d", totalCPU, totalMem)
	}
	logger.Info("build task executor manager", zap.Int("total-cpu", totalCPU),
		zap.String("total-mem", units.BytesSize(float64(totalMem))))
	m := &Manager{
		id:          id,
		taskTable:   taskTable,
		logger:      logger,
		slotManager: newSlotManager(totalCPU),
		totalCPU:    totalCPU,
		totalMem:    int64(totalMem),
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.mu.taskExecutors = make(map[int64]TaskExecutor)

	return m, nil
}

// InitMeta initializes the meta of the Manager.
// not a must-success step before start manager,
// manager will try to recover meta periodically.
func (m *Manager) InitMeta() (err error) {
	for i := 0; i < retrySQLTimes; i++ {
		err = m.taskTable.InitMeta(m.ctx, m.id, config.GetGlobalConfig().Instance.TiDBServiceScope)
		if err == nil {
			break
		}
		if err1 := m.ctx.Err(); err1 != nil {
			return err1
		}
		if i%10 == 0 {
			m.logger.Warn("start manager failed",
				zap.String("scope", config.GetGlobalConfig().Instance.TiDBServiceScope),
				zap.Int("retry times", i),
				zap.Error(err))
		}
		time.Sleep(retrySQLInterval)
	}
	return err
}

func (m *Manager) recoverMeta() (err error) {
	for i := 0; i < retrySQLTimes; i++ {
		err = m.taskTable.RecoverMeta(m.ctx, m.id, config.GetGlobalConfig().Instance.TiDBServiceScope)
		if err == nil {
			break
		}
		if err1 := m.ctx.Err(); err1 != nil {
			return err1
		}
		if i%10 == 0 {
			m.logger.Warn("recover meta failed",
				zap.String("scope", config.GetGlobalConfig().Instance.TiDBServiceScope),
				zap.Int("retry times", i),
				zap.Error(err))
		}
		time.Sleep(retrySQLInterval)
	}
	return err
}

// Start starts the Manager.
func (m *Manager) Start() error {
	m.logger.Info("task executor manager start")
	m.wg.Run(m.handleTasksLoop)
	m.wg.Run(m.recoverMetaLoop)
	return nil
}

// Cancel cancels the executor manager.
// used in test to simulate tidb node shutdown.
func (m *Manager) Cancel() {
	m.cancel()
}

// Stop stops the Manager.
func (m *Manager) Stop() {
	m.cancel()
	m.executorWG.Wait()
	m.wg.Wait()
}

// handleTasksLoop handle tasks of interested states, including:
//   - pending/running: start the task executor.
//   - reverting: cancel the task executor, and mark running subtasks as Canceled.
//   - pausing: cancel the task executor, mark all pending/running subtasks of current
//     node as paused.
//
// Pausing is handled on every executor to make sure all subtasks are
// NOT running by executor before mark the task as paused.
func (m *Manager) handleTasksLoop() {
	defer tidbutil.Recover(metrics.LabelDomain, "handleTasksLoop", m.handleTasksLoop, false)
	ticker := time.NewTicker(DefaultCheckInterval)
	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("handle tasks loop done")
			return
		case <-ticker.C:
		}

		m.handleTasks()
	}
}

func (m *Manager) handleTasks() {
	tasks, err := m.taskTable.GetTaskExecInfoByExecID(m.ctx, m.id)
	if err != nil {
		m.logErr(err)
		return
	}

	executableTasks := make([]*storage.TaskExecInfo, 0, len(tasks))
	for _, task := range tasks {
		switch task.State {
		case proto.TaskStateRunning, proto.TaskStateReverting:
			if task.State == proto.TaskStateReverting {
				m.cancelRunningSubtaskOf(task.ID)
			}
			// TaskStateReverting require executor to run rollback logic.
			if !m.isExecutorStarted(task.ID) {
				executableTasks = append(executableTasks, task)
			}
		case proto.TaskStatePausing:
			if err := m.handlePausingTask(task.ID); err != nil {
				m.logErr(err)
			}
		}
	}

	if len(executableTasks) > 0 {
		m.handleExecutableTasks(executableTasks)
	}
}

// handleExecutableTasks handles executable tasks.
func (m *Manager) handleExecutableTasks(taskInfos []*storage.TaskExecInfo) {
	for _, task := range taskInfos {
		canAlloc, tasksNeedFree := m.slotManager.canAlloc(task.Task)
		if len(tasksNeedFree) > 0 {
			m.cancelTaskExecutors(tasksNeedFree)
			// do not handle the tasks with lower priority if current task is waiting tasks free.
			break
		}

		if !canAlloc {
			m.logger.Debug("no enough slots to run task", zap.Int64("task-id", task.ID))
			continue
		}
		m.startTaskExecutor(task.Task)
	}
}

// cancelRunningSubtaskOf cancels the running subtask of the task, the subtask
// will switch to `canceled` state.
func (m *Manager) cancelRunningSubtaskOf(taskID int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if executor, ok := m.mu.taskExecutors[taskID]; ok {
		m.logger.Info("onCanceledTasks", zap.Int64("task-id", taskID))
		executor.CancelRunningSubtask()
	}
}

// onPausingTasks pauses/cancels the pending/running subtasks.
func (m *Manager) handlePausingTask(taskID int64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.logger.Info("handle pausing task", zap.Int64("task-id", taskID))
	if executor, ok := m.mu.taskExecutors[taskID]; ok {
		executor.Cancel()
	}
	// we pause subtasks belongs to this exec node even when there's no executor running.
	// as balancer might move subtasks to this node when the executor hasn't started.
	return m.taskTable.PauseSubtasks(m.ctx, m.id, taskID)
}

// recoverMetaLoop recovers dist_framework_meta for the tidb node running the taskExecutor manager.
// This is necessary when the TiDB node experiences a prolonged network partition
// and the scheduler deletes `dist_framework_meta`.
// When the TiDB node recovers from the network partition,
// we need to re-insert the metadata.
func (m *Manager) recoverMetaLoop() {
	defer tidbutil.Recover(metrics.LabelDomain, "recoverMetaLoop", m.recoverMetaLoop, false)
	ticker := time.NewTicker(recoverMetaInterval)
	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("recoverMetaLoop done")
			return
		case <-ticker.C:
			if err := m.recoverMeta(); err != nil {
				m.logErr(err)
				continue
			}
		}
	}
}

// cancelTaskExecutors cancels the task executors.
// unlike cancelRunningSubtaskOf, this function doesn't change subtask state.
func (m *Manager) cancelTaskExecutors(tasks []*proto.Task) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range tasks {
		m.logger.Info("cancelTasks", zap.Int64("task-id", task.ID))
		if executor, ok := m.mu.taskExecutors[task.ID]; ok {
			executor.Cancel()
		}
	}
}

// TestContext only used in tests.
type TestContext struct {
	TestSyncSubtaskRun chan struct{}
	mockDown           atomic.Bool
}

// startTaskExecutor handles a runnable task.
func (m *Manager) startTaskExecutor(task *proto.Task) {
	// runCtx only used in executor.Run, cancel in m.fetchAndFastCancelTasks.
	factory := GetTaskExecutorFactory(task.Type)
	if factory == nil {
		err := errors.Errorf("task type %s not found", task.Type)
		m.logErrAndPersist(err, task.ID, nil)
		return
	}
	executor := factory(m.ctx, m.id, task, m.taskTable)
	err := executor.Init(m.ctx)
	if err != nil {
		m.logErrAndPersist(err, task.ID, executor)
		return
	}
	m.addTaskExecutor(executor)
	m.slotManager.alloc(task)
	resource := m.getStepResource(task.Concurrency)
	m.logger.Info("task executor started", zap.Int64("task-id", task.ID),
		zap.Stringer("type", task.Type), zap.Int("remaining-slots", m.slotManager.availableSlots()))
	m.executorWG.RunWithLog(func() {
		defer func() {
			m.logger.Info("task executor exit", zap.Int64("task-id", task.ID), zap.Stringer("type", task.Type))
			m.slotManager.free(task.ID)
			m.delTaskExecutor(executor)
			executor.Close()
		}()
		executor.Run(resource)
	})
}

func (m *Manager) getStepResource(concurrency int) *proto.StepResource {
	return &proto.StepResource{
		CPU: proto.NewAllocatable(int64(concurrency)),
		// same proportion as CPU
		Mem: proto.NewAllocatable(int64(float64(concurrency) / float64(m.totalCPU) * float64(m.totalMem))),
	}
}

func (m *Manager) addTaskExecutor(executor TaskExecutor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mu.taskExecutors[executor.GetTask().ID] = executor
}

func (m *Manager) delTaskExecutor(executor TaskExecutor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mu.taskExecutors, executor.GetTask().ID)
}

func (m *Manager) isExecutorStarted(taskID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.mu.taskExecutors[taskID]
	return ok
}

func (m *Manager) logErr(err error) {
	m.logger.Error("task manager met error", zap.Error(err), zap.Stack("stack"))
}

func (m *Manager) logErrAndPersist(err error, taskID int64, taskExecutor TaskExecutor) {
	m.logErr(err)
	// TODO we want to define err of taskexecutor.Init as fatal, but add-index have
	// some code in Init that need retry, remove it after it's decoupled.
	if taskExecutor != nil && taskExecutor.IsRetryableError(err) {
		m.logger.Error("met retryable err", zap.Error(err), zap.Stack("stack"))
		return
	}
	err1 := m.taskTable.FailSubtask(m.ctx, m.id, taskID, err)
	if err1 != nil {
		m.logger.Error("update to subtask failed", zap.Error(err1), zap.Stack("stack"))
	}
	m.logger.Error("update error to subtask", zap.Int64("task-id", taskID), zap.Error(err1), zap.Stack("stack"))
}
