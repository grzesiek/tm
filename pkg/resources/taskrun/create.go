// Copyright 2018 TriggerMesh, Inc
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

package taskrun

import (
	tektonV1alpha1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/triggermesh/tm/pkg/client"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (tr *TaskRun) Deploy(clientset *client.ConfigSet) error {
	taskRunObject := tr.newObject(clientset)
	return t.createOrUpdate(taskRunObject, clientset)
}

func (tr *TaskRun) newObject(clientset *client.ConfigSet) tektonV1alpha1.TaskRun {
	return tektonV1alpha1.TaskRun{
		TypeMeta: metav1.TypeMeta{
			Kind:       "TaskRun",
			APIVersion: "",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.Name,
			Namespace: t.Namespace,
		},
		Spec: tektonV1alpha1.TaskRunSpec{},
		Status: tektonV1alpha1.TaskRunStatus{},
	}
}

func (tr *TaskRun) createOrUpdate(taskRunObject tektonV1alpha1.TaskRun, clientset *client.ConfigSet) error {
	_, err := clientset.Tekton.TektonV1alpha1().TaskRuns(tr.Namespace).Create(&taskRunObject)
	if k8sErrors.IsAlreadyExists(err) {
		taskRun, err := clientset.Tekton.TektonV1alpha1().TaskRuns(tr.Namespace).Get(taskRunObject.ObjectMeta.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		taskRunObject.ObjectMeta.ResourceVersion = taskRun.GetResourceVersion()
		_, err = clientset.Tekton.TektonV1alpha1().TaskRuns(t.Namespace).Update(&taskRunObject)
	}
	return err
}
