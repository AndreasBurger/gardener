// Copyright 2018 The Gardener Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package quotavalidator

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gardener/gardener/pkg/apis/garden"
	"github.com/gardener/gardener/pkg/apis/garden/helper"
	admissioninitializer "github.com/gardener/gardener/pkg/apiserver/admission/initializer"
	informers "github.com/gardener/gardener/pkg/client/garden/informers/internalversion"
	listers "github.com/gardener/gardener/pkg/client/garden/listers/garden/internalversion"
	"github.com/gardener/gardener/pkg/operation/common"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/admission"
)

const (
	// PluginName is the name of this admission plugin.
	PluginName = "ShootQuotaValidator"
)

var (
	quotaMetricNames = [6]v1.ResourceName{
		garden.QuotaMetricCPU,
		garden.QuotaMetricGPU,
		garden.QuotaMetricMemory,
		garden.QuotaMetricStorageStandard,
		garden.QuotaMetricStoragePremium,
		garden.QuotaMetricLoadbalancer}
)

type quotaWorker struct {
	garden.Worker
	// VolumeType is the type of the root volumes.
	VolumeType string
	// VolumeSize is the size of the root volume.
	VolumeSize resource.Quantity
}

// Register registers a plugin.
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return New()
	})
}

// RejectShootIfQuotaExceeded contains listers and and admission handler.
type RejectShootIfQuotaExceeded struct {
	*admission.Handler
	shootLister        listers.ShootLister
	cloudProfileLister listers.CloudProfileLister
	crossSBLister      listers.CrossSecretBindingLister
	privateSBLister    listers.PrivateSecretBindingLister
	quotaLister        listers.QuotaLister
}

var _ = admissioninitializer.WantsInternalGardenInformerFactory(&RejectShootIfQuotaExceeded{})

// New creates a new RejectShootIfQuotaExceeded admission plugin.
func New() (*RejectShootIfQuotaExceeded, error) {
	return &RejectShootIfQuotaExceeded{
		Handler: admission.NewHandler(admission.Create, admission.Update),
	}, nil
}

// SetInternalGardenInformerFactory gets Lister from SharedInformerFactory.
func (h *RejectShootIfQuotaExceeded) SetInternalGardenInformerFactory(f informers.SharedInformerFactory) {
	h.shootLister = f.Garden().InternalVersion().Shoots().Lister()
	h.cloudProfileLister = f.Garden().InternalVersion().CloudProfiles().Lister()
	h.crossSBLister = f.Garden().InternalVersion().CrossSecretBindings().Lister()
	h.privateSBLister = f.Garden().InternalVersion().PrivateSecretBindings().Lister()
	h.quotaLister = f.Garden().InternalVersion().Quotas().Lister()
}

// ValidateInitialization checks whether the plugin was correctly initialized.
func (h *RejectShootIfQuotaExceeded) ValidateInitialization() error {
	if h.shootLister == nil {
		return errors.New("missing shoot lister")
	}
	if h.cloudProfileLister == nil {
		return errors.New("missing cloudProfile lister")
	}
	if h.crossSBLister == nil {
		return errors.New("missing crossSecretBinding lister")
	}
	if h.privateSBLister == nil {
		return errors.New("missing privateSecretBinding lister")
	}
	if h.quotaLister == nil {
		return errors.New("missing quota lister")
	}
	return nil
}

// Admit checks that the requested Shoot resources are within the quota limits.
func (h *RejectShootIfQuotaExceeded) Admit(a admission.Attributes) error {
	// Wait until the caches have been synced
	if !h.WaitForReady() {
		return admission.NewForbidden(a, errors.New("not yet ready to handle request"))
	}

	// Ignore all kinds other than Shoot
	if a.GetKind().GroupKind() != garden.Kind("Shoot") {
		return nil
	}
	if a.GetSubresource() != "" {
		return nil
	}

	shoot, ok := a.GetObject().(*garden.Shoot)
	if !ok {
		return apierrors.NewBadRequest("could not convert resource into Shoot object")
	}

	// Pass if the shoot is intended to get deleted
	if shoot.DeletionTimestamp != nil && shoot.Annotations[common.ConfirmationDeletionTimestamp] != "" {
		return nil
	}

	quotaReferences, err := h.getShootsQuotaReferences(*shoot)
	if err != nil {
		return apierrors.NewInternalError(err)
	}

	// Quotas are cumulative, means each quota must be not exceeded that the admission pass.
	var maxShootLifetime *int
	for _, quotaRef := range quotaReferences {
		quota, err := h.quotaLister.Quotas(quotaRef.Namespace).Get(quotaRef.Name)
		if err != nil {
			return apierrors.NewInternalError(err)
		}

		// Get the max clusterLifeTime
		if quota.Spec.ClusterLifetimeDays != nil {
			if maxShootLifetime == nil {
				maxShootLifetime = quota.Spec.ClusterLifetimeDays
			}
			if *maxShootLifetime > *quota.Spec.ClusterLifetimeDays {
				maxShootLifetime = quota.Spec.ClusterLifetimeDays
			}
		}

		exceededMetrics, err := h.isQuotaExceeded(*shoot, *quota)
		if err != nil {
			return apierrors.NewInternalError(err)
		}
		if exceededMetrics != nil {
			message := ""
			for _, metric := range *exceededMetrics {
				message = message + metric.String() + " "
			}
			return admission.NewForbidden(a, fmt.Errorf("Quota limits exceeded. Unable to allocate further %s", message))
		}
	}

	if lifetime, exists := shoot.Annotations[common.ShootExpirationTimestamp]; exists && maxShootLifetime != nil {
		var (
			plannedExpirationTime   time.Time
			oldExpirationTime       time.Time
			calulatedExpirationTime time.Time
		)

		plannedExpirationTime, err = time.Parse(time.RFC3339, lifetime)
		if err != nil {
			return apierrors.NewInternalError(err)
		}

		// Get the prior version of the Shoot
		oldShoot, ok := a.GetOldObject().(*garden.Shoot)
		if !ok {
			return apierrors.NewBadRequest("could not convert resource into Shoot object")
		}

		if lifetime, exists := oldShoot.Annotations[common.ShootExpirationTimestamp]; exists {
			oldExpirationTime, err = time.Parse(time.RFC3339, lifetime)
			if err != nil {
				return apierrors.NewInternalError(err)
			}
		} else {
			oldExpirationTime = shoot.CreationTimestamp.Time
		}

		calulatedExpirationTime = oldExpirationTime.Add(time.Duration(*maxShootLifetime*24) * time.Hour)
		if plannedExpirationTime.After(calulatedExpirationTime) {
			return admission.NewForbidden(a, fmt.Errorf("Requested shoot expiration time to long. Can only be extended by %d day(s)", *maxShootLifetime))
		}
	}

	return nil
}

func (h *RejectShootIfQuotaExceeded) getShootsQuotaReferences(shoot garden.Shoot) ([]v1.ObjectReference, error) {
	switch shoot.Spec.Cloud.SecretBindingRef.Kind {
	case "CrossSecretBinding":
		usedCrossSB, err := h.crossSBLister.CrossSecretBindings(shoot.Namespace).Get(shoot.Spec.Cloud.SecretBindingRef.Name)
		if err != nil {
			return nil, err
		}
		return usedCrossSB.Quotas, nil
	case "PrivateSecretBinding":
		usedPrivateSB, err := h.privateSBLister.PrivateSecretBindings(shoot.Namespace).Get(shoot.Spec.Cloud.SecretBindingRef.Name)
		if err != nil {
			return nil, err
		}
		return usedPrivateSB.Quotas, nil
	}
	return nil, fmt.Errorf("Unknown binding type %s", shoot.Spec.Cloud.SecretBindingRef.Kind)
}

func (h *RejectShootIfQuotaExceeded) isQuotaExceeded(shoot garden.Shoot, quota garden.Quota) (*[]v1.ResourceName, error) {
	allocatedResources, err := h.determineAllocatedResources(quota, shoot)
	if err != nil {
		return nil, err
	}
	requiredResources, err := h.determineRequiredResources(allocatedResources, shoot)
	if err != nil {
		return nil, err
	}

	exceededMetrics := make([]v1.ResourceName, 0)
	for _, metric := range quotaMetricNames {
		if _, ok := quota.Spec.Metrics[metric]; !ok {
			continue
		}
		if !hasSufficientQuota(quota.Spec.Metrics[metric], requiredResources[metric]) {
			exceededMetrics = append(exceededMetrics, metric)
		}
	}
	if len(exceededMetrics) != 0 {
		return &exceededMetrics, nil
	}
	return nil, nil
}

func (h *RejectShootIfQuotaExceeded) determineAllocatedResources(quota garden.Quota, shoot garden.Shoot) (v1.ResourceList, error) {
	shoots, err := h.findShootsReferQuota(quota, shoot)
	if err != nil {
		return nil, err
	}

	// Collect the resources which are allocated according to the shoot specs
	allocatedResources := make(v1.ResourceList)
	for _, s := range shoots {
		shootResources, err := h.getShootResources(s)
		if err != nil {
			return nil, err
		}
		for _, metric := range quotaMetricNames {
			allocatedResources[metric] = sumQuantity(allocatedResources[metric], shootResources[metric])
		}
	}

	// TODO: We have to determine and add the amount of storage, which is allocated by manually created persistent volumes
	// and the count of loadbalancer, which are created due to manually created services of type loadbalancer

	return allocatedResources, nil
}

func (h *RejectShootIfQuotaExceeded) findShootsReferQuota(quota garden.Quota, shoot garden.Shoot) ([]garden.Shoot, error) {
	var shootsReferQuota []garden.Shoot

	privateSecretBindings, err := h.getPrivateSecretBindingsReferQuota(quota)
	if err != nil {
		return nil, err
	}
	crossSecretBindings, err := h.getCrossSecretBindingsReferQuota(quota)
	if err != nil {
		return nil, err
	}

	// Find all shoots which are referencing found PrivateSecretBindings
	for _, binding := range privateSecretBindings {
		shoots, err := h.shootLister.
			Shoots(binding.Namespace).
			List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, s := range shoots {
			// exclude actual shoot from resource allocation calculation (update case)
			if shoot.Namespace == s.Namespace && shoot.Name == s.Name {
				continue
			}
			if s.Spec.Cloud.SecretBindingRef.Kind == "PrivateSecretBinding" && s.Spec.Cloud.SecretBindingRef.Name == binding.Name {
				shootsReferQuota = append(shootsReferQuota, *s)
			}
		}
	}

	// Find all shoots which are referencing found CrossSecretBindings
	for _, binding := range crossSecretBindings {
		shoots, err := h.shootLister.Shoots(binding.Namespace).List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, s := range shoots {
			// exclude actual shoot from resource allocation calculation (update case)
			if shoot.Namespace == s.Namespace && shoot.Name == s.Name {
				continue
			}
			if s.Spec.Cloud.SecretBindingRef.Kind == "CrossSecretBinding" && s.Spec.Cloud.SecretBindingRef.Name == binding.Name {
				shootsReferQuota = append(shootsReferQuota, *s)
			}
		}
	}
	return shootsReferQuota, nil
}

func (h *RejectShootIfQuotaExceeded) getPrivateSecretBindingsReferQuota(quota garden.Quota) ([]garden.PrivateSecretBinding, error) {
	var privateSBsReferQuota []garden.PrivateSecretBinding

	privateSBs, err := h.privateSBLister.PrivateSecretBindings(v1.NamespaceAll).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, binding := range privateSBs {
		for _, quotaRef := range binding.Quotas {
			if quota.Name == quotaRef.Name && quota.Namespace == quotaRef.Namespace {
				privateSBsReferQuota = append(privateSBsReferQuota, *binding)
			}
		}
	}
	return privateSBsReferQuota, nil
}

func (h *RejectShootIfQuotaExceeded) getCrossSecretBindingsReferQuota(quota garden.Quota) ([]garden.CrossSecretBinding, error) {
	var crossSBsReferQuota []garden.CrossSecretBinding

	crossSBs, err := h.crossSBLister.CrossSecretBindings(v1.NamespaceAll).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, binding := range crossSBs {
		for _, quotaRef := range binding.Quotas {
			if quota.Name == quotaRef.Name && quota.Namespace == quotaRef.Namespace {
				crossSBsReferQuota = append(crossSBsReferQuota, *binding)
			}
		}
	}
	return crossSBsReferQuota, nil
}

func (h *RejectShootIfQuotaExceeded) determineRequiredResources(allocatedResources v1.ResourceList, shoot garden.Shoot) (v1.ResourceList, error) {
	shootResources, err := h.getShootResources(shoot)
	if err != nil {
		return nil, err
	}

	requiredResourches := make(v1.ResourceList)
	for _, metric := range quotaMetricNames {
		requiredResourches[metric] = sumQuantity(allocatedResources[metric], shootResources[metric])
	}
	return requiredResourches, nil
}

func (h *RejectShootIfQuotaExceeded) getShootResources(shoot garden.Shoot) (v1.ResourceList, error) {
	cloudProfile, err := h.cloudProfileLister.Get(shoot.Spec.Cloud.Profile)
	if err != nil {
		return nil, apierrors.NewBadRequest("could not find referenced cloud profile")
	}

	cloudProvider, err := helper.DetermineCloudProviderInShoot(shoot.Spec.Cloud)
	if err != nil {
		return nil, apierrors.NewBadRequest("could not identify the cloud provider kind in the Shoot resource")
	}

	var (
		countLB      int64 = 1
		resources          = make(v1.ResourceList)
		workers            = getShootWorkerResources(shoot, cloudProvider, *cloudProfile)
		machineTypes       = getMachineTypes(cloudProvider, *cloudProfile)
		volumeTypes        = getVolumeTypes(cloudProvider, *cloudProfile)
	)

	for _, worker := range workers {
		var (
			machineType *garden.MachineType
			volumeType  *garden.VolumeType
		)

		// Get the proper machineType
		for _, element := range machineTypes {
			if element.Name == worker.MachineType {
				machineType = &element
				break
			}
		}
		if machineType == nil {
			return nil, fmt.Errorf("MachineType %s not found in CloudProfile %s", worker.MachineType, cloudProfile.Name)
		}

		// Get the proper VolumeType
		for _, element := range volumeTypes {
			if element.Name == worker.VolumeType {
				volumeType = &element
				break
			}
		}
		if volumeType == nil {
			return nil, fmt.Errorf("VolumeType %s not found in CloudProfile %s", worker.MachineType, cloudProfile.Name)
		}

		// For now we always use the max. amount of resources for quota calculation
		resources[garden.QuotaMetricCPU] = multiplyQuantity(machineType.CPU, worker.AutoScalerMax)
		resources[garden.QuotaMetricGPU] = multiplyQuantity(machineType.GPU, worker.AutoScalerMax)
		resources[garden.QuotaMetricMemory] = multiplyQuantity(machineType.Memory, worker.AutoScalerMax)

		switch volumeType.Class {
		case garden.VolumeClassStandard:
			resources[garden.QuotaMetricStorageStandard] = multiplyQuantity(worker.VolumeSize, worker.AutoScalerMax)
		case garden.VolumeClassPremium:
			resources[garden.QuotaMetricStoragePremium] = multiplyQuantity(worker.VolumeSize, worker.AutoScalerMax)
		default:
			return nil, fmt.Errorf("Unknown volumeType class %s", volumeType.Class)
		}
	}

	if shoot.Spec.Addons != nil && shoot.Spec.Addons.NginxIngress != nil && shoot.Spec.Addons.NginxIngress.Addon.Enabled {
		countLB++
	}
	resources[garden.QuotaMetricLoadbalancer] = *resource.NewQuantity(countLB, resource.DecimalSI)

	return resources, nil
}

func getShootWorkerResources(shoot garden.Shoot, cloudProvider garden.CloudProvider, cloudProfile garden.CloudProfile) []quotaWorker {
	var workers []quotaWorker

	switch cloudProvider {
	case garden.CloudProviderAWS:
		workers = make([]quotaWorker, len(shoot.Spec.Cloud.AWS.Workers))

		for idx, awsWorker := range shoot.Spec.Cloud.AWS.Workers {
			workers[idx].Worker = awsWorker.Worker
			workers[idx].VolumeType = awsWorker.VolumeType
			workers[idx].VolumeSize = resource.MustParse(awsWorker.VolumeSize)
		}
	case garden.CloudProviderAzure:
		workers = make([]quotaWorker, len(shoot.Spec.Cloud.Azure.Workers))

		for idx, azureWorker := range shoot.Spec.Cloud.Azure.Workers {
			workers[idx].Worker = azureWorker.Worker
			workers[idx].VolumeType = azureWorker.VolumeType
			workers[idx].VolumeSize = resource.MustParse(azureWorker.VolumeSize)
		}
	case garden.CloudProviderGCP:
		workers = make([]quotaWorker, len(shoot.Spec.Cloud.GCP.Workers))

		for idx, gcpWorker := range shoot.Spec.Cloud.GCP.Workers {
			workers[idx].Worker = gcpWorker.Worker
			workers[idx].VolumeType = gcpWorker.VolumeType
			workers[idx].VolumeSize = resource.MustParse(gcpWorker.VolumeSize)
		}
	case garden.CloudProviderOpenStack:
		workers = make([]quotaWorker, len(shoot.Spec.Cloud.OpenStack.Workers))

		for idx, osWorker := range shoot.Spec.Cloud.OpenStack.Workers {
			workers[idx].Worker = osWorker.Worker
			for _, machineType := range cloudProfile.Spec.OpenStack.Constraints.MachineTypes {
				if osWorker.MachineType == machineType.Name {
					workers[idx].VolumeType = machineType.MachineType.Name
					workers[idx].VolumeSize = machineType.VolumeSize
				}
			}
		}
	}
	return workers
}

func getMachineTypes(provider garden.CloudProvider, cloudProfile garden.CloudProfile) []garden.MachineType {
	var machineTypes []garden.MachineType
	switch provider {
	case garden.CloudProviderAWS:
		machineTypes = cloudProfile.Spec.AWS.Constraints.MachineTypes
	case garden.CloudProviderAzure:
		machineTypes = cloudProfile.Spec.Azure.Constraints.MachineTypes
	case garden.CloudProviderGCP:
		machineTypes = cloudProfile.Spec.GCP.Constraints.MachineTypes
	case garden.CloudProviderOpenStack:
		machineTypes = make([]garden.MachineType, 0)
		for _, element := range cloudProfile.Spec.OpenStack.Constraints.MachineTypes {
			machineTypes = append(machineTypes, element.MachineType)
		}
	}
	return machineTypes
}

func getVolumeTypes(provider garden.CloudProvider, cloudProfile garden.CloudProfile) []garden.VolumeType {
	var volumeTypes []garden.VolumeType
	switch provider {
	case garden.CloudProviderAWS:
		volumeTypes = cloudProfile.Spec.AWS.Constraints.VolumeTypes
	case garden.CloudProviderAzure:
		volumeTypes = cloudProfile.Spec.Azure.Constraints.VolumeTypes
	case garden.CloudProviderGCP:
		volumeTypes = cloudProfile.Spec.GCP.Constraints.VolumeTypes
	case garden.CloudProviderOpenStack:
		volumeTypes = make([]garden.VolumeType, 0)
		contains := func(types []garden.VolumeType, volumeType string) bool {
			for _, element := range types {
				if element.Name == volumeType {
					return true
				}
			}
			return false
		}

		for _, machineType := range cloudProfile.Spec.OpenStack.Constraints.MachineTypes {
			if !contains(volumeTypes, machineType.MachineType.Name) {
				volumeTypes = append(volumeTypes, garden.VolumeType{
					Name:  machineType.MachineType.Name,
					Class: machineType.VolumeType,
				})
			}
		}
	}
	return volumeTypes
}

func hasSufficientQuota(limit, required resource.Quantity) bool {
	compareCode := limit.Cmp(required)
	if compareCode == -1 {
		return false
	}
	return true
}

func sumQuantity(values ...resource.Quantity) resource.Quantity {
	res := resource.Quantity{}
	for _, v := range values {
		res.Add(v)
	}
	return res
}

func multiplyQuantity(quantity resource.Quantity, multiplier int) resource.Quantity {
	res := resource.Quantity{}
	for i := 0; i < multiplier; i++ {
		res.Add(quantity)
	}
	return res
}
