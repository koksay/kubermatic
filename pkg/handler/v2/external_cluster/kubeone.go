/*
Copyright 2022 The Kubermatic Kubernetes Platform contributors.

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

package externalcluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/go-autorest/autorest/to"

	"github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1"
	clusterv1alpha1 "github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1"
	kubeonev1beta2 "k8c.io/kubeone/pkg/apis/kubeone/v1beta2"
	apiv1 "k8c.io/kubermatic/v2/pkg/api/v1"
	apiv2 "k8c.io/kubermatic/v2/pkg/api/v2"
	kubermaticv1 "k8c.io/kubermatic/v2/pkg/apis/kubermatic/v1"
	v1 "k8c.io/kubermatic/v2/pkg/apis/kubermatic/v1"
	"k8c.io/kubermatic/v2/pkg/handler/v1/common"
	kuberneteshelper "k8c.io/kubermatic/v2/pkg/kubernetes"
	"k8c.io/kubermatic/v2/pkg/provider"
	"k8c.io/kubermatic/v2/pkg/resources"
	"k8c.io/kubermatic/v2/pkg/util/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	NodeControlPlaneLabel = "node-role.kubernetes.io/control-plane"
)

func importKubeOneCluster(ctx context.Context, name string, userInfoGetter func(ctx context.Context, projectID string) (*provider.UserInfo, error), project *kubermaticv1.Project, cloud *apiv2.ExternalClusterCloudSpec, clusterProvider provider.ExternalClusterProvider, privilegedClusterProvider provider.PrivilegedExternalClusterProvider) (*kubermaticv1.ExternalCluster, error) {
	kubeOneCluster, err := DecodeManifestFromKubeOneReq(cloud.KubeOne.Manifest)
	if err != nil {
		return nil, err
	}

	newCluster := genExternalCluster(kubeOneCluster.Name, project.Name)
	newCluster.Spec.CloudSpec = &kubermaticv1.ExternalClusterCloudSpec{
		KubeOne: &kubermaticv1.ExternalClusterKubeOneCloudSpec{},
	}

	err = clusterProvider.CreateKubeOneClusterNamespace(ctx, newCluster)
	if err != nil {
		return nil, common.KubernetesErrorToHTTPError(err)
	}
	kuberneteshelper.AddFinalizer(newCluster, apiv1.ExternalClusterKubeOneNamespaceCleanupFinalizer)

	err = clusterProvider.CreateOrUpdateKubeOneSSHSecret(ctx, cloud.KubeOne.SSHKey, newCluster)
	if err != nil {
		return nil, common.KubernetesErrorToHTTPError(err)
	}
	err = clusterProvider.CreateOrUpdateKubeOneManifestSecret(ctx, cloud.KubeOne.Manifest, newCluster)
	if err != nil {
		return nil, common.KubernetesErrorToHTTPError(err)
	}

	err = clusterProvider.CreateOrUpdateKubeOneCredentialSecret(ctx, *cloud.KubeOne.CloudSpec, newCluster)
	if err != nil {
		return nil, common.KubernetesErrorToHTTPError(err)
	}

	newCluster.Spec.CloudSpec.KubeOne.ClusterStatus.Status = kubermaticv1.StatusProvisioning
	return createNewCluster(ctx, userInfoGetter, clusterProvider, privilegedClusterProvider, newCluster, project)
}

func patchKubeOneCluster(ctx context.Context,
	cluster *kubermaticv1.ExternalCluster,
	oldCluster *apiv2.ExternalCluster,
	newCluster *apiv2.ExternalCluster,
	secretKeySelector provider.SecretKeySelectorValueFunc,
	clusterProvider provider.ExternalClusterProvider,
	masterClient ctrlruntimeclient.Client) (*apiv2.ExternalCluster, error) {
	kubeOneSpec := cluster.Spec.CloudSpec.KubeOne
	operation := kubeOneSpec.ClusterStatus.Status
	if operation == kubermaticv1.StatusReconciling {
		return nil, errors.NewBadRequest("Operation is not allowed: Another operation: (%s) is in progress, please wait for it to finish before starting a new operation.", operation)
	}

	if oldCluster.Spec.Version != newCluster.Spec.Version {
		return UpgradeKubeOneCluster(ctx, cluster, oldCluster, newCluster, clusterProvider, masterClient)
	}
	if oldCluster.Cloud.KubeOne.ContainerRuntime != newCluster.Cloud.KubeOne.ContainerRuntime {
		if oldCluster.Cloud.KubeOne.ContainerRuntime == resources.ContainerRuntimeDocker {
			return MigrateKubeOneToContainerd(ctx, cluster, oldCluster, newCluster, clusterProvider, masterClient)
		} else {
			return nil, fmt.Errorf("Operation not supported: only migration from docker to containerd is supported: %s", oldCluster.Cloud.KubeOne.ContainerRuntime)
		}
	}

	return newCluster, nil
}

func UpgradeKubeOneCluster(ctx context.Context,
	externalCluster *kubermaticv1.ExternalCluster,
	oldCluster *apiv2.ExternalCluster,
	newCluster *apiv2.ExternalCluster,
	externalClusterProvider provider.ExternalClusterProvider,
	masterClient ctrlruntimeclient.Client,
) (*apiv2.ExternalCluster, error) {
	manifest := externalCluster.Spec.CloudSpec.KubeOne.ManifestReference

	manifestSecret := &corev1.Secret{}
	if err := masterClient.Get(ctx, types.NamespacedName{Namespace: manifest.Namespace, Name: manifest.Name}, manifestSecret); err != nil {
		return nil, errors.NewBadRequest(fmt.Sprintf("can not retrieve kubeone manifest secret: %v", err))
	}
	currentManifest := manifestSecret.Data[resources.KubeOneManifest]

	cluster := &kubeonev1beta2.KubeOneCluster{}
	if err := yaml.UnmarshalStrict(currentManifest, cluster); err != nil {
		return nil, fmt.Errorf("failed to decode manifest secret data: %w", err)
	}
	upgradeVersion := newCluster.Spec.Version.Semver().String()
	cluster.Versions = kubeonev1beta2.VersionConfig{
		Kubernetes: upgradeVersion,
	}

	if oldCluster.Cloud.KubeOne.ContainerRuntime == resources.ContainerRuntimeDocker {
		cluster.ContainerRuntime.Containerd = nil
		if upgradeVersion >= "1.24" {
			return nil, errors.NewBadRequest("container runtime is \"docker\". Support for docker will be removed with Kubernetes 1.24 release.")
		} else if cluster.ContainerRuntime.Docker == nil {
			cluster.ContainerRuntime.Docker = &kubeonev1beta2.ContainerRuntimeDocker{}
		}
	}

	patchManifest, err := yaml.Marshal(cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to encode kubeone cluster manifest config as YAML: %w", err)
	}

	oldManifestSecret := manifestSecret.DeepCopy()
	manifestSecret.Data = map[string][]byte{
		resources.KubeOneManifest: patchManifest,
	}
	if err := masterClient.Patch(ctx, manifestSecret, ctrlruntimeclient.MergeFrom(oldManifestSecret)); err != nil {
		return nil, fmt.Errorf("failed to update kubeone manifest secret for upgrade version %s/%s: %w", manifest.Name, manifest.Namespace, err)
	}

	// update api externalcluster status.
	newCluster.Status.State = apiv2.RECONCILING
	return newCluster, nil
}

func MigrateKubeOneToContainerd(ctx context.Context,
	externalCluster *kubermaticv1.ExternalCluster,
	oldCluster *apiv2.ExternalCluster,
	newCluster *apiv2.ExternalCluster,
	externalClusterProvider provider.ExternalClusterProvider,
	masterClient ctrlruntimeclient.Client,
) (*apiv2.ExternalCluster, error) {
	kubeOneSpec := externalCluster.Spec.CloudSpec.KubeOne
	manifest := kubeOneSpec.ManifestReference
	wantedContainerRuntime := newCluster.Cloud.KubeOne.ContainerRuntime

	if kubeOneSpec.ClusterStatus.Status == kubermaticv1.StatusReconciling {
		return nil, errors.NewBadRequest("Operation is not allowed: Another operation: (Upgrading) is in progress, please wait for it to finish before starting a new operation.")
	}

	// currently only migration to containerd is supported
	if !sets.NewString("containerd").Has(wantedContainerRuntime) {
		return nil, fmt.Errorf("Operation not supported: Only migration from docker to containerd is supported: %s", wantedContainerRuntime)
	}

	manifestSecret := &corev1.Secret{}
	if err := masterClient.Get(ctx, types.NamespacedName{Namespace: manifest.Namespace, Name: manifest.Name}, manifestSecret); err != nil {
		return nil, errors.NewBadRequest(fmt.Sprintf("can not retrieve kubeone manifest secret: %v", err))
	}
	currentManifest := manifestSecret.Data[resources.KubeOneManifest]
	cluster := &kubeonev1beta2.KubeOneCluster{}
	if err := yaml.UnmarshalStrict(currentManifest, cluster); err != nil {
		return nil, fmt.Errorf("failed to decode manifest secret data: %w", err)
	}
	cluster.ContainerRuntime.Docker = nil
	cluster.ContainerRuntime.Containerd = &kubeonev1beta2.ContainerRuntimeContainerd{}

	patchManifest, err := yaml.Marshal(cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to encode kubeone cluster manifest config as YAML: %w", err)
	}

	oldManifestSecret := manifestSecret.DeepCopy()
	manifestSecret.Data = map[string][]byte{
		resources.KubeOneManifest: patchManifest,
	}
	if err := masterClient.Patch(ctx, manifestSecret, ctrlruntimeclient.MergeFrom(oldManifestSecret)); err != nil {
		return nil, fmt.Errorf("failed to update kubeone manifest secret for container-runtime containerd %s/%s: %w", manifest.Name, manifest.Namespace, err)
	}

	// update api externalcluster status.
	newCluster.Status = apiv2.ExternalClusterStatus{State: apiv2.RECONCILING}

	return newCluster, nil
}

func checkContainerRuntime(ctx context.Context,
	externalCluster *kubermaticv1.ExternalCluster,
	externalClusterProvider provider.ExternalClusterProvider,
) (string, error) {
	nodes, err := externalClusterProvider.ListNodes(ctx, externalCluster)
	if err != nil {
		return "", fmt.Errorf("Failed to fetch container runtime: not able to list nodes %w", err)
	}
	for _, node := range nodes.Items {
		if _, ok := node.Labels[NodeControlPlaneLabel]; ok {
			containerRuntimeVersion := node.Status.NodeInfo.ContainerRuntimeVersion
			strSlice := strings.Split(containerRuntimeVersion, ":")
			for _, containerRuntime := range strSlice {
				return containerRuntime, nil
			}
		}
	}
	return "", fmt.Errorf("Failed to fetch container runtime: no control plane nodes found with label %s", NodeControlPlaneLabel)
}

func createAPIMachineDeployment(md *clusterv1alpha1.MachineDeployment) *apiv2.ExternalClusterMachineDeployment {
	apimd := apiv2.ExternalClusterMachineDeployment{
		NodeDeployment: apiv1.NodeDeployment{
			ObjectMeta: apiv1.ObjectMeta{
				ID:   md.Name,
				Name: md.Name,
			},
			Spec: apiv1.NodeDeploymentSpec{
				Replicas: to.Int32(md.Spec.Replicas),
				Template: apiv1.NodeSpec{
					Versions: apiv1.NodeVersionInfo{
						Kubelet: to.String(&md.Spec.Template.Spec.Versions.Kubelet),
					},
				},
			},
			Status: clusterv1alpha1.MachineDeploymentStatus{
				Replicas:      md.Status.Replicas,
				ReadyReplicas: md.Status.ReadyReplicas,
			},
		},
	}

	return &apimd
}

func getKubeOneMachineDeployment(ctx context.Context, mdName string, cluster *v1.ExternalCluster, clusterProvider provider.ExternalClusterProvider) (*clusterv1alpha1.MachineDeployment, error) {
	machineDeployment := &clusterv1alpha1.MachineDeployment{}
	userClusterClient, err := clusterProvider.GetClient(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if err := userClusterClient.Get(ctx, types.NamespacedName{Name: mdName, Namespace: metav1.NamespaceSystem}, machineDeployment); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("failed to get MachineDeployment: %w", err)
	}
	return machineDeployment, nil
}

func patchKubeOneMachineDeployment(ctx context.Context, machineDeployment *v1alpha1.MachineDeployment, oldmd, newmd *apiv2.ExternalClusterMachineDeployment, cluster *v1.ExternalCluster, clusterProvider provider.ExternalClusterProvider) (*apiv2.ExternalClusterMachineDeployment, error) {
	currentVersion := oldmd.NodeDeployment.Spec.Template.Versions.Kubelet
	desiredVersion := newmd.NodeDeployment.Spec.Template.Versions.Kubelet
	if desiredVersion != currentVersion {
		machineDeployment.Spec.Template.Spec.Versions.Kubelet = newmd.NodeDeployment.Spec.Template.Versions.Kubelet
		userClusterClient, err := clusterProvider.GetClient(ctx, cluster)
		if err != nil {
			return nil, err
		}
		if err := userClusterClient.Update(ctx, machineDeployment); err != nil && !meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("failed to update MachineDeployment: %w", err)
		}
		return newmd, nil
	}

	return oldmd, nil
}
