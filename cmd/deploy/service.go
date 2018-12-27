/*
Copyright (c) 2018 TriggerMesh, Inc

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

package deploy

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/watch"

	buildv1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	servingv1alpha1 "github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/triggermesh/tm/pkg/client"
	"github.com/triggermesh/tm/pkg/file"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Knative build timeout in minutes
	timeout           = 10
	uploadDoneTrigger = "/home/.done"
)

type status struct {
	domain string
	err    error
}

// Service represents knative service structure
type Service struct {
	Name           string
	Source         string
	Revision       string
	PullPolicy     string
	ResultImageTag string
	Buildtemplate  string
	RegistrySecret string // Does not belong to the service, need to be deleted
	Env            []string
	Annotations    map[string]string
	Labels         []string
	BuildArgs      []string
	Wait           bool
}

type Config struct {
	servingv1alpha1.Configuration
}

type Route struct {
	servingv1alpha1.Route
}

// Deploy receives Service structure and generate knative/service object to deploy it in knative cluster
func (s *Service) Deploy(clientset *client.ConfigSet) error {
	fmt.Printf("Creating %s function\n", s.Name)
	var clusterBuildtemplate bool
	configuration := servingv1alpha1.ConfigurationSpec{}

	if len(s.Buildtemplate) != 0 {
		if _, err := clientset.Build.BuildV1alpha1().BuildTemplates(clientset.Namespace).Get(s.Buildtemplate, metav1.GetOptions{}); err == nil {
			clusterBuildtemplate = false
		} else if _, err := clientset.Build.BuildV1alpha1().ClusterBuildTemplates().Get(s.Buildtemplate, metav1.GetOptions{}); err == nil {
			clusterBuildtemplate = true
		} else {
			buildtemplate := Buildtemplate{
				Name:           s.Name + "-buildtemplate",
				File:           s.Buildtemplate,
				RegistrySecret: s.RegistrySecret,
			}
			if s.Buildtemplate, err = buildtemplate.Deploy(clientset); err != nil {
				return err
			}
		}
	}

	switch {
	case file.IsLocal(s.Source):
		configuration = s.fromPath()
	case file.IsGit(s.Source):
		if len(s.Revision) == 0 {
			s.Revision = "master"
		}
		configuration = s.fromSource()
	default:
		configuration = s.fromImage()
	}

	if configuration.Build != nil {
		configuration.RevisionTemplate = servingv1alpha1.RevisionTemplateSpec{
			Spec: servingv1alpha1.RevisionSpec{
				Container: corev1.Container{
					Image: fmt.Sprintf("%s/%s-%s:%s", clientset.Registry, clientset.Namespace, s.Name, s.ResultImageTag),
				},
			},
		}
	}

	if len(s.Buildtemplate) != 0 {
		configuration.Build.BuildSpec.Template = &buildv1alpha1.TemplateInstantiationSpec{
			Name:      s.Buildtemplate,
			Arguments: getBuildArguments(fmt.Sprintf("%s/%s-%s", clientset.Registry, clientset.Namespace, s.Name), s.BuildArgs),
		}
		if clusterBuildtemplate {
			configuration.Build.BuildSpec.Template.Kind = buildv1alpha1.ClusterBuildTemplateKind
		}
	}

	configuration.RevisionTemplate.ObjectMeta = metav1.ObjectMeta{
		Name:              s.Name,
		CreationTimestamp: metav1.Time{time.Now()},
		Annotations:       s.Annotations,
		Labels:            getArgsFromSlice(s.Labels),
	}

	envVars := []corev1.EnvVar{
		{
			Name:  "timestamp",
			Value: time.Now().Format("2006-01-02 15:04:05"),
		},
	}
	for k, v := range getArgsFromSlice(s.Env) {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	configuration.RevisionTemplate.Spec.Container.Env = envVars
	configuration.RevisionTemplate.Spec.Container.ImagePullPolicy = corev1.PullPolicy(s.PullPolicy)

	spec := servingv1alpha1.ServiceSpec{
		Manual: &servingv1alpha1.ManualType{},
	}

	serviceObject := servingv1alpha1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "serving.knative.dev/servingv1alpha1",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Labels:    configuration.RevisionTemplate.ObjectMeta.Labels,
			Namespace: clientset.Namespace,
			CreationTimestamp: metav1.Time{
				time.Now(),
			},
		},

		Spec: spec,
	}

	service, err := s.createOrUpdate(serviceObject, clientset)
	if err != nil {
		return err
	}

	c := Config{
		servingv1alpha1.Configuration{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Configuration",
				APIVersion: "serving.knative.dev/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.Name,
				Namespace: clientset.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "serving.knative.dev/v1alpha1", Kind: "Service", Name: s.Name, UID: service.UID},
				},
			},
			Spec: configuration,
		},
	}

	if err := c.createOrUpdate(clientset); err != nil {
		return err
	}

	r := Route{
		servingv1alpha1.Route{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Configuration",
				APIVersion: "serving.knative.dev/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.Name,
				Namespace: clientset.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "serving.knative.dev/v1alpha1", Kind: "Service", Name: s.Name, UID: service.UID},
				},
			},
			Spec: servingv1alpha1.RouteSpec{
				Traffic: []servingv1alpha1.TrafficTarget{
					{Name: "foo", ConfigurationName: s.Name, Percent: 100},
				},
			},
		},
	}

	if err := r.createOrUpdate(clientset); err != nil {
		return err
	}

	if file.IsLocal(s.Source) {
		if err := injectSources(s.Name, path.Dir(s.Source), clientset); err != nil {
			return err
		}
	}
	fmt.Printf("Deployment started. Run \"tm -n %s describe service %s\" to see the details\n", clientset.Namespace, s.Name)

	if s.Wait {
		fmt.Printf("Waiting for %s ready state\n", s.Name)
		domain, err := waitService(s.Name, clientset)
		if err != nil {
			return err
		}
		fmt.Printf("Service %s URL: http://%s\n", s.Name, domain)
	}
	return nil
}

func (s *Service) createOrUpdate(serviceObject servingv1alpha1.Service, clientset *client.ConfigSet) (*servingv1alpha1.Service, error) {
	service, err := clientset.Serving.ServingV1alpha1().Services(clientset.Namespace).Create(&serviceObject)
	if k8sErrors.IsAlreadyExists(err) {
		service, err = clientset.Serving.ServingV1alpha1().Services(clientset.Namespace).Get(serviceObject.ObjectMeta.Name, metav1.GetOptions{})
		if err != nil {
			return service, err
		}
		serviceObject.ObjectMeta.ResourceVersion = service.GetResourceVersion()
		return clientset.Serving.ServingV1alpha1().Services(clientset.Namespace).Update(&serviceObject)
	}
	return service, err
}

func (c *Config) createOrUpdate(clientset *client.ConfigSet) error {
	_, err := clientset.Serving.ServingV1alpha1().Configurations(clientset.Namespace).Create(&c.Configuration)
	if k8sErrors.IsAlreadyExists(err) {
		oldConfig, err := clientset.Serving.ServingV1alpha1().Configurations(clientset.Namespace).Get(c.Configuration.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		c.Configuration.ObjectMeta.ResourceVersion = oldConfig.GetResourceVersion()
		_, err = clientset.Serving.ServingV1alpha1().Configurations(clientset.Namespace).Update(&c.Configuration)
		return err
	}
	return err
}

func (r *Route) createOrUpdate(clientset *client.ConfigSet) error {
	_, err := clientset.Serving.ServingV1alpha1().Routes(clientset.Namespace).Create(&r.Route)
	if k8sErrors.IsAlreadyExists(err) {
		oldRoute, err := clientset.Serving.ServingV1alpha1().Routes(clientset.Namespace).Get(r.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		r.Route.ObjectMeta.ResourceVersion = oldRoute.GetResourceVersion()
		_, err = clientset.Serving.ServingV1alpha1().Routes(clientset.Namespace).Update(&r.Route)
		return err
	}
	return err
}

func (s *Service) fromImage() servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		RevisionTemplate: servingv1alpha1.RevisionTemplateSpec{
			Spec: servingv1alpha1.RevisionSpec{
				Container: corev1.Container{
					Image: s.Source,
				},
			},
		},
	}
}

func (s *Service) fromSource() servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		Build: &servingv1alpha1.RawExtension{
			BuildSpec: &buildv1alpha1.BuildSpec{
				Source: &buildv1alpha1.SourceSpec{
					Git: &buildv1alpha1.GitSourceSpec{
						Url:      s.Source,
						Revision: s.Revision,
					},
				},
			},
		},
	}
}

func (s *Service) fromPath() servingv1alpha1.ConfigurationSpec {
	return servingv1alpha1.ConfigurationSpec{
		Build: &servingv1alpha1.RawExtension{
			BuildSpec: &buildv1alpha1.BuildSpec{
				Source: &buildv1alpha1.SourceSpec{
					Custom: &corev1.Container{
						Image:   "library/busybox",
						Command: []string{"sh"},
						Args: []string{"-c", fmt.Sprintf("while [ -z \"$(ls %s)\" ]; do sleep 1; done; sync; ls -lah /home/; mv /home/%s/* /workspace; sync",
							uploadDoneTrigger, path.Base(path.Dir(s.Source)))},
					},
				},
			},
		},
	}
}

func getArgsFromSlice(slice []string) map[string]string {
	m := make(map[string]string)
	for _, s := range slice {
		t := regexp.MustCompile("[:=]").Split(s, 2)
		if len(t) != 2 {
			fmt.Printf("Can't parse argument slice %s\n", s)
			continue
		}
		m[t[0]] = t[1]
	}
	return m
}

func serviceLatestRevision(name string, clientset *client.ConfigSet) (string, error) {
	var latestRevision string
	for latestRevision == "" {
		service, err := clientset.Serving.ServingV1alpha1().Services(clientset.Namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		latestRevision = service.Status.LatestCreatedRevisionName
		time.Sleep(1 * time.Second)
	}
	return latestRevision, nil
}

func serviceBuildPod(name string, clientset *client.ConfigSet) (string, error) {
	var buildPod string
	for buildPod == "" {
		build, err := clientset.Build.BuildV1alpha1().Builds(clientset.Namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}

		if build.Status.Cluster != nil {
			buildPod = build.Status.Cluster.PodName
		}
		time.Sleep(1 * time.Second)
	}
	return buildPod, nil
}

func injectSources(name string, filepath string, clientset *client.ConfigSet) error {
	latestRevision, err := serviceLatestRevision(name, clientset)
	if err != nil {
		return err
	}
	buildPod, err := serviceBuildPod(latestRevision, clientset)
	if err != nil {
		return err
	}
	res, err := clientset.Core.CoreV1().Pods(clientset.Namespace).Watch(metav1.ListOptions{FieldSelector: "metadata.name=" + buildPod})
	if err != nil {
		return err
	}
	defer res.Stop()

	var sourceContainer string
	for sourceContainer == "" {
		e := <-res.ResultChan()
		pod := e.Object.(*corev1.Pod)

		for _, v := range pod.Status.InitContainerStatuses {
			if v.Name == "build-step-custom-source" {
				if v.State.Terminated != nil {
					// Looks like we got watch interface for "previous" service version, updating
					if latestRevision, err = serviceLatestRevision(name, clientset); err != nil {
						return err
					}
					if buildPod, err = serviceBuildPod(latestRevision, clientset); err != nil {
						return err
					}
					if res, err = clientset.Core.CoreV1().Pods(clientset.Namespace).Watch(metav1.ListOptions{FieldSelector: "metadata.name=" + buildPod}); err != nil {
						return err
					}
					break
				}
				if v.State.Running != nil {
					sourceContainer = v.Name
					break
				}
			}
		}
	}

	c := file.Copy{
		Pod:         buildPod,
		Container:   sourceContainer,
		Source:      filepath,
		Destination: "/home",
	}
	if err := c.Upload(clientset); err != nil {
		return err
	}

	if _, _, err := c.RemoteExec(clientset, "touch "+uploadDoneTrigger, nil); err != nil {
		return err
	}

	return nil
}

func watchService(service string, clientset *client.ConfigSet) (watch.Interface, error) {
	return clientset.Serving.ServingV1alpha1().Services(clientset.Namespace).Watch(metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", service),
	})
}

func waitService(service string, clientset *client.ConfigSet) (string, error) {
	quit := time.After(timeout * time.Minute)
	res, err := watchService(service, clientset)
	if err != nil {
		return "", err
	}
	defer res.Stop()

	firstError := true
	for {
		select {
		case <-quit:
			return "", errors.New("Service status wait timeout")
		case event := <-res.ResultChan():
			if event.Object == nil {
				if res, err = watchService(service, clientset); err != nil {
					return "", err
				}
				break
			}
			serviceEvent, ok := event.Object.(*servingv1alpha1.Service)
			if ok {
				if serviceEvent.Status.IsReady() {
					return serviceEvent.Status.Domain, nil
				}
				for _, v := range serviceEvent.Status.Conditions {
					if v.IsFalse() {
						if v.Reason == "RevisionFailed" && firstError {
							time.Sleep(time.Second * 3)
							if res, err = watchService(service, clientset); err != nil {
								return "", err
							}
							firstError = false
							break
						}
						return "", errors.New(v.Message)
					}
				}
			}
		}
	}
	return "", nil
}
