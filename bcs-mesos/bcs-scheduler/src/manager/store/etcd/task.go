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

package etcd

import (
	"bk-bcs/bcs-common/common/blog"
	mstore "bk-bcs/bcs-mesos/bcs-scheduler/src/manager/store"
	"bk-bcs/bcs-mesos/bcs-scheduler/src/types"
	"bk-bcs/bcs-mesos/pkg/apis/bkbcs/v2"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (store *managerStore) CheckTaskExist(task *types.Task) (string, bool) {
	obj, err := store.FetchTask(task.ID)
	if err == nil {
		return obj.ResourceVersion, true
	}

	return "", false
}

func (store *managerStore) SaveTask(task *types.Task) error {
	now := time.Now().UnixNano()
	ns, _ := types.GetRunAsAndAppIDbyTaskID(task.ID)
	client := store.BkbcsClient.Tasks(ns)
	v2Task := &v2.Task{
		TypeMeta: metav1.TypeMeta{
			Kind:       CrdTask,
			APIVersion: ApiversionV2,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      task.ID,
			Namespace: ns,
		},
		Spec: v2.TaskSpec{
			Task: *task,
		},
	}

	var err error
	rv, exist := store.CheckTaskExist(task)
	if exist && rv != "" {
		v2Task.ResourceVersion = rv
		v2Task, err = client.Update(v2Task)
	} else {
		v2Task, err = client.Create(v2Task)
	}
	if err != nil {
		return err
	}

	task.ResourceVersion = v2Task.ResourceVersion
	saveCacheTask(task)
	blog.Warnf("save task(%s) time(%d)", task.ID, (time.Now().UnixNano()-now)/1000/1000)
	return nil
}

/*func (store *managerStore) ListTasks(runAs, appID string) ([]*types.Task, error) {
	client := store.BkbcsClient.Tasks(runAs)
	v2Tasks, err := client.List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	tasks := make([]*types.Task, 0, len(v2Tasks.Items))
	for _, task := range v2Tasks.Items {
		_, appid := types.GetRunAsAndAppIDbyTaskID(task.Spec.ID)
		if appID == appid {
			task.Spec.Task.ResourceVersion = task.ResourceVersion
			obj := task.Spec.Task
			tasks = append(tasks, &obj)
		}
	}
	return tasks, nil
}*/

func (store *managerStore) DeleteTask(taskId string) error {
	now := time.Now().UnixNano()
	ns, _ := types.GetRunAsAndAppIDbyTaskID(taskId)
	client := store.BkbcsClient.Tasks(ns)
	err := client.Delete(taskId, &metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	deleteCacheTask(taskId)
	blog.Warnf("delete task(%s) time(%d)", taskId, (time.Now().UnixNano()-now)/1000/1000)
	return nil
}

func (store *managerStore) FetchTask(taskId string) (*types.Task, error) {
	if cacheMgr.isOK {
		cacheTask, _ := fetchCacheTask(taskId)
		if cacheTask == nil {
			return nil, mstore.ErrNoFound
		}
		return cacheTask, nil
	}

	return store.FetchDBTask(taskId)
}

func (store *managerStore) FetchDBTask(taskId string) (*types.Task, error) {
	ns, _ := types.GetRunAsAndAppIDbyTaskID(taskId)
	client := store.BkbcsClient.Tasks(ns)
	v2Task, err := client.Get(taskId, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, mstore.ErrNoFound
		}
		return nil, err
	}

	v2Task.Spec.Task.ResourceVersion = v2Task.ResourceVersion
	return &v2Task.Spec.Task, nil
}
