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

package update

import (
	"fmt"
	"sort"
	"time"

	tkexv1alpha1 "github.com/Tencent/bk-bcs/bcs-k8s/bcs-gamedeployment-operator/pkg/apis/tkex/v1alpha1"
	gdcore "github.com/Tencent/bk-bcs/bcs-k8s/bcs-gamedeployment-operator/pkg/core"
	"github.com/Tencent/bk-bcs/bcs-k8s/bcs-gamedeployment-operator/pkg/util"
	"github.com/Tencent/bk-bcs/bcs-k8s/kubernetes/common/expectations"
	"github.com/Tencent/bk-bcs/bcs-k8s/kubernetes/common/update/hotpatchupdate"
	"github.com/Tencent/bk-bcs/bcs-k8s/kubernetes/common/update/inplaceupdate"
	"github.com/Tencent/bk-bcs/bcs-k8s/kubernetes/common/util/requeueduration"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
)

// Interface for managing pods updating.
type Interface interface {
	Manage(cs *tkexv1alpha1.GameDeployment,
		updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
		pods []*v1.Pod,
	) (time.Duration, error)
}

func New(kubeClient clientset.Interface, recorder record.EventRecorder, scaleExp expectations.ScaleExpectations, updateExp expectations.UpdateExpectations) Interface {
	return &realControl{
		inPlaceControl:  inplaceupdate.NewForTypedClient(kubeClient, apps.ControllerRevisionHashLabelKey),
		hotPatchControl: hotpatchupdate.NewForTypedClient(kubeClient, apps.ControllerRevisionHashLabelKey),
		kubeClient:      kubeClient,
		recorder:        recorder,
		scaleExp:        scaleExp,
		updateExp:       updateExp,
	}
}

type realControl struct {
	kubeClient      clientset.Interface
	inPlaceControl  inplaceupdate.Interface
	hotPatchControl hotpatchupdate.Interface
	recorder        record.EventRecorder
	scaleExp        expectations.ScaleExpectations
	updateExp       expectations.UpdateExpectations
}

func (c *realControl) Manage(deploy *tkexv1alpha1.GameDeployment,
	updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
	pods []*v1.Pod,
) (time.Duration, error) {

	requeueDuration := requeueduration.Duration{}
	coreControl := gdcore.New(deploy)

	if deploy.Spec.UpdateStrategy.Paused {
		return requeueDuration.Get(), nil
	}

	// 1. find currently updated and not-ready count and all pods waiting to update
	var waitUpdateIndexes []int
	for i := range pods {
		if coreControl.IsPodUpdatePaused(pods[i]) {
			continue
		}

		if res := c.inPlaceControl.Refresh(pods[i], coreControl.GetUpdateOptions()); res.RefreshErr != nil {
			klog.Errorf("GameDeployment %s/%s failed to update pod %s condition for inplace: %v",
				deploy.Namespace, deploy.Name, pods[i].Name, res.RefreshErr)
			return requeueDuration.Get(), res.RefreshErr
		} else if res.DelayDuration > 0 {
			requeueDuration.Update(res.DelayDuration)
		}

		if util.GetPodRevision(pods[i]) != updateRevision.Name {
			waitUpdateIndexes = append(waitUpdateIndexes, i)
		}
	}

	// 2. sort all pods waiting to update
	waitUpdateIndexes = sortUpdateIndexes(coreControl, deploy.Spec.UpdateStrategy, pods, waitUpdateIndexes)

	// 3. calculate max count of pods can update
	needToUpdateCount := calculateUpdateCount(coreControl, deploy.Spec.UpdateStrategy, deploy.Spec.MinReadySeconds, int(*deploy.Spec.Replicas), waitUpdateIndexes, pods)
	if needToUpdateCount < len(waitUpdateIndexes) {
		waitUpdateIndexes = waitUpdateIndexes[:needToUpdateCount]
	}

	// 4. update pods
	for _, idx := range waitUpdateIndexes {
		pod := pods[idx]
		if duration, err := c.updatePod(deploy, coreControl, updateRevision, revisions, pod); err != nil {
			return requeueDuration.Get(), err
		} else if duration > 0 {
			requeueDuration.Update(duration)
		}
	}

	return requeueDuration.Get(), nil
}

func sortUpdateIndexes(coreControl gdcore.Control, strategy tkexv1alpha1.GameDeploymentUpdateStrategy, pods []*v1.Pod, waitUpdateIndexes []int) []int {
	// Sort Pods with default sequence
	sort.Slice(waitUpdateIndexes, coreControl.GetPodsSortFunc(pods, waitUpdateIndexes))
	return waitUpdateIndexes
}

func calculateUpdateCount(coreControl gdcore.Control, strategy tkexv1alpha1.GameDeploymentUpdateStrategy, minReadySeconds int32, totalReplicas int, waitUpdateIndexes []int, pods []*v1.Pod) int {
	partition := 0
	if strategy.Partition != nil {
		partition = int(*strategy.Partition)
	}

	if len(waitUpdateIndexes)-partition <= 0 {
		return 0
	}
	waitUpdateIndexes = waitUpdateIndexes[:(len(waitUpdateIndexes) - partition)]

	roundUp := true
	if strategy.MaxSurge != nil {
		maxSurge, _ := intstrutil.GetValueFromIntOrPercent(strategy.MaxSurge, totalReplicas, true)
		roundUp = maxSurge == 0
	}
	maxUnavailable, _ := intstrutil.GetValueFromIntOrPercent(
		intstrutil.ValueOrDefault(strategy.MaxUnavailable, intstrutil.FromString(tkexv1alpha1.DefaultGameDeploymentMaxUnavailable)), totalReplicas, roundUp)
	usedSurge := len(pods) - totalReplicas

	var notReadyCount, updateCount int
	for _, p := range pods {
		if !coreControl.IsPodUpdateReady(p, minReadySeconds) {
			notReadyCount++
		}
	}
	for _, i := range waitUpdateIndexes {
		if coreControl.IsPodUpdateReady(pods[i], minReadySeconds) {
			if notReadyCount >= (maxUnavailable + usedSurge) {
				break
			} else {
				notReadyCount++
			}
		}
		updateCount++
	}

	return updateCount
}

func (c *realControl) updatePod(deploy *tkexv1alpha1.GameDeployment, coreControl gdcore.Control,
	updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
	pod *v1.Pod,
) (time.Duration, error) {
	var oldRevision *apps.ControllerRevision
	for _, r := range revisions {
		if r.Name == util.GetPodRevision(pod) {
			oldRevision = r
			break
		}
	}

	switch deploy.Spec.UpdateStrategy.Type {
	case tkexv1alpha1.InPlaceGameDeploymentUpdateStrategyType:
		res := c.inPlaceControl.Update(pod, oldRevision, updateRevision, coreControl.GetUpdateOptions())

		if res.InPlaceUpdate {
			if res.UpdateErr == nil {
				c.recorder.Eventf(deploy, v1.EventTypeNormal, "SuccessfulUpdatePodInPlace", "successfully update pod %s in-place", pod.Name)
				c.updateExp.ExpectUpdated(util.GetControllerKey(deploy), updateRevision.Name, pod)
				return res.DelayDuration, nil
			}

			c.recorder.Eventf(deploy, v1.EventTypeWarning, "FailedUpdatePodInPlace", "failed to update pod %s in-place: %v", pod.Name, res.UpdateErr)
			return res.DelayDuration, res.UpdateErr

		}

		err := fmt.Errorf("find Pod %s update strategy is HotPatch, but the diff not only contains replace operation of spec.containers[x].image", pod)
		c.recorder.Eventf(deploy, v1.EventTypeWarning, "FailedUpdatePodInPlace", "find Pod %s update strategy is InPlace but can not update in-place: %v", pod.Name, err)
		klog.Warningf("GameDeployment %s/%s can not update Pod %s in-place: v%", deploy.Namespace, deploy.Name, pod.Name, err)
		return res.DelayDuration, err
	case tkexv1alpha1.RollingGameDeploymentUpdateStrategyType:
		klog.V(2).Infof("GameDeployment %s/%s deleting Pod %s for update %s", deploy.Namespace, deploy.Name, pod.Name, updateRevision.Name)

		c.scaleExp.ExpectScale(util.GetControllerKey(deploy), expectations.Delete, pod.Name)
		if err := c.kubeClient.CoreV1().Pods(deploy.Namespace).Delete(pod.Name, &metav1.DeleteOptions{}); err != nil {
			c.scaleExp.ObserveScale(util.GetControllerKey(deploy), expectations.Delete, pod.Name)
			c.recorder.Eventf(deploy, v1.EventTypeWarning, "FailedUpdatePodReCreate",
				"failed to delete pod %s for update: %v", pod.Name, err)
			return 0, err
		}

		c.recorder.Eventf(deploy, v1.EventTypeNormal, "SuccessfulUpdatePodReCreate",
			"successfully delete pod %s for update", pod.Name)
		return 0, nil

	case tkexv1alpha1.HotPatchGameDeploymentUpdateStrategyType:
		err := c.hotPatchControl.Update(pod, oldRevision, updateRevision)
		if err != nil {
			c.recorder.Eventf(deploy, v1.EventTypeWarning, "FailedUpdatePodHotPatch", "failed to update pod %s hot-patch: %v", pod.Name, err)
			return 0, err
		}
		c.recorder.Eventf(deploy, v1.EventTypeNormal, "SuccessfulUpdatePodHotPatch", "successfully update pod %s hot-patch", pod.Name)
		c.updateExp.ExpectUpdated(util.GetControllerKey(deploy), updateRevision.Name, pod)
		return 0, nil
	}

	return 0, fmt.Errorf("invalid update strategy type")
}
