// Copyright 2019 The redis-operator Authors
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

package redis

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"

	k8sv1alpha1 "github.com/amaizfinance/redis-operator/pkg/apis/k8s/v1alpha1"
	"github.com/amaizfinance/redis-operator/pkg/redis"

	"golang.org/x/crypto/argon2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	redisName = "redis"
	redisPort = redis.Port

	exporterName = "exporter"
	exporterPort = 9121

	// key-value pair of the label indicating the Redis master
	roleLabelKey = "role"
	masterLabel  = "master"
	replicaLabel = "replica"

	// hashAnnotationKey for storing the Kubernetes resource hash
	hashAnnotationKey = "resource-revision-hash"

	// templates
	namePrefixTemplate = `redis-%s`
	authConfTemplate   = "requirepass %[1]s\nmasterauth %[1]s\n"

	// paths and file paths
	configFileName     = "redis.conf"
	configMapMountPath = "/config/" + configFileName
	secretFileName     = "auth.conf"
	secretMountPath    = "/secret/" + secretFileName
	dataMountPath      = "/data"
	workingDir         = dataMountPath

	// argon2id parameters.
	// Recommended parameters are time = 1, Memory = 65536.
	// Below parameters are equivalent(time-wise) to time = 4, Memory = 65536.
	// Time is set to 64 to compensate low Memory = 4096
	argonTime   = 1 << 6
	argonMemory = 1 << 12
	hashLen     = 1 << 6

	// Annotation key for password hash
	passwordHashKey = "redis-password-hash"

	headlessServiceTypeLabelKey = "service-type"
	headlessServiceTypeLabel    = "headless"

	// types of services created
	serviceTypeAll = iota
	serviceTypeHeadless
	serviceTypeMaster
)

var (
	// excludedConfigDirectives represents a set of configuration directive that will be ignored.
	// This will prevent breaking the configuration of a Redis instance by accidentally setting the parameters
	// that are not supposed to be changed or those controlled by redis-operator.
	// Sorted in order of appearance in https://github.com/antirez/redis/blob/5.0/redis.conf
	excludedConfigDirectives = map[string]struct{}{
		"include":               {},
		"bind":                  {},
		"protected-mode":        {},
		"port":                  {},
		"daemonize":             {},
		"dir":                   {},
		"replica-announce-ip":   {},
		"replica-announce-port": {},
		"replicaof":             {},
		"masterauth":            {},
		"requirepass":           {},
		"rename-command":        {},
	}
	argonThreads = uint8(runtime.NumCPU())
)

// objectGeneratorOptions is needed to be passed to a generic object generator
type objectGeneratorOptions struct {
	password    string
	master      redis.Address
	serviceType int
}

// generateObject is a Kubernetes object factory, returns the name of the object and the object itself
func generateObject(r *k8sv1alpha1.Redis, object k8sruntime.Object, options objectGeneratorOptions) (string, k8sruntime.Object) {
	switch object.(type) {
	case *corev1.Secret:
		return generateSecret(r, options.password)
	case *corev1.ConfigMap:
		return generateConfigMap(r, options.master)
	case *corev1.Service:
		return generateService(r, options.serviceType)
	case *policyv1beta1.PodDisruptionBudget:
		return generatePodDisruptionBudget(r)
	case *appsv1.StatefulSet:
		return generateStatefulSet(r, options.password)
	}
	return "", nil
}

// objectUpdateNeeded compares two generic Kubernetes objects and updates the fields that differ.
// See below for specific implementations.
func objectUpdateNeeded(got, want k8sruntime.Object) (needed bool) {
	switch got.(type) {
	case *corev1.Secret:
		return secretUpdateNeeded(got.(*corev1.Secret), want.(*corev1.Secret))
	case *corev1.ConfigMap:
		return configMapUpdateNeeded(got.(*corev1.ConfigMap), want.(*corev1.ConfigMap))
	case *corev1.Service:
		return serviceUpdateNeeded(got.(*corev1.Service), want.(*corev1.Service))
	case *policyv1beta1.PodDisruptionBudget:
		return podDisruptionBudgetUpdateNeeded(got.(*policyv1beta1.PodDisruptionBudget), want.(*policyv1beta1.PodDisruptionBudget))
	case *appsv1.StatefulSet:
		return statefulSetUpdateNeeded(got.(*appsv1.StatefulSet), want.(*appsv1.StatefulSet))
	}
	return
}

// resource generators
func generateSecret(r *k8sv1alpha1.Redis, password string) (string, *corev1.Secret) {
	var b strings.Builder
	defer b.Reset()
	fmt.Fprintf(&b, authConfTemplate, password)

	return generateName(r), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: generateName(r), Namespace: r.Namespace, Labels: r.Labels},
		Data:       map[string][]byte{secretFileName: []byte(b.String())},
	}
}

func generateConfigMap(r *k8sv1alpha1.Redis, master redis.Address) (string, *corev1.ConfigMap) {
	var b strings.Builder
	defer b.Reset()
	// explicitly set the working directory
	fmt.Fprintf(&b, "# Generated by redis-operator for redis.k8s.amaiz.com/%s\ndir %s\n", r.Name, workingDir)

	if r.Spec.Password.SecretKeyRef != nil {
		fmt.Fprintf(&b, "include %s\n", secretMountPath)
	}

	for k, v := range r.Spec.Config {
		if _, ok := excludedConfigDirectives[k]; !ok {
			fmt.Fprintf(&b, "%s %s\n", k, v)
		}
	}

	if master != (redis.Address{}) {
		fmt.Fprintf(&b, "replicaof %s %d\n", master.Host, redis.Port)
	}

	return generateName(r), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: generateName(r), Namespace: r.Namespace, Labels: r.Labels},
		Data:       map[string]string{configFileName: b.String()}}
}

func generateService(r *k8sv1alpha1.Redis, serviceType int) (string, *corev1.Service) {
	var name, clusterIP string
	var selector map[string]string
	labels := map[string]string{}
	for k, v := range r.Labels {
		labels[k] = v
	}

	switch serviceType {
	case serviceTypeAll:
		name = generateName(r)
		selector = r.Labels
	case serviceTypeHeadless:
		name = fmt.Sprintf("%s-%s", generateName(r), headlessServiceTypeLabel)
		selector = r.Labels
		labels[headlessServiceTypeLabelKey] = headlessServiceTypeLabel
		clusterIP = corev1.ClusterIPNone
	case serviceTypeMaster:
		name = fmt.Sprintf("%s-%s", generateName(r), masterLabel)
		selector = labels
		labels[roleLabelKey] = masterLabel
	}

	ports := []corev1.ServicePort{{
		Name:       redisName,
		Protocol:   corev1.ProtocolTCP,
		Port:       redisPort,
		TargetPort: intstr.FromInt(redisPort),
	}}
	if !reflect.DeepEqual(r.Spec.Exporter, k8sv1alpha1.ContainerSpec{}) {
		ports = append(ports, corev1.ServicePort{
			Name:       exporterName,
			Protocol:   corev1.ProtocolTCP,
			Port:       exporterPort,
			TargetPort: intstr.FromInt(exporterPort),
		})
	}

	return name, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Ports:     ports,
			Selector:  selector,
			ClusterIP: clusterIP,
			Type:      corev1.ServiceTypeClusterIP,
		},
	}
}

func generatePodDisruptionBudget(r *k8sv1alpha1.Redis) (string, *policyv1beta1.PodDisruptionBudget) {
	return generateName(r), &policyv1beta1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: generateName(r), Namespace: r.Namespace, Labels: r.Labels},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			MinAvailable: &[]intstr.IntOrString{intstr.FromInt(redis.MinimumFailoverSize)}[0],
			Selector:     &metav1.LabelSelector{MatchLabels: r.Labels},
		},
	}
}

func generateStatefulSet(r *k8sv1alpha1.Redis, password string) (string, *appsv1.StatefulSet) {
	// VolumeMount names
	configMapMountName := fmt.Sprintf("%s-config", generateName(r))
	secretMountName := fmt.Sprintf("%s-secret", generateName(r))
	dataMountName := fmt.Sprintf("%s-data", generateName(r))

	volumes := []corev1.Volume{{
		Name: configMapMountName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: generateName(r),
				},
			},
		},
	}}

	// redis container goes first
	containers := []corev1.Container{{
		Name:       redisName,
		Image:      r.Spec.Redis.Image,
		Args:       []string{configMapMountPath},
		WorkingDir: workingDir,
		Resources:  r.Spec.Redis.Resources,
		VolumeMounts: []corev1.VolumeMount{{
			Name:      configMapMountName,
			ReadOnly:  true,
			MountPath: configMapMountPath,
			SubPath:   configFileName,
		}},
		LivenessProbe:   &corev1.Probe{Handler: corev1.Handler{Exec: &corev1.ExecAction{Command: []string{"redis-cli", "ping"}}}},
		ReadinessProbe:  &corev1.Probe{Handler: corev1.Handler{Exec: &corev1.ExecAction{Command: []string{"redis-cli", "ping"}}}},
		ImagePullPolicy: corev1.PullAlways,
		SecurityContext: r.Spec.Redis.SecurityContext,
	}}

	// if Redis is protected by password:
	// - add the password hash as the annotation to pod,
	// - add the volume with auth.conf
	// - mount the volume
	if r.Spec.Password.SecretKeyRef != nil {
		// rotating passwords requires Pod restarts.
		// adding password hash as the pod annotation will automatically trigger rolling pod restarts.
		r.Spec.Annotations[passwordHashKey] = fmt.Sprintf("%x", argon2.IDKey(
			[]byte(password), []byte(r.UID), argonTime, argonMemory, argonThreads, hashLen,
		))

		volumes = append(volumes, corev1.Volume{
			Name: secretMountName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: generateName(r),
				},
			},
		})

		containers[0].Env = []corev1.EnvVar{{
			Name: "REDISCLI_AUTH",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: r.Spec.Password.SecretKeyRef,
			},
		}}

		containers[0].VolumeMounts = append(containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      secretMountName,
			ReadOnly:  true,
			MountPath: secretMountPath,
			SubPath:   secretFileName,
		})
	}

	volumeClaimTemplates := []corev1.PersistentVolumeClaim{}
	if !reflect.DeepEqual(r.Spec.DataVolumeClaimTemplate, corev1.PersistentVolumeClaim{}) {
		volumeClaimTemplates = append(volumeClaimTemplates, r.Spec.DataVolumeClaimTemplate)
		containers[0].VolumeMounts = append(containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      r.Spec.DataVolumeClaimTemplate.Name,
			MountPath: dataMountPath,
		})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name:         dataMountName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		containers[0].VolumeMounts = append(containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      dataMountName,
			MountPath: dataMountPath,
		})
	}

	// exporter goes next if it is defined
	if !reflect.DeepEqual(r.Spec.Exporter, k8sv1alpha1.ContainerSpec{}) {
		containers = append(containers, corev1.Container{
			Name:  exporterName,
			Image: r.Spec.Exporter.Image,
			Args:  []string{fmt.Sprintf("--web.listen-address=:%d", exporterPort)},
			Env: []corev1.EnvVar{{
				Name: "REDIS_ALIAS",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			}},
			Resources:       r.Spec.Exporter.Resources,
			LivenessProbe:   &corev1.Probe{Handler: corev1.Handler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(exporterPort)}}},
			ReadinessProbe:  &corev1.Probe{Handler: corev1.Handler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(exporterPort)}}},
			ImagePullPolicy: corev1.PullAlways,
			SecurityContext: r.Spec.Exporter.SecurityContext,
		})

		if r.Spec.Password.SecretKeyRef != nil {
			containers[1].Env = append(containers[1].Env, corev1.EnvVar{
				Name:      "REDIS_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: r.Spec.Password.SecretKeyRef},
			})
		}
	}

	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: generateName(r), Namespace: r.Namespace, Labels: r.Labels, Annotations: map[string]string{}},
		Spec: appsv1.StatefulSetSpec{
			Replicas: r.Spec.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: r.Labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      r.Labels,
					Annotations: r.Spec.Annotations,
				},
				Spec: corev1.PodSpec{
					Volumes:           volumes,
					Containers:        containers,
					SecurityContext:   r.Spec.SecurityContext,
					Affinity:          r.Spec.Affinity,
					Tolerations:       r.Spec.Tolerations,
					PriorityClassName: r.Spec.PriorityClassName,
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
			ServiceName:          fmt.Sprintf("%s-%s", generateName(r), headlessServiceTypeLabel),
		},
	}

	// compute the hash of the generated Statefulset and add it as the annotation
	hash, err := hashObject(s)
	if err != nil {
		// Failing to calculate the hash should not prevent normal operation.
		// The risk is next to zero anyway.
		hash = fmt.Sprintf("failed to calculate revision hash: %s", err)
	}
	s.Annotations[hashAnnotationKey] = hash

	return generateName(r), s
}

// state checkers
func secretUpdateNeeded(got, want *corev1.Secret) (needed bool) {
	if !mapsEqual(got.Labels, want.Labels) {
		got.Labels = want.Labels
		needed = true
	}
	if !reflect.DeepEqual(got.Data, want.Data) {
		got.Data = want.Data
		needed = true
	}
	return
}

func configMapUpdateNeeded(got, want *corev1.ConfigMap) (needed bool) {
	if !mapsEqual(got.Labels, want.Labels) {
		got.Labels = want.Labels
		needed = true
	}
	if !strings.Contains(got.Data[configFileName], want.Data[configFileName]) {
		got.Data = want.Data
		needed = true
	}
	return
}

func serviceUpdateNeeded(got, want *corev1.Service) (needed bool) {
	if !mapsEqual(got.Labels, want.Labels) {
		got.Labels = want.Labels
		needed = true
	}
	if !mapsEqual(got.Spec.Selector, want.Spec.Selector) {
		got.Spec.Selector = want.Spec.Selector
		needed = true
	}
	if !deepContains(got.Spec.Ports, want.Spec.Ports) {
		got.Spec.Ports = want.Spec.Ports
		needed = true
	}
	return
}

func podDisruptionBudgetUpdateNeeded(got, want *policyv1beta1.PodDisruptionBudget) (needed bool) {
	// updating PDB spec is forbidden
	// TODO: keep an eye on https://github.com/kubernetes/kubernetes/issues/45398
	if !mapsEqual(got.Labels, want.Labels) {
		got.Labels = want.Labels
		return true
	}
	return
}

func statefulSetUpdateNeeded(got, want *appsv1.StatefulSet) (needed bool) {
	if !mapsEqual(got.Labels, want.Labels) {
		got.Labels = want.Labels
		needed = true
	}
	if !mapsEqual(got.Annotations, want.Annotations) {
		got.Annotations = want.Annotations
		needed = true
	}

	if *got.Spec.Replicas != *want.Spec.Replicas {
		got.Spec.Replicas = want.Spec.Replicas
		needed = true
	}
	if !deepContains(got.Spec.Template, want.Spec.Template) {
		got.Spec.Template = want.Spec.Template
		needed = true
	}
	return
}

// generateName returns generic name for all owned resources.
// It should be used as a prefix for all resources requiring more specific naming scheme.
func generateName(r *k8sv1alpha1.Redis) string {
	return fmt.Sprintf(namePrefixTemplate, r.Name)
}

// mapsEqual compares two plain map[string]string values
func mapsEqual(a, b map[string]string) bool {
	return len(a) == len(b) && isSubset(a, b)
}

// isSubset checks if b is a subset of a
func isSubset(a, b map[string]string) bool {
	for k, valueB := range b {
		valueA, ok := a[k]
		if !ok || valueB != valueA {
			return false
		}
	}
	return true
}

// hashObject calculates sha256 value of a kubernetes runtime.Object encoded as a JSON string
func hashObject(object k8sruntime.Object) (string, error) {
	hash := sha256.New()
	defer hash.Reset()

	if err := json.NewEncoder(hash).Encode(object); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
