// Copyright 2020 Authors of Cilium
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

package helpers

import (
	"fmt"
	"sync"
	"time"

	ginkgoext "github.com/cilium/cilium/test/ginkgo-ext"
)

// Manifest represents a deployment manifest that can consist of an any number
// of Deployments, DaemonSets, Pods, etc.
type Manifest struct {
	Filename      string
	NumDaemonSet  int
	NumPods       int
	LabelSelector string
}

// Deploy deploys the manifest. It will call ginkgoext.Fail() if any aspect of
// that fails.
func (m Manifest) Deploy(kubectl *Kubectl, namespace string) *Deployment {
	deploy, err := m.deploy(kubectl, namespace)
	if err != nil {
		ginkgoext.Failf("Unable to deploy manifest %s: %s", m.Filename, err)
	}

	return deploy
}

func (m Manifest) deploy(kubectl *Kubectl, namespace string) (*Deployment, error) {
	ginkgoext.By("Deploying %s in namespace %s", m.Filename, namespace)

	numNodes := kubectl.GetNumCiliumNodes()
	if numNodes == 0 {
		return nil, fmt.Errorf("No available nodes to deploy")
	}

	path := ManifestGet(kubectl.BasePath(), m.Filename)
	res := kubectl.Apply(ApplyOptions{Namespace: namespace, FilePath: path})
	if !res.WasSuccessful() {
		return nil, fmt.Errorf("Unable to deploy manifest %s: %s", path, res.CombineOutput().String())
	}

	d := &Deployment{
		manifest:  m,
		kubectl:   kubectl,
		numNodes:  numNodes,
		namespace: namespace,
		path:      path,
	}

	return d, nil
}

// Deployment is a deployed manifest. The deployment binds the manifest to a
// particular namespace and records the number of nodes the deployment is
// spread over.
type Deployment struct {
	kubectl   *Kubectl
	manifest  Manifest
	numNodes  int
	namespace string
	path      string
}

// numExpectedPods returns the number of expected pods the deployment resulted
// in
func (d *Deployment) numExpectedPods() int {
	return (d.numNodes * d.manifest.NumDaemonSet) + d.manifest.NumPods
}

// WaitUntilReady waits until all pods of the deployment are up and in ready
// state
func (d *Deployment) WaitUntilReady() {
	expectedPods := d.numExpectedPods()
	if expectedPods == 0 {
		return
	}

	ginkgoext.By("Waiting for %s for %d pods of deployment %s to become ready",
		HelperTimeout, expectedPods, d.manifest.Filename)

	if err := d.kubectl.WaitforNPods(d.namespace, "", expectedPods, HelperTimeout); err != nil {
		ginkgoext.Failf("Pods are not ready in time: %s", err)
	}
}

// Delete deletes the deployment
func (d *Deployment) Delete() {
	ginkgoext.By("Deleting deployment %s", d.manifest.Filename)
	d.kubectl.Delete(d.path)
}

// DeploymentManager manages a set of deployments
type DeploymentManager struct {
	kubectl          *Kubectl
	deployments      []*Deployment
	randomNamespaces map[string]struct{}
	ciliumDeployed   bool
}

// NewDeploymentManager returns a new deployment manager
func NewDeploymentManager() *DeploymentManager {
	return &DeploymentManager{
		randomNamespaces: map[string]struct{}{},
	}
}

// Deploy deploys a manifest using the deployment manager and stores the
// deployment in the manager
func (m *DeploymentManager) Deploy(kubectl *Kubectl, namespace string, manifest Manifest) {
	namespaceCreated := false

	m.kubectl = kubectl
	if namespace != "default" {
		if _, ok := m.randomNamespaces[namespace]; !ok {
			res := m.kubectl.NamespaceCreate(namespace)
			if !res.WasSuccessful() {
				ginkgoext.Failf("Unable to create namespace %s: %s", namespace, 1)
			}
			namespaceCreated = true
		}
	}

	d, err := manifest.deploy(kubectl, namespace)
	if err != nil {
		if namespaceCreated {
			ginkgoext.By("Deleting namespace %s", namespace)
			m.kubectl.NamespaceDelete(namespace)
		}

		ginkgoext.Failf("Unable to deploy manifest %s: %s", manifest.Filename, err)
	}

	if namespace != "default" {
		m.randomNamespaces[namespace] = struct{}{}
	}

	m.add(d)
}

func (m *DeploymentManager) add(d *Deployment) {
	m.deployments = append(m.deployments, d)
}

// DeleteAll deletes all deployments which have previously been deployed using
// this deployment manager
func (m *DeploymentManager) DeleteAll() {
	var (
		deleted = 0
		wg      sync.WaitGroup
	)

	wg.Add(len(m.deployments))
	for _, d := range m.deployments {
		// Issue all delete triggers in parallel
		go func(d *Deployment) {
			d.Delete()
			wg.Done()
		}(d)
		deleted++
	}
	wg.Wait()
	m.deployments = nil

	wg.Add(len(m.randomNamespaces))
	for namespace := range m.randomNamespaces {
		ginkgoext.By("Deleting namespace %s", namespace)
		go func(namespace string) {
			m.kubectl.NamespaceDelete(namespace)
			wg.Done()
		}(namespace)
	}

	m.randomNamespaces = map[string]struct{}{}

	if deleted > 0 {
		m.kubectl.WaitCleanAllTerminatingPods(2 * time.Minute)
	}
	wg.Wait()
}

func (m *DeploymentManager) DeleteCilium() {
	if m.ciliumDeployed {
		m.kubectl.DeleteCiliumDS()
		m.kubectl.WaitCleanAllTerminatingPods(2 * time.Minute)
	}
}

// WaitUntilReady waits until all deployments managed by this manager are up
// and ready
func (m *DeploymentManager) WaitUntilReady() {
	for _, d := range m.deployments {
		d.WaitUntilReady()
	}
}

type CiliumDeployFunc func(kubectl *Kubectl, ciliumFilename string, options map[string]string)

func (m *DeploymentManager) DeployCilium(kubectl *Kubectl, options map[string]string, deploy CiliumDeployFunc) {
	deploy(kubectl, TimestampFilename("cilium.yaml"), options)

	_, err := kubectl.CiliumNodesWait()
	if err != nil {
		ginkgoext.Failf("Kubernetes nodes were not annotated by Cilium")
	}

	ginkgoext.By("Making sure all endpoints are in ready state")
	if err = kubectl.CiliumEndpointWaitReady(); err != nil {
		ginkgoext.Failf("Failure while waiting for all cilium endpoints to reach ready state: %s", err)
	}

	m.ciliumDeployed = true
}
