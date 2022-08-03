package kube

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"ksniff/pkg/service/sniffer/runtime"
	"ksniff/utils"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const cpuLimit = "500m"
const cpuReq = "300m"
const memLimit = "256Mi"
const memReq = "128Mi"

type KubernetesApiService interface {
	ExecuteCommand(podName string, containerName string, command []string, stdOut io.Writer) (int, error)

	DeletePod(podName string) error

	CreatePrivilegedPod(nodeName string, containerName string, image string, socketPath string, timeout time.Duration) (*corev1.Pod, error)

	UploadFile(localPath string, remotePath string, podName string, containerName string) error
}

type KubernetesApiServiceImpl struct {
	clientset       *kubernetes.Clientset
	restConfig      *rest.Config
	targetNamespace string
}

func NewKubernetesApiService(clientset *kubernetes.Clientset,
	restConfig *rest.Config, targetNamespace string) KubernetesApiService {

	return &KubernetesApiServiceImpl{clientset: clientset,
		restConfig:      restConfig,
		targetNamespace: targetNamespace}
}

func (k *KubernetesApiServiceImpl) IsSupportedContainerRuntime(nodeName string) (bool, error) {
	node, err := k.clientset.CoreV1().Nodes().Get(context.TODO(), nodeName, v1.GetOptions{})
	if err != nil {
		return false, err
	}

	nodeRuntimeVersion := node.Status.NodeInfo.ContainerRuntimeVersion

	for _, runtime := range runtime.SupportedContainerRuntimes {
		if strings.HasPrefix(nodeRuntimeVersion, runtime) {
			return true, nil
		}
	}

	return false, nil
}

func (k *KubernetesApiServiceImpl) ExecuteCommand(podName string, containerName string, command []string, stdOut io.Writer) (int, error) {

	log.Infof("executing command: '%s' on container: '%s', pod: '%s', namespace: '%s'", command, containerName, podName, k.targetNamespace)
	stdErr := new(Writer)

	executeTcpdumpRequest := ExecCommandRequest{
		KubeRequest: KubeRequest{
			Clientset:  k.clientset,
			RestConfig: k.restConfig,
			Namespace:  k.targetNamespace,
			Pod:        podName,
			Container:  containerName,
		},
		Command: command,
		StdErr:  stdErr,
		StdOut:  stdOut,
	}

	exitCode, err := PodExecuteCommand(executeTcpdumpRequest)
	if err != nil {
		log.WithError(err).Errorf("failed executing command: '%s', exitCode: '%d', stdErr: '%s'",
			command, exitCode, stdErr.Output)

		return exitCode, err
	}

	log.Infof("command: '%s' executing successfully exitCode: '%d', stdErr :'%s'", command, exitCode, stdErr.Output)

	return exitCode, err
}

func (k *KubernetesApiServiceImpl) DeletePod(podName string) error {
	var gracePeriodTime int64 = 0

	err := k.clientset.CoreV1().Pods(k.targetNamespace).Delete(context.TODO(), podName, v1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodTime,
	})

	return err
}

func (k *KubernetesApiServiceImpl) CreatePrivilegedPod(nodeName string, containerName string, image string, socketPath string, timeout time.Duration) (*corev1.Pod, error) {
	log.Debugf("creating privileged pod on remote node")

	isSupported, err := k.IsSupportedContainerRuntime(nodeName)
	if err != nil {
		return nil, err
	}

	if !isSupported {
		return nil, errors.Errorf("Container runtime on node %s isn't supported. Supported container runtimes are: %v", nodeName, runtime.SupportedContainerRuntimes)
	}

	typeMetadata := v1.TypeMeta{
		Kind:       "Pod",
		APIVersion: "v1",
	}

	objectMetadata := v1.ObjectMeta{
		GenerateName: "ksniff-",
		Namespace:    k.targetNamespace,
		Labels: map[string]string{
			"app": "ksniff",
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "container-socket",
			ReadOnly:  true,
			MountPath: socketPath,
		},
		{
			Name:      "host",
			ReadOnly:  false,
			MountPath: "/host",
		},
	}

	privileged := true
	privilegedContainer := corev1.Container{
		Name:            containerName,
		Image:           image,
		ImagePullPolicy: "IfNotPresent",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"cpu":    resource.MustParse(cpuLimit),
				"memory": resource.MustParse(memLimit),
			},
			Requests: corev1.ResourceList{
				"cpu":    resource.MustParse(cpuReq),
				"memory": resource.MustParse(memReq),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: &privileged,
		},

		Command:      []string{"sh", "-c", "sleep 10000000"},
		VolumeMounts: volumeMounts,
	}

	hostPathType := corev1.HostPathSocket
	directoryType := corev1.HostPathDirectory

	podSpecs := corev1.PodSpec{
		NodeName:      nodeName,
		RestartPolicy: corev1.RestartPolicyNever,
		HostPID:       true,
		Containers:    []corev1.Container{privilegedContainer},
		Volumes: []corev1.Volume{
			{
				Name: "host",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/",
						Type: &directoryType,
					},
				},
			},
			{
				Name: "container-socket",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: socketPath,
						Type: &hostPathType,
					},
				},
			},
		},
	}

	pod := corev1.Pod{
		TypeMeta:   typeMetadata,
		ObjectMeta: objectMetadata,
		Spec:       podSpecs,
	}

	createdPod, err := k.clientset.CoreV1().Pods(k.targetNamespace).Create(context.TODO(), &pod, v1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	log.Infof("pod: '%v' created successfully in namespace: '%v'", createdPod.ObjectMeta.Name, createdPod.ObjectMeta.Namespace)
	log.Debugf("created pod details: %v", createdPod)

	verifyPodState := func() bool {
		podStatus, err := k.clientset.CoreV1().Pods(k.targetNamespace).Get(context.TODO(), createdPod.Name, v1.GetOptions{})
		if err != nil {
			return false
		}

		if podStatus.Status.Phase == corev1.PodRunning {
			return true
		}

		return false
	}

	log.Info("waiting for pod successful startup")

	if !utils.RunWhileFalse(verifyPodState, timeout, 1*time.Second) {
		return nil, errors.Errorf("failed to create pod within timeout (%s)", timeout)
	}

	return createdPod, nil
}

func (k *KubernetesApiServiceImpl) checkIfFileExistOnPod(remotePath string, podName string, containerName string) (bool, error) {
	stdOut := new(Writer)
	stdErr := new(Writer)

	command := []string{"/bin/sh", "-c", fmt.Sprintf("test -f %s", remotePath)}

	exitCode, err := k.ExecuteCommand(podName, containerName, command, stdOut)
	if err != nil {
		return false, err
	}

	if exitCode != 0 {
		return false, nil
	}

	if stdErr.Output != "" {
		return false, errors.New("failed to check for tcpdump")
	}

	log.Infof("file found: '%s'", stdOut.Output)

	return true, nil
}

func (k *KubernetesApiServiceImpl) UploadFile(localPath string, remotePath string, podName string, containerName string) error {
	log.Infof("uploading file: '%s' to '%s' on container: '%s'", localPath, remotePath, containerName)

	isExist, err := k.checkIfFileExistOnPod(remotePath, podName, containerName)
	if err != nil {
		return err
	}

	if isExist {
		log.Info("file was already found on remote pod")
		return nil
	}

	log.Infof("file not found on: '%s', starting to upload", remotePath)

	req := UploadFileRequest{
		KubeRequest: KubeRequest{
			Clientset:  k.clientset,
			RestConfig: k.restConfig,
			Namespace:  k.targetNamespace,
			Pod:        podName,
			Container:  containerName,
		},
		Src: localPath,
		Dst: remotePath,
	}

	exitCode, err := PodUploadFile(req)
	if err != nil || exitCode != 0 {
		return errors.Wrapf(err, "upload file failed, exitCode: %d", exitCode)
	}

	log.Info("verifying file uploaded successfully")

	isExist, err = k.checkIfFileExistOnPod(remotePath, podName, containerName)
	if err != nil {
		return err
	}

	if !isExist {
		log.Error("failed to upload file.")
		return errors.New("couldn't locate file on pod after upload done")
	}

	log.Info("file uploaded successfully")

	return nil
}
