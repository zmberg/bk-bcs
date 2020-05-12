/*
 * Tencent is pleased to support the open source community by making Blueking Container Service available.
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package scheduler

import (
	alarm "bk-bcs/bcs-common/common/bcs-health/api"
	"bk-bcs/bcs-common/common/blog"
	bcstype "bk-bcs/bcs-common/common/types"
	containertypes "bk-bcs/bcs-mesos/bcs-container-executor/container"
	"bk-bcs/bcs-mesos/bcs-scheduler/src/manager/store"
	"bk-bcs/bcs-mesos/bcs-scheduler/src/mesosproto/mesos"
	"bk-bcs/bcs-mesos/bcs-scheduler/src/mesosproto/sched"
	"bk-bcs/bcs-mesos/bcs-scheduler/src/types"
	"encoding/json"
	"net/http"
	//"sort"
	"strings"
	"time"
)

// The goroutine function for process task status report
// When scheduler receive a task status report messege, it will create a goroutine for process this message,
// #lizard forgives StatusReport
func (s *Scheduler) StatusReport(status *mesos.TaskStatus) {

	taskId := status.TaskId.GetValue()
	taskGroupID := types.GetTaskGroupID(taskId)
	if taskGroupID == "" {
		blog.Error("status report: can not get taskGroupId from taskID(%s)", taskId)
		return
	}
	runAs, appId := types.GetRunAsAndAppIDbyTaskGroupID(taskGroupID)
	s.store.LockApplication(runAs + "." + appId)
	defer s.store.UnLockApplication(runAs + "." + appId)

	// ack and check
	if s.preCheckTaskStatusReport(status) == false {
		return
	}

	now := time.Now().Unix()
	updateTime := now - MAX_DATA_UPDATE_INTERVAL

	state := status.GetState()
	healthy := status.GetHealthy()
	executorID := status.GetExecutorId()
	agentID := status.GetAgentId()
	taskData := status.GetData()
	task, err := s.store.FetchTask(taskId)
	if task == nil {
		blog.Warn("status report: fetch task(%s) return nil", taskId)
		return
	}

	var alarmTimeval uint16 = 600
	oldStatus := task.Status
	oldData := task.StatusData
	reportStatus := ""
	// update task status
	switch state {
	case mesos.TaskState_TASK_STAGING:
		blog.Warn("status report: Task(%s) Staging unexpected, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_STAGING
	case mesos.TaskState_TASK_STARTING:
		blog.Info("status report: Task(%s) Starting, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_STARTING
	case mesos.TaskState_TASK_RUNNING:
		blog.V(3).Infof("status report: Task(%s) Running, data:%s", taskId, string(taskData))
		reportStatus = types.TASK_STATUS_RUNNING
	case mesos.TaskState_TASK_FINISHED:
		blog.Info("status report: Task(%s) Finished, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_FINISH
	case mesos.TaskState_TASK_FAILED, mesos.TaskState_TASK_GONE:
		blog.Info("status report: Task(%s) mesos status(%d) Failed, message: %s", taskId, state, status.GetMessage())
		reportStatus = types.TASK_STATUS_FAIL
		taskGroup, _ := s.store.FetchTaskGroup(taskGroupID)
		if taskGroup != nil {
			s.SendHealthMsg(alarm.WarnKind, taskGroup.RunAs, task.ID+"("+taskGroup.HostName+")"+" fail, message:"+status.GetMessage(), taskGroup.RunAs+"."+taskGroup.Name+"-task", &alarmTimeval)
		}
	case mesos.TaskState_TASK_KILLING:
		blog.Info("status report: Task(%s) Killing, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_KILLING
	case mesos.TaskState_TASK_KILLED:
		blog.Info("status report: Task(%s) Killed, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_KILLED
	case mesos.TaskState_TASK_LOST, mesos.TaskState_TASK_UNREACHABLE, mesos.TaskState_TASK_GONE_BY_OPERATOR:
		blog.Info("status report: Task(%s) mesos status(%d) Lost, message: %s", taskId, state, status.GetMessage())
		reportStatus = types.TASK_STATUS_LOST
		taskGroup, _ := s.store.FetchTaskGroup(taskGroupID)
		if taskGroup != nil {
			if strings.Contains(status.GetMessage(), "is unreachable") {
				//s.addLostSlave(taskGroup.HostName)
				s.offerPool.AddLostSlave(taskGroup.HostName)
			}
			s.SendHealthMsg(alarm.WarnKind, taskGroup.RunAs, task.ID+"("+taskGroup.HostName+")"+" lost, message:"+status.GetMessage(), taskGroup.RunAs+"."+taskGroup.Name+"-task", &alarmTimeval)
		}
	case mesos.TaskState_TASK_ERROR:
		blog.Info("status report: Task(%s) Error, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_ERROR
	// extent state for restart
	case mesos.TaskState(types.Ext_TaskState_TASK_RESTARTING):
		blog.Info("status report: Task(%s) Restarting, message: %s", taskId, status.GetMessage())
		reportStatus = types.TASK_STATUS_RESTARTING
	default:
		blog.Error("status report: Unprocessed task status (%d), TaskID:%s, message: %s", state, taskId, status.GetMessage())
		return
	}

	task.Status = reportStatus
	task.Message = status.GetMessage()
	task.StatusData = string(taskData)
	if task.Status != oldStatus {
		blog.Info("status report: task %s, status change: %s --> %s", taskId, oldStatus, task.Status)
		task.LastStatus = oldStatus
		s.produceEvent(*task)
	}

	var bcsMsg *types.BcsMessage
	if task.StatusData != oldData {
		blog.Info("status report: task %s, statusData change: %s --> %s", taskId, oldData, task.StatusData)
		var containerInfo *containertypes.BcsContainerInfo
		err = json.Unmarshal([]byte(task.StatusData), &containerInfo)
		if err != nil {
			blog.Errorf("unmarshal task statusdata(%s) error: %s", task.StatusData, err.Error())
		} else {
			bcsMsg = containerInfo.BcsMessage
			task.IsChecked = containerInfo.IsChecked
			task.ConsecutiveFailureTimes = uint32(containerInfo.ConsecutiveFailureTimes)
		}
	}
	if oldData != "" && task.StatusData == "" {
		blog.Warn("status report: Task %s, Status: %s, reported StatusData is empty, keep oldData(%s)", taskId, task.Status, oldData)
		task.StatusData = oldData
	}

	healthyChg := s.checkTaskHealth(task, taskGroupID, healthy)

	taskUpdated := false
	if task.Status != oldStatus || task.StatusData != oldData || healthyChg {
		task.UpdateTime = now
		taskUpdated = true
	}

	if taskUpdated || task.LastUpdateTime <= updateTime {
		blog.V(3).Infof("status report: Save Task %s, Status: %s, StatusData: %s, Healthy: %t",
			taskId, task.Status, task.StatusData, task.Healthy)
	} else {
		blog.V(3).Infof("task %s status report, not change", taskId)
		return
	}
	task.LastUpdateTime = now
	if err = s.store.SaveTask(task); err != nil {
		blog.Error("status report: SaveTask %s err: %s", taskId, err.Error())
		return
	}

	// NOTE: in function FetchTaskGroup, tasks` data will update to taskgroup, we must fetch taskgroup here again
	taskGroup, err := s.store.FetchTaskGroup(taskGroupID)
	if err != nil {
		blog.Error("status report: Fetch task group %s failed: %s", taskGroupID, err.Error())
		return
	}
	blog.Info("status report: task(%s) status(%s), taskgroup(%s)", taskId, task.Status, taskGroup.Status)

	taskGroupStatus := taskGroup.Status
	// update taskGroup Status according to tasks status
	taskgroupUpdated, err := s.updateTaskgroup(taskGroup, agentID.GetValue(), executorID.GetValue())
	if err != nil {
		blog.Error("status report: updateTaskgroup %s failed", taskGroupID)
		return
	}
	if taskUpdated == true {
		taskgroupUpdated = true
	}
	if taskgroupUpdated == true {
		taskGroup.UpdateTime = now
	}

	reportTaskgroupReportMetrics(taskGroup.RunAs, taskGroup.AppID, taskGroup.Name, taskGroup.Status)
	// taskgroup info changed
	if taskGroup.LastUpdateTime <= updateTime || taskgroupUpdated == true {
		s.ServiceMgr.TaskgroupUpdate(taskGroup)
		if taskGroup.Status != taskGroupStatus {
			s.taskGroupStatusUpdated(taskGroup, taskGroupStatus)
		}
		if bcsMsg != nil {
			taskGroup.BcsEventMsg = bcsMsg
		}
		taskGroup.LastUpdateTime = now
		//save taskGroup into zk, in this function, task will alse be saved
		if err = s.store.SaveTaskGroup(taskGroup); err != nil {
			blog.Error("status report: save taskgroup: %s information into db failed! err:%s", taskGroup.ID, err.Error())
			return
		}
	}

	s.checkApplicationChange(runAs, appId, taskGroupStatus, taskGroup, now)
	return
}

func (s *Scheduler) checkTaskHealth(task *types.Task, taskGroupID string, healthy bool) bool {

	healthyChg := false
	if task.Status == types.TASK_STATUS_RUNNING {
		oldHealthy := task.Healthy
		task.Healthy = healthy
		if task.Healthy != oldHealthy {
			healthyChg = true
			for _, healthStatus := range task.HealthCheckStatus {
				switch healthStatus.Type {
				case bcstype.BcsHealthCheckType_COMMAND:
					healthStatus.Result = task.Healthy
				case bcstype.BcsHealthCheckType_TCP:
					healthStatus.Result = task.Healthy
				case bcstype.BcsHealthCheckType_HTTP:
					healthStatus.Result = task.Healthy
				}
			}
			blog.Infof("status report: Task(%s) healthy changed to %t", task.ID, task.Healthy)
			taskGroup, _ := s.store.FetchTaskGroup(taskGroupID)
			if taskGroup != nil {
				if task.Healthy == false {
					s.SendHealthMsg(alarm.WarnKind, taskGroup.RunAs, task.ID+"("+taskGroup.HostName+")"+" healthy change to false", "", nil)
				} else {
					s.SendHealthMsg(alarm.InfoKind, taskGroup.RunAs, task.ID+"("+taskGroup.HostName+")"+" healthy change to true", "", nil)
				}
			}
		}
		//check health check ConsecutiveFailureTimes
		if task.LocalMaxConsecutiveFailures > 0 {
			if !task.Healthy && task.IsChecked && task.ConsecutiveFailureTimes > task.LocalMaxConsecutiveFailures {
				blog.Infof("status report: task(%s) in running but not ConsecutiveFailureTimes(%d>%d), set to Failed",
					task.ID, task.ConsecutiveFailureTimes, task.LocalMaxConsecutiveFailures)
				healthyChg = true
				task.Status = types.TASK_STATUS_FAIL
				task.Message = "health check consecutive failure times over, kill by scheduler"
				taskGroup, _ := s.store.FetchTaskGroup(taskGroupID)
				if taskGroup != nil {
					s.KillTaskGroup(taskGroup)
				}
			}
		}
	}

	return healthyChg
}

func (s *Scheduler) checkApplicationChange(runAs, appId, taskGroupStatus string, taskGroup *types.TaskGroup, now int64) {

	applicationUpdated := false
	updateTime := now - MAX_DATA_UPDATE_INTERVAL

	app, err := s.store.FetchApplication(runAs, appId)
	if err != nil {
		blog.Error("status report: fetch application(%s.%s) failed, err:%s", runAs, appId, err.Error())
		return
	}

	appStatus := app.Status
	// add condition for performance
	if appStatus == types.APP_STATUS_OPERATING {
		if taskGroupStatus != taskGroup.Status {
			if taskGroup.Status == types.TASKGROUP_STATUS_RUNNING {
				app.RunningInstances = app.RunningInstances + 1
				blog.Info("applicaiton(%s.%s) RunningInstances change to %d", runAs, appId, app.RunningInstances)
				applicationUpdated = true
			} else if taskGroupStatus == types.TASKGROUP_STATUS_RUNNING {
				if app.RunningInstances > 0 {
					app.RunningInstances = app.RunningInstances - 1
					blog.Info("application(%s.%s) RunningInstances change to %d", runAs, appId, app.RunningInstances)
					applicationUpdated = true
				}
			}
		}
	} else {
		applicationUpdated, err = s.updateApplicationStatus(app)
	}

	if applicationUpdated {
		app.UpdateTime = now
		app.LastUpdateTime = now
		if err = s.store.SaveApplication(app); err != nil {
			blog.Error("status report: save application(%s.%s) information into db failed! err:%s", app.RunAs, app.ID, err.Error())
			return
		}
		s.applicationStatusUpdated(app, appStatus)

	} else if app.LastUpdateTime <= updateTime {
		app.LastUpdateTime = now
		if err = s.store.SaveApplication(app); err != nil {
			blog.Error("status report: save application(%s.%s) information into db failed! err:%s", app.RunAs, app.ID, err.Error())
			return
		}
		blog.Infof("save application(%s.%s) success",app.RunAs,app.ID)
	}

	return
}

func (s *Scheduler) preCheckTaskStatusReport(status *mesos.TaskStatus) bool {
	//ack
	if status.GetUuid() != nil {
		call := &sched.Call{
			FrameworkId: s.framework.GetId(),
			Type:        sched.Call_ACKNOWLEDGE.Enum(),
			Acknowledge: &sched.Call_Acknowledge{
				AgentId: status.GetAgentId(),
				TaskId:  status.GetTaskId(),
				Uuid:    status.GetUuid(),
			},
		}
		// send call
		resp, err := s.send(call)
		if err != nil {
			blog.Error("status report: Unable to send Acknowledge Call: %s ", err)
			return false
		}
		if resp.StatusCode != http.StatusAccepted {
			blog.Error("status report: Acknowledge call returned unexpected status: %d", resp.StatusCode)
			return false
		}
	}

	taskId := status.TaskId.GetValue()
	state := status.GetState()
	executorID := status.GetExecutorId()
	agentID := status.GetAgentId()
	blog.V(3).Infof("status report: get status report: task %s, status: %s, executorID: %s, agentID: %s ",
		taskId, state, executorID, agentID)
	taskGroupID := types.GetTaskGroupID(taskId)
	runAs, appId := types.GetRunAsAndAppIDbyTaskGroupID(taskGroupID)
	task, err := s.store.FetchTask(taskId)
	if err != nil && err != store.ErrNoFound {
		blog.Warn("status report: fetch task(%s) err(%s)", taskId, err.Error())
		return false
	}

	if task == nil {
		blog.Warn("status report: task(%s) not exist", taskId)
		taskGroups, err1 := s.store.ListTaskGroups(runAs, appId)
		if err1 != nil {
			blog.Warn("status report: list taskgroups(%s.%s) failed, err:%s", runAs, appId, err1.Error())
			return false
		}
		for _, taskGroup := range taskGroups {
			if taskGroup.ID == taskGroupID {
				blog.Error("status report: task(%s) not exist but taskgroup(%s) exist", taskId, taskGroupID)
				return false
			}
		}

		if agentID.GetValue() == "" || executorID.GetValue() == "" {
			blog.Warn("status report: task(%s) not exist and reported executor(%s) or agent(%s) error, do nothing",
				taskId, executorID.GetValue(), agentID.GetValue())
			return false
		}

		blog.Warn("status report: task(%s) not eixst, kill executor(%s) on agent(%s)", taskId, executorID.GetValue(), agentID.GetValue())
		s.KillExecutor(agentID.GetValue(), executorID.GetValue())
		return false
	}

	if task.Status == types.TASK_STATUS_FINISH || task.Status == types.TASK_STATUS_ERROR || task.Status == types.TASK_STATUS_KILLED || task.Status == types.TASK_STATUS_FAIL {
		blog.Warn("status report ignored: task %s, but current status is %s", taskId, task.Status)
		return false
	}

	return true
}

func (s *Scheduler) updateTaskgroup(taskGroup *types.TaskGroup, agentId, executorId string) (bool, error) {
	isUpdated := false

	if "" != agentId && taskGroup.AgentID != agentId {
		taskGroup.AgentID = agentId
		isUpdated = true
	}

	if "" != executorId && taskGroup.ExecutorID != executorId {
		taskGroup.ExecutorID = executorId
		isUpdated = true
	}

	// already in end statuses, donot change
	if taskGroup.Status == types.TASKGROUP_STATUS_FINISH || taskGroup.Status == types.TASKGROUP_STATUS_ERROR || taskGroup.Status == types.TASKGROUP_STATUS_KILLED || taskGroup.Status == types.TASKGROUP_STATUS_FAIL {
		blog.V(3).Infof("taskgroup %s is already in end status:%s ", taskGroup.ID, taskGroup.Status)

	} else {
		currStatus := taskGroup.Status
		status := types.TASKGROUP_STATUS_UNKNOWN
		totalNum := 0
		stagingNum := 0
		startingNum := 0
		runningNum := 0
		finishedNum := 0
		errorNum := 0
		failedNum := 0
		killedNum := 0
		killingNum := 0
		lostNum := 0
		restartingNum := 0
		unknowNum := 0

		var errMessage string
		var failedMessage string

		for _, task := range taskGroup.Taskgroup {
			totalNum++
			switch task.Status {
			case types.TASK_STATUS_RESTARTING:
				restartingNum++
			case types.TASK_STATUS_STAGING:
				stagingNum++
			case types.TASK_STATUS_STARTING:
				startingNum++
			case types.TASK_STATUS_RUNNING:
				runningNum++
			case types.TASK_STATUS_FINISH:
				finishedNum++
			case types.TASK_STATUS_ERROR:
				errorNum++
				errMessage = task.Message
			case types.TASK_STATUS_FAIL:
				failedNum++
				failedMessage = task.Message
			case types.TASK_STATUS_KILLED:
				killedNum++
			case types.TASK_STATUS_KILLING:
				killingNum++
			case types.TASK_STATUS_LOST:
				lostNum++
			default:
				blog.Error("unknow task status %s for task: %s", task.Status, task.ID)
				unknowNum++
			}
		}

		blog.V(3).Infof("Tasks status for taskGroup %s : totalNum(%d) restartNum(%d) stagingNum(%d) "+
			"startingNum(%d) runningNum(%d) finishedNum(%d) errorNum(%d) failedNum(%d) killingNum(%d) killedNum(%d) "+
			"lostNum(%d) unknowNum(%d)", taskGroup.ID, totalNum, restartingNum, stagingNum, startingNum, runningNum,
			finishedNum, errorNum, failedNum, killingNum, killedNum, lostNum, unknowNum)

		if failedNum > 0 {
			status = types.TASKGROUP_STATUS_FAIL
			taskGroup.Message = failedMessage
		} else if killingNum > 0 {
			status = types.TASKGROUP_STATUS_KILLING
			taskGroup.Message = "some tasks are killing"
		} else if errorNum > 0 {
			status = types.TASKGROUP_STATUS_ERROR
			taskGroup.Message = errMessage
		} else if totalNum == finishedNum {
			status = types.TASKGROUP_STATUS_FINISH
			taskGroup.Message = "all tasks are finish"
		} else if totalNum == killedNum {
			status = types.TASKGROUP_STATUS_KILLED
			taskGroup.Message = "some tasks are killed"
		} else if lostNum > 0 {
			status = types.TASKGROUP_STATUS_LOST
			taskGroup.Message = "some tasks are lost"
		} else if stagingNum == totalNum {
			status = types.TASKGROUP_STATUS_STAGING
			taskGroup.Message = "all tasks is staging"
		} else if startingNum > 0 {
			status = types.TASKGROUP_STATUS_STARTING
			taskGroup.Message = "some tasks are starting"
		} else if restartingNum > 0 {
			status = types.TASKGROUP_STATUS_RESTARTING
			taskGroup.Message = "pod is restarting"
		} else if runningNum > 0 {
			status = types.TASKGROUP_STATUS_RUNNING
			taskGroup.Message = "pod is running"
		} else {
			blog.Error("Unknow status for taskGroup %s, tasks: totalNum(%d) stagingNum(%d) startingNum(%d) "+
				"runningNum(%d) finishedNum(%d) errorNum(%d) failedNum(%d) killingNum(%d) killedNum(%d) lostNum(%d) "+
				"unknowNum(%d)", taskGroup.ID, totalNum, stagingNum, startingNum, runningNum, finishedNum, errorNum,
				failedNum, killingNum, killedNum, lostNum, unknowNum)
		}

		if currStatus != status {
			blog.Info("taskgroup %s status changed: %s -> %s", taskGroup.ID, currStatus, status)
			taskGroup.Status = status
			taskGroup.LastStatus = currStatus
			isUpdated = true
		}
	}

	return isUpdated, nil
}

// update application`s status according to taskgroups` status
func (s *Scheduler) updateApplicationStatus(app *types.Application) (bool, error) {

	isUpdated := false

	runAs := app.RunAs
	appId := app.ID

	taskGroups, err := s.store.ListTaskGroups(runAs, appId)
	if err != nil {
		blog.Warn("list taskgroups(%s.%s) failed, err:%s", runAs, appId, err.Error())
		return isUpdated, err
	}

	currStatus := app.Status
	totalNum := 0
	stagingNum := 0
	startingNum := 0
	runningNum := 0
	finishedNum := 0
	errorNum := 0
	failedNum := 0
	killedNum := 0
	killingNum := 0
	lostNum := 0
	unknowNum := 0

	for _, taskGroup := range taskGroups {
		totalNum++
		switch taskGroup.Status {
		case types.TASKGROUP_STATUS_STAGING:
			stagingNum++
		case types.TASKGROUP_STATUS_STARTING:
			startingNum++
		case types.TASKGROUP_STATUS_RUNNING:
			runningNum++
		case types.TASKGROUP_STATUS_RESTARTING:
			runningNum++
		case types.TASKGROUP_STATUS_FINISH:
			finishedNum++
		case types.TASKGROUP_STATUS_ERROR:
			errorNum++
		case types.TASKGROUP_STATUS_FAIL:
			failedNum++
		case types.TASKGROUP_STATUS_KILLED:
			killedNum++
		case types.TASKGROUP_STATUS_KILLING:
			killingNum++
		case types.TASKGROUP_STATUS_LOST:
			lostNum++
		default:
			blog.Error("unknow taskgroup status %s for taskgroup: %s", taskGroup.Status, taskGroup.ID)
			unknowNum++
		}
	}

	blog.V(3).Infof("TaskGroups status for application(%s.%s): totalNum(%d) stagingNum(%d) startingNum(%d) "+
		"runningNum(%d) finishedNum(%d) errorNum(%d) failedNum(%d) killingNum(%d) killedNum(%d) lostNum(%d) unknowNum(%d)",
		runAs, appId, totalNum, stagingNum, startingNum, runningNum, finishedNum, errorNum, failedNum, killingNum,
		killedNum, lostNum, unknowNum)

	if totalNum != int(app.Instances) {
		blog.Error("applicaiton(%s.%s) Instances(%d), but only find %d", runAs, appId, app.Instances, totalNum)
	}

	var status, message string
	if errorNum > 0 {
		status = types.APP_STATUS_ERROR
		message = "application has error pods"
	} else if failedNum > 0 {
		status = types.APP_STATUS_ABNORMAL
		message = "application has failed pods"
	} else if lostNum > 0 {
		status = types.APP_STATUS_ABNORMAL
		message = "have some lost taskgroups"
	} else if totalNum < int(app.DefineInstances) {
		status = types.APP_STATUS_ABNORMAL
		message = "have not enough resources to launch application"
	} else if finishedNum == totalNum {
		status = types.APP_STATUS_FINISH
		message = "all pods are finish"
	} else if startingNum+stagingNum > 0 {
		status = types.APP_STATUS_DEPLOYING
		message = "some pods in staing or starting"
	} else if runningNum == int(app.DefineInstances) {
		status = types.APP_STATUS_RUNNING
		message = types.APP_STATUS_RUNNING_STR
	} else {
		status = types.APP_STATUS_ABNORMAL
		message = types.APP_STATUS_ABNORMAL_STR
	}

	if app.Status == types.APP_STATUS_OPERATING || app.Status == types.APP_STATUS_ROLLINGUPDATE {
		blog.V(3).Infof("application(%s.%s) status(%s), not change", runAs, appId, app.Status)
	} else if currStatus != status {
		blog.Info("applicaiton(%s.%s) status changed: %s -> %s", runAs, appId, currStatus, status)
		app.Status = status
		app.Message = message
		app.SubStatus = types.APP_SUBSTATUS_UNKNOWN
		app.LastStatus = currStatus
		isUpdated = true
	}

	if app.RunningInstances != uint64(runningNum) {
		blog.Info("applicaiton(%s.%s) RunningInstances changed: %d -> %d", runAs, appId, app.RunningInstances, runningNum)
		app.RunningInstances = uint64(runningNum)
		isUpdated = true
	}

	return isUpdated, nil
}

// after a taskgroup's status changed, do some work in this function
func (s *Scheduler) taskGroupStatusUpdated(taskGroup *types.TaskGroup, originStatus string) {

	if taskGroup.Status == originStatus {
		return
	}

	if taskGroup.Status == types.TASKGROUP_STATUS_FAIL || taskGroup.Status == types.TASKGROUP_STATUS_LOST || taskGroup.Status == types.TASK_STATUS_KILLED || taskGroup.Status == types.TASKGROUP_STATUS_UNKNOWN {
		blog.Info("taskgroup(%s) status come to %s, send alarm message", taskGroup.ID, taskGroup.Status)
		var alarmTimeval uint16 = 600
		s.SendHealthMsg(alarm.WarnKind, taskGroup.RunAs, taskGroup.ID+"("+taskGroup.HostName+")"+"status changed:"+originStatus+"->"+taskGroup.Status, taskGroup.RunAs+"."+taskGroup.Name, &alarmTimeval)
	}

	if taskGroup.Status == types.TASKGROUP_STATUS_FAIL || taskGroup.Status == types.TASKGROUP_STATUS_LOST || taskGroup.Status == types.TASKGROUP_STATUS_FINISH {
		blog.Info("taskgroup(%s) status come to %s, check reschedule ", taskGroup.ID, taskGroup.Status)
		if taskGroup.RestartPolicy == nil {
			blog.Info("taskgroup(%s) has no restart policy, donot reschedule", taskGroup.ID)
			return
		}
		blog.V(3).Infof("taskgroup(%s) restart policy: %s", taskGroup.ID, taskGroup.RestartPolicy.Policy)

		if taskGroup.RestartPolicy.Policy == bcstype.RestartPolicy_NEVER {
			blog.Info("taskgroup(%s) restart policy(%s), donot reschedule", taskGroup.ID, taskGroup.RestartPolicy.Policy)
			return
		}
		if taskGroup.RestartPolicy.Policy != bcstype.RestartPolicy_ALWAYS && taskGroup.Status != types.TASKGROUP_STATUS_FAIL {
			blog.Info("taskgroup(%s) restart policy(%s) status(%s), donot reschedule",
				taskGroup.ID, taskGroup.RestartPolicy.Policy, taskGroup.Status)
			return
		}

		forceReschedule := false
		if taskGroup.Status == types.TASKGROUP_STATUS_LOST {
			blog.Infof("taskgroup(%s) status come to LOST, force to do reschedule", taskGroup.ID)
			forceReschedule = true
		}

		reschedTimes := taskGroup.ReschededTimes
		lastTime := taskGroup.LastReschedTime
		now := time.Now().Unix()
		var passTime int64
		if lastTime > 0 && lastTime < now {
			passTime = now - lastTime
		}

		if passTime >= TRANSACTION_RESCHEDULE_RESET_INTERVAL {
			blog.Info("taskgroup(%s) ReschededTimes and LastReschedTime resetted after running more than %d seconds",
				taskGroup.ID, TRANSACTION_RESCHEDULE_RESET_INTERVAL)
			taskGroup.ReschededTimes = 0
			taskGroup.LastReschedTime = 0
			reschedTimes = 0
		}

		maxTimes := taskGroup.RestartPolicy.MaxTimes
		if maxTimes > 0 && reschedTimes >= maxTimes {
			blog.Warn("taskgroup(%s) already reschedule times(%d/%d), will not reschedule again",
				taskGroup.ID, reschedTimes, maxTimes)
			return
		}
		delayTime := taskGroup.RestartPolicy.Interval
		if reschedTimes > 0 {
			delayTime = delayTime + reschedTimes*taskGroup.RestartPolicy.Backoff
		}
		if delayTime < 0 {
			delayTime = 0
		}

		if delayTime > TRANSACTION_INNER_RESCHEDULE_LIFEPERIOD-TRANSACTION_DEFAULT_LIFEPERIOD {
			delayTime = TRANSACTION_INNER_RESCHEDULE_LIFEPERIOD - TRANSACTION_DEFAULT_LIFEPERIOD
		}

		blog.Info("taskgroup(%s) will be rescheduled %d seconds later, already reschedule %d times",
			taskGroup.ID, delayTime, reschedTimes)

		taskGroupID := taskGroup.ID
		runAs, appID := types.GetRunAsAndAppIDbyTaskGroupID(taskGroupID)

		//var rescheduleTrans Transaction
		rescheduleTrans := CreateTransaction()
		rescheduleTrans.DelayTime = int64(delayTime)
		rescheduleTrans.RunAs = runAs
		rescheduleTrans.AppID = appID
		rescheduleTrans.OpType = types.OPERATION_RESCHEDULE
		rescheduleTrans.Status = types.OPERATION_STATUS_INIT
		rescheduleTrans.LifePeriod = TRANSACTION_INNER_RESCHEDULE_LIFEPERIOD

		var rescheduleOpdata TransRescheduleOpData
		rescheduleOpdata.TaskGroupID = taskGroupID
		rescheduleOpdata.Force = forceReschedule
		rescheduleOpdata.IsInner = true

		rescheduleOpdata.HostRetainTime = taskGroup.RestartPolicy.HostRetainTime
		if rescheduleOpdata.HostRetainTime > 0 {
			blog.Info("taskgroup(%s) will rescheduled retain host(%s) for %d seconds",
				taskGroup.ID, taskGroup.HostName, rescheduleOpdata.HostRetainTime)
			rescheduleOpdata.HostRetain = taskGroup.HostName
		} else {
			rescheduleOpdata.HostRetainTime = 0
		}

		// computer resource needed
		//versions, err := s.store.ListVersions(runAs, appID)
		//if err != nil {
		//	blog.Error("reschedule %s fail: list versions for application(%s.%s), err:%s",
		//		taskGroupID, runAs, appID, err.Error())
		//	return
		//}
		//sort.Strings(versions)
		//newestVersion := versions[len(versions)-1]
		//version, err := s.store.FetchVersion(runAs, appID, newestVersion)
		//if err != nil {
		//	blog.Error("reschedule taskgroup(%s) fail, fetch version(%s) for application(%s.%s), err:%s",
		//		taskGroupID, newestVersion, runAs, appID, err.Error())
		//	return
		//}
		version, _ := s.store.GetVersion(runAs, appID)
		if version == nil {
			blog.Error("prepare reschedule taskgroup(%s) fail, no version for application(%s.%s)", runAs, appID)
			return
		}
		rescheduleOpdata.NeedResource = version.AllResource()
		rescheduleOpdata.Version = version
		rescheduleTrans.OpData = &rescheduleOpdata
		go s.RunRescheduleTaskgroup(rescheduleTrans)
	}

	return
}

// after a application's status changed, do some work in this function
func (s *Scheduler) applicationStatusUpdated(app *types.Application, originStatus string) {

	if originStatus == app.Status {
		return
	}

	blog.Infof("application(%s.%s) status change from %s to %s", app.RunAs, app.ID, originStatus, app.Status)

	return
}

//current only update task status running by mesos message, if task status changed by mesos status update
func (s *Scheduler) UpdateTaskStatus(agentID, executorID string, bcsMsg *types.BcsMessage) {
	taskId := bcsMsg.TaskID.GetValue()
	taskGroupID := types.GetTaskGroupID(taskId)
	if taskGroupID == "" {
		blog.Error("message status report: can not get taskGroupId from taskID(%s)", taskId)
		return
	}
	runAs, appId := types.GetRunAsAndAppIDbyTaskGroupID(taskGroupID)
	s.store.LockApplication(runAs + "." + appId)
	defer s.store.UnLockApplication(runAs + "." + appId)

	// ack and check
	if s.preCheckMessageTaskStatus(agentID, executorID, taskId) == false {
		return
	}

	now := time.Now().Unix()
	updateTime := now - MAX_DATA_UPDATE_INTERVAL
	task, err := s.store.FetchTask(taskId)
	if task == nil {
		blog.Warn("message status report: fetch task(%s) return nil", taskId)
		return
	}

	var taskInfo *containertypes.BcsContainerInfo
	err = json.Unmarshal(bcsMsg.TaskStatus, &taskInfo)
	if err != nil {
		blog.Errorf("message Unmarshal data(%s) to types.BcsMessage error %s", string(bcsMsg.TaskStatus), err.Error())
		return
	}
	oldStatus := task.Status
	oldData := task.StatusData
	reportStatus := ""
	// update task status
	switch strings.ToLower(taskInfo.Status) {
	case "running":
		blog.V(3).Infof("message status report: Task(%s) Running", taskId)
		reportStatus = types.TASK_STATUS_RUNNING
	default:
		blog.Warnf("message status report: Unprocessed task status (%v), TaskID:%s", taskInfo, taskId)
		return
	}

	task.Status = reportStatus
	task.StatusData = string(bcsMsg.TaskStatus)

	var msg *types.BcsMessage
	if task.StatusData != oldData {
		blog.Info("message status report: task %s, statusData change: %s --> %s", taskId, oldData, task.StatusData)
		var containerInfo *containertypes.BcsContainerInfo
		err = json.Unmarshal([]byte(task.StatusData), &containerInfo)
		if err != nil {
			blog.Errorf("message unmarshal task statusdata(%s) error: %s", task.StatusData, err.Error())
		} else {
			msg = containerInfo.BcsMessage
			task.IsChecked = containerInfo.IsChecked
			task.ConsecutiveFailureTimes = uint32(containerInfo.ConsecutiveFailureTimes)
		}
	}
	if oldData != "" && task.StatusData == "" {
		blog.Warn("message status report: Task %s, Status: %s, reported StatusData is empty, keep oldData(%s)", taskId, task.Status, oldData)
		task.StatusData = oldData
	}

	healthyChg := s.checkTaskHealth(task, taskGroupID, taskInfo.Healthy)
	taskUpdated := false
	if task.Status != oldStatus || task.StatusData != oldData || healthyChg {
		task.UpdateTime = now
		taskUpdated = true
	}

	if taskUpdated || task.LastUpdateTime <= updateTime {
		blog.V(3).Infof("message status report: Save Task %s, Status: %s, StatusData: %s, Healthy: %t",
			taskId, task.Status, task.StatusData, task.Healthy)
	} else {
		blog.V(3).Infof("task %s status report, not change", taskId)
		return
	}
	task.LastUpdateTime = now
	if err = s.store.SaveTask(task); err != nil {
		blog.Error("message status report: SaveTask %s err: %s", taskId, err.Error())
		return
	}

	// NOTE: in function FetchTaskGroup, tasks` data will update to taskgroup, we must fetch taskgroup here again
	taskGroup, err := s.store.FetchTaskGroup(taskGroupID)
	if err != nil {
		blog.Error("message status report: Fetch task group %s failed: %s", taskGroupID, err.Error())
		return
	}
	blog.Info("message status report: task(%s) status(%s), taskgroup(%s)", taskId, task.Status, taskGroup.Status)

	taskGroupStatus := taskGroup.Status
	// update taskGroup Status according to tasks status
	taskgroupUpdated, err := s.updateTaskgroup(taskGroup, agentID, executorID)
	if err != nil {
		blog.Error("status report: updateTaskgroup %s failed", taskGroupID)
		return
	}
	if taskUpdated == true {
		taskgroupUpdated = true
	}
	if taskgroupUpdated == true {
		taskGroup.UpdateTime = now
	}

	reportTaskgroupReportMetrics(taskGroup.RunAs, taskGroup.AppID, taskGroup.Name, taskGroup.Status)
	// taskgroup info changed
	if taskGroup.LastUpdateTime <= updateTime || taskgroupUpdated == true {
		s.ServiceMgr.TaskgroupUpdate(taskGroup)
		if taskGroup.Status != taskGroupStatus {
			s.taskGroupStatusUpdated(taskGroup, taskGroupStatus)
		}
		if msg != nil {
			taskGroup.BcsEventMsg = msg
		}
		taskGroup.LastUpdateTime = now
		//save taskGroup into zk, in this function, task will alse be saved
		if err = s.store.SaveTaskGroup(taskGroup); err != nil {
			blog.Error("message status report: save taskgroup: %s information into db failed! err:%s", taskGroup.ID, err.Error())
			return
		}
	}

	s.checkApplicationChange(runAs, appId, taskGroupStatus, taskGroup, now)
	return
}

func (s *Scheduler) preCheckMessageTaskStatus(agentID, executorID, taskId string) bool {

	taskGroupID := types.GetTaskGroupID(taskId)
	runAs, appId := types.GetRunAsAndAppIDbyTaskGroupID(taskGroupID)
	task, err := s.store.FetchTask(taskId)
	if err != nil && err != store.ErrNoFound {
		blog.Warn("message status report: fetch task(%s) err(%s)", taskId, err.Error())
		return false
	}
	blog.V(3).Infof("message status report: get status report: task %s, executorID: %s, agentID: %s ",
		taskId, executorID, agentID)

	if task == nil {
		blog.Warn("message status report: task(%s) not exist", taskId)
		taskGroups, err1 := s.store.ListTaskGroups(runAs, appId)
		if err1 != nil {
			blog.Warn("message status report: list taskgroups(%s.%s) failed, err:%s", runAs, appId, err1.Error())
			return false
		}
		for _, taskGroup := range taskGroups {
			if taskGroup.ID == taskGroupID {
				blog.Error("message status report: task(%s) not exist but taskgroup(%s) exist", taskId, taskGroupID)
				return false
			}
		}

		if agentID == "" || executorID == "" {
			blog.Warn("message status report: task(%s) not exist and reported executor(%s) or agent(%s) error, do nothing",
				taskId, executorID, agentID)
			return false
		}

		blog.Warn("message status report: task(%s) not eixst, kill executor(%s) on agent(%s)", taskId, executorID, agentID)
		s.KillExecutor(agentID, executorID)
		return false
	}

	return true
}
