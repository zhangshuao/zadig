/*
Copyright 2022 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jobcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	crClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	"github.com/koderover/zadig/pkg/setting"
	kubeclient "github.com/koderover/zadig/pkg/shared/kube/client"
	"github.com/koderover/zadig/pkg/shared/kube/wrapper"
	krkubeclient "github.com/koderover/zadig/pkg/tool/kube/client"
	"github.com/koderover/zadig/pkg/tool/kube/getter"
	"github.com/koderover/zadig/pkg/tool/kube/updater"
	"github.com/pkg/errors"
)

type DeployJobCtl struct {
	job         *commonmodels.JobTask
	namespace   string
	workflowCtx *commonmodels.WorkflowTaskCtx
	logger      *zap.SugaredLogger
	kubeClient  crClient.Client
	restConfig  *rest.Config
	jobTaskSpec *commonmodels.JobTaskDeploySpec
	ack         func()
}

func NewDeployJobCtl(job *commonmodels.JobTask, workflowCtx *commonmodels.WorkflowTaskCtx, ack func(), logger *zap.SugaredLogger) *DeployJobCtl {
	jobTaskSpec := &commonmodels.JobTaskDeploySpec{}
	if err := commonmodels.IToi(job.Spec, jobTaskSpec); err != nil {
		logger.Error(err)
	}
	return &DeployJobCtl{
		job:         job,
		workflowCtx: workflowCtx,
		logger:      logger,
		ack:         ack,
		jobTaskSpec: jobTaskSpec,
	}
}

func (c *DeployJobCtl) Run(ctx context.Context) {
	if err := c.run(ctx); err != nil {
		return
	}
	if c.jobTaskSpec.SkipCheckRunStatus {
		c.job.Status = config.StatusPassed
		return
	}
	c.wait(ctx)
}

func (c *DeployJobCtl) run(ctx context.Context) error {
	var (
		err      error
		replaced = false
	)
	env, err := commonrepo.NewProductColl().Find(&commonrepo.ProductFindOptions{
		Name:    c.workflowCtx.ProjectName,
		EnvName: c.jobTaskSpec.Env,
	})
	if err != nil {
		msg := fmt.Sprintf("find project error: %v", err)
		c.logger.Error(msg)
		c.job.Status = config.StatusFailed
		c.job.Error = msg
		return errors.New(msg)
	}
	c.namespace = env.Namespace
	c.jobTaskSpec.ClusterID = env.ClusterID

	if c.jobTaskSpec.ClusterID != "" {
		c.restConfig, err = kubeclient.GetRESTConfig(config.HubServerAddress(), c.jobTaskSpec.ClusterID)
		if err != nil {
			msg := fmt.Sprintf("can't get k8s rest config: %v", err)
			c.logger.Error(msg)
			c.job.Status = config.StatusFailed
			c.job.Error = msg
			return errors.New(msg)
		}

		c.kubeClient, err = kubeclient.GetKubeClient(config.HubServerAddress(), c.jobTaskSpec.ClusterID)
		if err != nil {
			msg := fmt.Sprintf("can't init k8s client: %v", err)
			c.logger.Error(msg)
			c.job.Status = config.StatusFailed
			c.job.Error = msg
			return errors.New(msg)
		}
	} else {
		c.kubeClient = krkubeclient.Client()
		c.restConfig = krkubeclient.RESTConfig()
	}

	// get servcie info
	var (
		serviceInfo *commonmodels.Service
		selector    labels.Selector
	)
	serviceInfo, err = commonrepo.NewServiceColl().Find(
		&commonrepo.ServiceFindOption{
			ServiceName:   c.jobTaskSpec.ServiceName,
			ProductName:   c.workflowCtx.ProjectName,
			ExcludeStatus: setting.ProductStatusDeleting,
			Type:          c.jobTaskSpec.ServiceType,
		})
	if err != nil {
		// Maybe it is a share service, the entity is not under the project
		serviceInfo, err = commonrepo.NewServiceColl().Find(
			&commonrepo.ServiceFindOption{
				ServiceName:   c.jobTaskSpec.ServiceName,
				ExcludeStatus: setting.ProductStatusDeleting,
				Type:          c.jobTaskSpec.ServiceType,
			})
		if err != nil {
			msg := fmt.Sprintf("find service %s error: %v", c.jobTaskSpec.ServiceName, err)
			c.logger.Error(msg)
			c.job.Status = config.StatusFailed
			c.job.Error = msg
			return errors.New(msg)
		}
	}

	if serviceInfo.WorkloadType == "" {
		selector = labels.Set{setting.ProductLabel: c.workflowCtx.ProjectName, setting.ServiceLabel: c.jobTaskSpec.ServiceName}.AsSelector()

		var deployments []*appsv1.Deployment
		deployments, err = getter.ListDeployments(env.Namespace, selector, c.kubeClient)
		if err != nil {
			c.logger.Error(err)
			c.job.Status = config.StatusFailed
			c.job.Error = err.Error()
			return err
		}

		var statefulSets []*appsv1.StatefulSet
		statefulSets, err = getter.ListStatefulSets(env.Namespace, selector, c.kubeClient)
		if err != nil {
			c.logger.Error(err)
			c.job.Status = config.StatusFailed
			c.job.Error = err.Error()
			return err
		}

	L:
		for _, deploy := range deployments {
			for _, container := range deploy.Spec.Template.Spec.Containers {
				if container.Name == c.jobTaskSpec.ServiceModule {
					err = updater.UpdateDeploymentImage(deploy.Namespace, deploy.Name, c.jobTaskSpec.ServiceModule, c.jobTaskSpec.Image, c.kubeClient)
					if err != nil {
						msg := fmt.Sprintf("failed to update container image in %s/deployments/%s/%s: %v", env.Namespace, deploy.Name, container.Name, err)
						c.logger.Error(msg)
						c.job.Status = config.StatusFailed
						c.job.Error = msg
						return errors.New(msg)
					}
					c.jobTaskSpec.ReplaceResources = append(c.jobTaskSpec.ReplaceResources, commonmodels.Resource{
						Kind:      setting.Deployment,
						Container: container.Name,
						Origin:    container.Image,
						Name:      deploy.Name,
					})
					replaced = true
					break L
				}
			}
		}
	Loop:
		for _, sts := range statefulSets {
			for _, container := range sts.Spec.Template.Spec.Containers {
				if container.Name == c.jobTaskSpec.ServiceModule {
					err = updater.UpdateStatefulSetImage(sts.Namespace, sts.Name, c.jobTaskSpec.ServiceModule, c.jobTaskSpec.Image, c.kubeClient)
					if err != nil {
						msg := fmt.Sprintf("failed to update container image in %s/statefulsets/%s/%s: %v", env.Namespace, sts.Name, container.Name, err)
						c.logger.Error(msg)
						c.job.Status = config.StatusFailed
						c.job.Error = msg
						return errors.New(msg)
					}
					c.jobTaskSpec.ReplaceResources = append(c.jobTaskSpec.ReplaceResources, commonmodels.Resource{
						Kind:      setting.StatefulSet,
						Container: container.Name,
						Origin:    container.Image,
						Name:      sts.Name,
					})
					replaced = true
					break Loop
				}
			}
		}
	} else {
		switch serviceInfo.WorkloadType {
		case setting.StatefulSet:
			var statefulSet *appsv1.StatefulSet
			statefulSet, _, err = getter.GetStatefulSet(env.Namespace, c.jobTaskSpec.ServiceName, c.kubeClient)
			if err != nil {
				return err
			}
			for _, container := range statefulSet.Spec.Template.Spec.Containers {
				if container.Name == c.jobTaskSpec.ServiceModule {
					err = updater.UpdateStatefulSetImage(statefulSet.Namespace, statefulSet.Name, c.jobTaskSpec.ServiceModule, c.jobTaskSpec.Image, c.kubeClient)
					if err != nil {
						msg := fmt.Sprintf("failed to update container image in %s/statefulsets/%s/%s: %v", env.Namespace, statefulSet.Name, container.Name, err)
						c.logger.Error(msg)
						c.job.Status = config.StatusFailed
						c.job.Error = msg
						return errors.New(msg)
					}
					c.jobTaskSpec.ReplaceResources = append(c.jobTaskSpec.ReplaceResources, commonmodels.Resource{
						Kind:      setting.StatefulSet,
						Container: container.Name,
						Origin:    container.Image,
						Name:      statefulSet.Name,
					})
					replaced = true
					break
				}
			}
		case setting.Deployment:
			var deployment *appsv1.Deployment
			deployment, _, err = getter.GetDeployment(env.Namespace, c.jobTaskSpec.ServiceName, c.kubeClient)
			if err != nil {
				return err
			}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name == c.jobTaskSpec.ServiceModule {
					err = updater.UpdateDeploymentImage(deployment.Namespace, deployment.Name, c.jobTaskSpec.ServiceModule, c.jobTaskSpec.Image, c.kubeClient)
					if err != nil {
						msg := fmt.Sprintf("failed to update container image in %s/deployments/%s/%s: %v", env.Namespace, deployment.Name, container.Name, err)
						c.logger.Error(msg)
						c.job.Status = config.StatusFailed
						c.job.Error = msg
						return errors.New(msg)
					}
					c.jobTaskSpec.ReplaceResources = append(c.jobTaskSpec.ReplaceResources, commonmodels.Resource{
						Kind:      setting.Deployment,
						Container: container.Name,
						Origin:    container.Image,
						Name:      deployment.Name,
					})
					replaced = true
					break
				}
			}
		}
	}
	if !replaced {
		msg := fmt.Sprintf("service %s container name %s is not found in env %s", c.jobTaskSpec.ServiceName, c.jobTaskSpec.ServiceModule, c.jobTaskSpec.Env)
		c.logger.Error(msg)
		c.job.Status = config.StatusFailed
		c.job.Error = msg
		return errors.New(msg)
	}
	c.job.Spec = c.jobTaskSpec
	return nil
}

func (c *DeployJobCtl) wait(ctx context.Context) {
	timeout := time.After(time.Duration(c.timeout()) * time.Second)

	selector := labels.Set{setting.ProductLabel: c.workflowCtx.ProjectName, setting.ServiceLabel: c.jobTaskSpec.ServiceName}.AsSelector()

	for {
		select {
		case <-ctx.Done():
			c.job.Status = config.StatusCancelled
			return

		case <-timeout:
			pods, err := getter.ListPods(c.namespace, selector, c.kubeClient)
			if err != nil {
				msg := fmt.Sprintf("list pods error: %v", err)
				c.logger.Error(msg)
				c.job.Status = config.StatusFailed
				c.job.Error = msg
				return
			}

			var msg []string
			for _, pod := range pods {
				podResource := wrapper.Pod(pod).Resource()
				if podResource.Status != setting.StatusRunning && podResource.Status != setting.StatusSucceeded {
					for _, cs := range podResource.ContainerStatuses {
						// message为空不认为是错误状态，有可能还在waiting
						if cs.Message != "" {
							msg = append(msg, fmt.Sprintf("Status: %s, Reason: %s, Message: %s", cs.Status, cs.Reason, cs.Message))
						}
					}
				}
			}

			if len(msg) != 0 {
				err = errors.New(strings.Join(msg, "\n"))
				c.logger.Error(err)
				c.job.Status = config.StatusFailed
				c.job.Error = err.Error()
				return
			}
			c.job.Status = config.StatusTimeout
			return

		default:
			time.Sleep(time.Second * 2)
			ready := true
			var err error
		L:
			for _, resource := range c.jobTaskSpec.ReplaceResources {
				switch resource.Kind {
				case setting.Deployment:
					d, found, e := getter.GetDeployment(c.namespace, resource.Name, c.kubeClient)
					if e != nil {
						err = e
					}
					if e != nil || !found {
						c.logger.Errorf(
							"failed to check deployment ready status %s/%s/%s - %v",
							c.namespace,
							resource.Kind,
							resource.Name,
							e,
						)
						ready = false
					} else {
						ready = wrapper.Deployment(d).Ready()
					}

					if !ready {
						break L
					}
				case setting.StatefulSet:
					st, found, e := getter.GetStatefulSet(c.namespace, resource.Name, c.kubeClient)
					if e != nil {
						err = e
					}
					if err != nil || !found {
						c.logger.Errorf(
							"failed to check statefulSet ready status %s/%s/%s",
							c.namespace,
							resource.Kind,
							resource.Name,
							e,
						)
						ready = false
					} else {
						ready = wrapper.StatefulSet(st).Ready()
					}

					if !ready {
						break L
					}
				}
			}

			if ready {
				c.job.Status = config.StatusPassed
				return
			}
		}
	}
}

func (c *DeployJobCtl) timeout() int {
	if c.jobTaskSpec.Timeout == 0 {
		c.jobTaskSpec.Timeout = setting.DeployTimeout
	}
	return c.jobTaskSpec.Timeout
}
