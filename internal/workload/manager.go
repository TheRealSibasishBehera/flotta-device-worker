package workload

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/jakub-dzon/k4e-device-worker/internal/volumes"

	"git.sr.ht/~spc/go-log"
	api2 "github.com/jakub-dzon/k4e-device-worker/internal/workload/api"
	"github.com/jakub-dzon/k4e-operator/models"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	defaultWorkloadsMonitoringInterval = 15
)

type WorkloadManager struct {
	manifestsDir        string
	volumesDir          string
	workloads           WorkloadWrapper
	managementLock      sync.Locker
	ticker              *time.Ticker
	deregistered        bool
	deviceConfigMapName string
	deviceConfigMapPath string
}

type podAndPath struct {
	pod          v1.Pod
	manifestPath string
}

func NewWorkloadManager(dataDir string, deviceConfigMapName string, deviceConfigMapPath string) (*WorkloadManager, error) {
	wrapper, err := newWorkloadInstance(dataDir)
	if err != nil {
		return nil, err
	}

	return NewWorkloadManagerWithParams(dataDir, wrapper, deviceConfigMapName, deviceConfigMapPath)
}

func NewWorkloadManagerWithParams(dataDir string, ww WorkloadWrapper, deviceConfigMapName string, deviceConfigMapPath string) (*WorkloadManager, error) {
	manifestsDir := path.Join(dataDir, "manifests")
	if err := os.MkdirAll(manifestsDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create directory: %w", err)
	}
	volumesDir := path.Join(dataDir, "volumes")
	if err := os.MkdirAll(volumesDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create directory: %w", err)
	}

	manager := WorkloadManager{
		manifestsDir:        manifestsDir,
		volumesDir:          volumesDir,
		workloads:           ww,
		managementLock:      &sync.Mutex{},
		deregistered:        false,
		deviceConfigMapName: deviceConfigMapName,
		deviceConfigMapPath: deviceConfigMapPath,
	}
	if err := manager.workloads.Init(); err != nil {
		return nil, err
	}

	manager.initTicker(defaultWorkloadsMonitoringInterval)
	return &manager, nil
}

func (w *WorkloadManager) ListWorkloads() ([]api2.WorkloadInfo, error) {
	return w.workloads.List()
}

func (w *WorkloadManager) GetExportedHostPath(workloadName string) string {
	return volumes.HostPathVolumePath(w.volumesDir, workloadName)
}

func (w *WorkloadManager) Update(configuration models.DeviceConfigurationMessage) error {
	w.managementLock.Lock()
	defer w.managementLock.Unlock()
	var errors error
	if w.deregistered {
		log.Info("Deregistration was finished, no need to update anymore")
		return errors
	}

	configMapsPaths := w.prepareConfigMapsPaths()
	configuredWorkloadNameSet := make(map[string]struct{})
	for _, workload := range configuration.Workloads {
		log.Tracef("Deploying workload: %s", workload.Name)
		configuredWorkloadNameSet[workload.Name] = struct{}{}

		pod, err := w.toPod(workload)
		if err != nil {
			errors = multierror.Append(errors, fmt.Errorf(
				"cannot convert workload '%s' to pod: %s", workload.Name, err))
			continue
		}
		manifestPath := w.getManifestPath(pod.Name)
		podYaml, err := w.toPodYaml(pod)
		if err != nil {
			errors = multierror.Append(errors, fmt.Errorf("cannot create pod's Yaml: %s", err))
			continue
		}
		if !w.podModified(manifestPath, podYaml) {
			log.Tracef("Pod '%s' definition is unchanged (%s)", workload.Name, manifestPath)
			continue
		}
		err = w.storeManifest(manifestPath, podYaml)
		if err != nil {
			errors = multierror.Append(errors, fmt.Errorf(
				"cannot store manifest for workload '%s': %s", workload.Name, err))
			continue
		}

		err = w.workloads.Remove(workload.Name)
		if err != nil {
			log.Errorf("Error removing workload: %v", err)
			errors = multierror.Append(errors, fmt.Errorf("error removing workload %s: %s", workload.Name, err))
			continue
		}

		err = w.workloads.Run(pod, manifestPath, configMapsPaths)
		if err != nil {
			log.Errorf("Cannot run workload: %v", err)
			errors = multierror.Append(errors, fmt.Errorf(
				"cannot run workload '%s': %s", workload.Name, err))
			continue
		}
	}

	deployedWorkloadByName, err := w.indexWorkloads()
	if err != nil {
		log.Errorf("Cannot get deployed workloads: %v", err)
		errors = multierror.Append(errors, fmt.Errorf("cannot get deployed workloads: %s", err))
		return errors
	}
	// Remove any workloads that don't correspond to the configured ones
	for name := range deployedWorkloadByName {
		if _, ok := configuredWorkloadNameSet[name]; !ok {
			log.Infof("Workload not found: %s. Removing", name)
			manifestPath := w.getManifestPath(name)
			err := os.Remove(manifestPath)
			if err != nil {
				if !os.IsNotExist(err) {
					errors = multierror.Append(errors, fmt.Errorf("cannot remove existing manifest workload: %s", err))
				}
			}

			if err := w.workloads.Remove(name); err != nil {
				errors = multierror.Append(errors, fmt.Errorf("cannot remove stale workload name='%s': %s", name, err))
			}
			log.Infof("Workload %s removed", name)
		}
	}
	// Reset the interval of the current monitoring routine
	if configuration.WorkloadsMonitoringInterval > 0 {
		w.ticker.Reset(time.Duration(configuration.WorkloadsMonitoringInterval))
	}
	return errors
}

func (w *WorkloadManager) initTicker(periodSeconds int64) {
	ticker := time.NewTicker(time.Second * time.Duration(periodSeconds))
	w.ticker = ticker
	go func() {
		for range ticker.C {
			err := w.ensureWorkloadsFromManifestsAreRunning()
			if err != nil {
				log.Error(err)
			}
		}
	}()
}

func (w *WorkloadManager) storeManifest(filePath string, podYaml []byte) error {
	return ioutil.WriteFile(filePath, podYaml, 0640)
}

func (w *WorkloadManager) getManifestPath(workloadName string) string {
	fileName := strings.ReplaceAll(workloadName, " ", "-") + ".yaml"
	return path.Join(w.manifestsDir, fileName)
}

func (w *WorkloadManager) toPodYaml(pod *v1.Pod) ([]byte, error) {
	podYaml, err := yaml.Marshal(pod)
	if err != nil {
		return nil, err
	}
	return podYaml, nil
}

func (w *WorkloadManager) ensureWorkloadsFromManifestsAreRunning() error {
	w.managementLock.Lock()
	defer w.managementLock.Unlock()
	nameToWorkload, err := w.indexWorkloads()
	if err != nil {
		return err
	}

	manifestInfo, err := ioutil.ReadDir(w.manifestsDir)
	if err != nil {
		return err
	}
	manifestNameToPodAndPath := make(map[string]podAndPath)
	for _, fi := range manifestInfo {
		filePath := path.Join(w.manifestsDir, fi.Name())
		manifest, err := ioutil.ReadFile(filePath)
		if err != nil {
			log.Error(err)
			continue
		}
		pod := v1.Pod{}
		err = yaml.Unmarshal(manifest, &pod)
		if err != nil {
			log.Error(err)
			continue
		}
		manifestNameToPodAndPath[pod.Name] = podAndPath{pod, filePath}
	}

	// Remove any workloads that don't correspond to stored manifests
	for name := range nameToWorkload {
		if _, ok := manifestNameToPodAndPath[name]; !ok {
			log.Infof("Workload not found: %s. Removing", name)
			if err := w.workloads.Remove(name); err != nil {
				log.Error(err)
			}
		}
	}

	for name, podWithPath := range manifestNameToPodAndPath {
		if workload, ok := nameToWorkload[name]; ok {
			if workload.Status != "Running" {
				// Workload is not running - start
				err = w.workloads.Start(&podWithPath.pod)
				if err != nil {
					log.Errorf("failed to start workload %s: %v", name, err)
				}
			}
			continue
		}
		// Workload is not present - run
		err = w.workloads.Run(&podWithPath.pod, podWithPath.manifestPath, w.prepareConfigMapsPaths())
		if err != nil {
			log.Errorf("failed to run workload %s (manifest: %s): %v", name, podWithPath.manifestPath, err)
			continue
		}
	}
	if err = w.workloads.PersistConfiguration(); err != nil {
		log.Errorf("failed to persist workload configuration: %v", err)
	}
	return nil
}

func (w *WorkloadManager) indexWorkloads() (map[string]api2.WorkloadInfo, error) {
	workloads, err := w.workloads.List()
	if err != nil {
		return nil, err
	}
	nameToWorkload := make(map[string]api2.WorkloadInfo)
	for _, workload := range workloads {
		nameToWorkload[workload.Name] = workload
	}
	return nameToWorkload, nil
}

func (w *WorkloadManager) RegisterObserver(observer Observer) {
	w.workloads.RegisterObserver(observer)
}

func (w *WorkloadManager) Deregister() error {
	w.managementLock.Lock()
	defer w.managementLock.Unlock()

	var errors error
	err := w.removeAllWorkloads()
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("failed to remove workloads: %v", err))
		log.Errorf("failed to remove workloads: %v", err)
	}

	err = w.deleteManifestsDir()
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("failed to delete manifests directory: %v", err))
		log.Errorf("failed to delete manifests directory: %v", err)
	}

	err = w.deleteTable()
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("failed to delete table: %v", err))
		log.Errorf("failed to delete table: %v", err)
	}

	err = w.deleteVolumeDir()
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("failed to delete volumes directory: %v", err))
		log.Errorf("failed to delete volumes directory: %v", err)
	}

	err = w.removeTicker()
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("failed to remove ticker: %v", err))
		log.Errorf("failed to remove ticker: %v", err)
	}

	err = w.removeMappingFile()
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("failed to remove mapping file: %v", err))
		log.Errorf("failed to remove mapping file: %v", err)
	}

	w.deregistered = true
	return errors
}

func (w *WorkloadManager) removeTicker() error {
	log.Info("Stopping ticker that ensure workloads from manifests are running")
	if w.ticker != nil {
		w.ticker.Stop()
	}
	return nil
}

func (w *WorkloadManager) removeAllWorkloads() error {
	log.Info("Removing all workload")
	workloads, err := w.workloads.List()
	if err != nil {
		return err
	}
	for _, workload := range workloads {
		log.Infof("Removing workload %s", workload.Name)
		err := w.workloads.Remove(workload.Name)
		if err != nil {
			log.Errorf("Error removing workload %[1]s: %v", workload.Name, err)
			return err
		}
	}
	return nil
}

func (w *WorkloadManager) deleteManifestsDir() error {
	log.Info("Deleting manifests directory")
	err := os.RemoveAll(w.manifestsDir)
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

func (w *WorkloadManager) deleteVolumeDir() error {
	log.Info("Deleting volumes directory")
	err := os.RemoveAll(w.volumesDir)
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

func (w *WorkloadManager) deleteTable() error {
	log.Info("Deleting nftable")
	err := w.workloads.RemoveTable()
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

func (w *WorkloadManager) removeMappingFile() error {
	log.Info("Deleting mapping file")
	err := w.workloads.RemoveMappingFile()
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

func (w *WorkloadManager) toPod(workload *models.Workload) (*v1.Pod, error) {
	podSpec := v1.PodSpec{}
	err := yaml.Unmarshal([]byte(workload.Specification), &podSpec)
	if err != nil {
		return nil, err
	}
	pod := v1.Pod{
		Spec: podSpec,
	}
	pod.Kind = "Pod"
	pod.Name = workload.Name
	exportVolume := volumes.HostPathVolume(w.volumesDir, workload.Name)
	pod.Spec.Volumes = append(pod.Spec.Volumes, exportVolume)
	var containers []v1.Container
	for _, container := range pod.Spec.Containers {
		mount := v1.VolumeMount{
			Name:      exportVolume.Name,
			MountPath: "/export",
		}
		container.VolumeMounts = append(container.VolumeMounts, mount)
		configMapRef := v1.EnvFromSource{
			ConfigMapRef: &v1.ConfigMapEnvSource{
				LocalObjectReference: v1.LocalObjectReference{Name: w.deviceConfigMapName},
			},
		}
		container.EnvFrom = append(container.EnvFrom, configMapRef)
		containers = append(containers, container)
	}
	pod.Spec.Containers = containers
	return &pod, nil
}

func (w *WorkloadManager) podModified(manifestPath string, podYaml []byte) bool {
	file, err := ioutil.ReadFile(manifestPath)
	if err != nil {
		return true
	}
	return bytes.Compare(file, podYaml) != 0
}

func (w *WorkloadManager) prepareConfigMapsPaths() []string {
	return []string{w.deviceConfigMapPath}
}
