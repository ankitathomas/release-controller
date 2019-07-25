package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	imagev1 "github.com/openshift/api/image/v1"

	prowapiv1 "github.com/openshift/release-controller/pkg/prow/apiv1"
)

func (c *Controller) ensureProwJobForReleaseTag(release *Release, verifyName string, verifyType ReleaseVerification, releaseTag *imagev1.TagReference) (*unstructured.Unstructured, error) {
	jobName := verifyType.ProwJob.Name
	// Name must be limited to 63 characters
	prowJobName := fmt.Sprintf("%s-%s", releaseTag.Name, verifyName)
	if len(prowJobName) > 63 {
		prowJobName = namespaceSafeHash(prowJobName)[:20]
	}

	obj, exists, err := c.prowLister.GetByKey(fmt.Sprintf("%s/%s", c.prowNamespace, prowJobName))
	if err != nil {
		return nil, err
	}
	if exists {
		// TODO: check metadata on object
		return obj.(*unstructured.Unstructured), nil
	}

	config := c.prowConfigLoader.Config()
	if config == nil {
		err := fmt.Errorf("the prow job %s is not valid: no prow jobs have been defined", jobName)
		c.eventRecorder.Event(release.Source, corev1.EventTypeWarning, "ProwJobInvalid", err.Error())
		return nil, terminalError{err}
	}
	periodicConfig, ok := hasProwJob(config, jobName)
	if !ok {
		err := fmt.Errorf("the prow job %s is not valid: no job with that name", jobName)
		c.eventRecorder.Eventf(release.Source, corev1.EventTypeWarning, "ProwJobInvalid", err.Error())
		return nil, terminalError{err}
	}
	spec := prowSpecForPeriodicConfig(periodicConfig, config.Plank.DefaultDecorationConfig)

	env, annotations, labels, err := c.buildKVMapsForProwJob(spec, release, releaseTag, map[string]string{}, verifyName)
	if err != nil {
		return nil, err
	}

	ok, err = addReleaseEnvToProwJobSpec(spec, env)
	if err != nil {
		return nil, err
	}
	if !ok {
		now := metav1.Now()
		// return a synthetic job to indicate that this test is impossible to run (no spec, or
		// this is an upgrade job and no upgrade is possible)
		return objectToUnstructured(&prowapiv1.ProwJob{
			TypeMeta: metav1.TypeMeta{APIVersion: "prow.k8s.io/v1", Kind: "ProwJob"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        prowJobName,
				Annotations: annotations,
				Labels:      labels,
			},
			Spec: *spec,
			Status: prowapiv1.ProwJobStatus{
				StartTime:      now,
				CompletionTime: &now,
				Description:    "Job was not defined or does not have any inputs",
				State:          prowapiv1.SuccessState,
			},
		}), nil
	}

	pj := &prowapiv1.ProwJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "prow.k8s.io/v1", Kind: "ProwJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        prowJobName,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: *spec,
		Status: prowapiv1.ProwJobStatus{
			StartTime: metav1.Now(),
			State:     prowapiv1.TriggeredState,
		},
	}
	if !verifyType.Upgrade {
		delete(pj.Annotations, releaseAnnotationFromTag)
	}

	out, err := c.ensureProwJob(prowJobName, pj)
	if _, ok := err.(terminalError); ok {
		c.eventRecorder.Eventf(release.Source, corev1.EventTypeWarning, "ProwJobInvalid", "the prow job %s is not valid: %v", pj.Name, err)
	}
	return out, err
}

func (c *Controller) ensureProwJobForAdditionalTest(release *Release, testName string, testType ReleaseAdditionalTest, releaseTag *imagev1.TagReference) (*unstructured.Unstructured, error) {
	jobName := testType.ProwJob.Name
	// Name must be limited to 63 characters
	prowJobName := fmt.Sprintf("%s-%s", releaseTag.Name, testName)
	if len(prowJobName) > 63 {
		prowJobName = namespaceSafeHash(prowJobName)[:20]
	}

	obj, exists, err := c.prowLister.GetByKey(fmt.Sprintf("%s/%s", c.prowNamespace, prowJobName))
	if err != nil {
		return nil, err
	}
	if exists {
		// TODO: check metadata on object
		return obj.(*unstructured.Unstructured), nil
	}

	config := c.prowConfigLoader.Config()
	if config == nil {
		err := fmt.Errorf("the prow job %s is not valid: no prow jobs have been defined", jobName)
		c.eventRecorder.Event(release.Source, corev1.EventTypeWarning, "ProwJobInvalid", err.Error())
		return nil, terminalError{err}
	}
	periodicConfig, ok := hasProwJob(config, jobName)
	if !ok {
		err := fmt.Errorf("the prow job %s is not valid: no job with that name", jobName)
		c.eventRecorder.Eventf(release.Source, corev1.EventTypeWarning, "ProwJobInvalid", err.Error())
		return nil, terminalError{err}
	}
	spec := prowSpecForPeriodicConfig(periodicConfig, config.Plank.DefaultDecorationConfig)

	params := make(map[string]string)
	if testType.Upgrade {
		params["RELEASE_IMAGE_INITIAL"] = testType.UpgradeRef
		params[releaseAnnotationFromTag] = testType.UpgradeTag
	}

	env, annotations, labels, err := c.buildKVMapsForProwJob(spec, release, releaseTag, params, testName)
	if err != nil {
		return nil, err
	}

	ok, err = addReleaseEnvToProwJobSpec(spec, env)
	if err != nil {
		return nil, err
	}
	if !ok {
		now := metav1.Now()
		// return a synthetic job to indicate that this test is impossible to run (no spec, or
		// this is an upgrade job and no upgrade is possible)
		return objectToUnstructured(&prowapiv1.ProwJob{
			TypeMeta: metav1.TypeMeta{APIVersion: "prow.k8s.io/v1", Kind: "ProwJob"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        prowJobName,
				Annotations: annotations,
				Labels:      labels,
			},
			Spec: *spec,
			Status: prowapiv1.ProwJobStatus{
				StartTime:      now,
				CompletionTime: &now,
				Description:    "Job was not defined or does not have any inputs",
				State:          prowapiv1.SuccessState,
			},
		}), nil
	}

	pj := &prowapiv1.ProwJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "prow.k8s.io/v1", Kind: "ProwJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        prowJobName,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: *spec,
		Status: prowapiv1.ProwJobStatus{
			StartTime: metav1.Now(),
			State:     prowapiv1.TriggeredState,
		},
	}
	if !testType.Upgrade {
		delete(pj.Annotations, releaseAnnotationFromTag)
	}

	out, err := c.ensureProwJob(prowJobName, pj)
	if _, ok := err.(terminalError); ok {
		c.eventRecorder.Eventf(release.Source, corev1.EventTypeWarning, "ProwJobInvalid", "the prow job %s is not valid: %v", pj.Name, err)
	}
	return out, err
}

func (c *Controller) ensureProwJob(prowJobName string, pj *prowapiv1.ProwJob) (*unstructured.Unstructured, error) {
	if len(prowJobName) == 0 {
		prowJobName = pj.Name
	}
	out, err := c.prowClient.Create(objectToUnstructured(pj), metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// find a cached version or do a live call
		job, exists, err := c.prowLister.GetByKey(fmt.Sprintf("%s/%s", c.prowNamespace, pj.Name))
		if err != nil {
			return nil, err
		}
		if exists {
			return job.(*unstructured.Unstructured), nil
		}
		return c.prowClient.Get(prowJobName, metav1.GetOptions{})
	}
	if errors.IsInvalid(err) {
		return nil, terminalError{err}
	}
	if err != nil {
		return nil, err
	}
	glog.V(2).Infof("Created new prow job %s", pj.Name)
	return out, nil

}

func objectToUnstructured(obj runtime.Object) *unstructured.Unstructured {
	buf := &bytes.Buffer{}
	if err := unstructured.UnstructuredJSONScheme.Encode(obj, buf); err != nil {
		panic(err)
	}
	u := &unstructured.Unstructured{}
	if _, _, err := unstructured.UnstructuredJSONScheme.Decode(buf.Bytes(), nil, u); err != nil {
		panic(err)
	}
	return u
}

// Generate the annotations, labels and container env for a prowjob
func (c *Controller) buildKVMapsForProwJob(spec *prowapiv1.ProwJobSpec, release *Release, releaseTag *imagev1.TagReference, params map[string]string, jobName string) (env, annotations, labels map[string]string, err error) {
	env = make(map[string]string)
	labels = make(map[string]string)
	annotations = make(map[string]string)
	if release == nil {
		return env, annotations, labels, fmt.Errorf("release cannot be nil")
	}
	if releaseTag == nil {
		return env, annotations, labels, fmt.Errorf("releaseTag cannot be nil")
	}
	if spec == nil {
		return env, annotations, labels, fmt.Errorf("spec cannot be nil")
	}

	labels["prow.k8s.io/type"] = string(spec.Type)
	labels["prow.k8s.io/job"] = spec.Job
	labels[releaseAnnotationVerify] = "true"

	annotations["prow.k8s.io/job"] = spec.Job
	annotations[releaseAnnotationToTag] = releaseTag.Name
	if release.Source != nil {
		annotations[releaseAnnotationSource] = release.Source.Namespace + "/" + release.Source.Name
	}

	if len(release.Target.Status.PublicDockerImageRepository) > 0 {
		env["RELEASE_IMAGE_LATEST"] = release.Target.Status.PublicDockerImageRepository + ":" + releaseTag.Name
	}

	if previousReleasePullSpec, ok := params["RELEASE_IMAGE_INITIAL"]; ok {
		env["RELEASE_IMAGE_INITIAL"] = previousReleasePullSpec
	}
	if previousReleaseFromTag, ok := params[releaseAnnotationFromTag]; ok {
		annotations[releaseAnnotationFromTag] = previousReleaseFromTag
	}

	if len(env["RELEASE_IMAGE_INITIAL"]) == 0 || len(annotations[releaseAnnotationFromTag]) == 0 {
		if tags := findTagReferencesByPhase(release, releasePhaseAccepted); len(tags) > 0 {
			annotations[releaseAnnotationFromTag] = tags[0].Name
			env["RELEASE_IMAGE_INITIAL"] = release.Target.Status.PublicDockerImageRepository + ":" + tags[0].Name
		}
	}
	env["NAMESPACE"] = fmt.Sprintf("ci-ln-%s", namespaceSafeHash(jobName)[:10])
	env["CLUSTER_DURATION"] = "7200"

	mirror, _ := c.getMirror(release, releaseTag.Name)
	if len(mirror.Status.PublicDockerImageRepository) != 0 {
		env["IMAGE_FORMAT"] = mirror.Status.PublicDockerImageRepository + ":${component}"
		env["IMAGE_"] = mirror.Status.PublicDockerImageRepository + ":"
	}
	return env, annotations, labels, nil
}

func addReleaseEnvToProwJobSpec(spec *prowapiv1.ProwJobSpec, env map[string]string) (bool, error) {
	if spec.PodSpec == nil {
		// Jenkins jobs cannot be parameterized
		return true, nil
	}
	for i := range spec.PodSpec.Containers {
		c := &spec.PodSpec.Containers[i]
		for j := range c.Env {
			switch name := c.Env[j].Name; {
			case name == "RELEASE_IMAGE_LATEST", name == "RELEASE_IMAGE_INITIAL", name == "NAMESPACE", name == "CLUSTER_DURATION":
				if value, ok := env[name]; ok {
					c.Env[j].Value = value
				} else {
					return false, nil
				}
			case name == "IMAGE_FORMAT":
				if value, ok := env[name]; ok {
					c.Env[j].Value = value
				} else {
					return false, fmt.Errorf("unable to determine %s for prow job %s", name, spec.Job)
				}
			case strings.HasPrefix(name, "IMAGE_"):
				suffix := strings.TrimPrefix(name, "IMAGE_")
				if len(suffix) == 0 {
					break
				}
				suffix = strings.ToLower(strings.Replace(suffix, "_", "-", -1))
				if value, ok := env["IMAGE_"]; ok {
					c.Env[j].Value = value + suffix
				} else {
					return false, fmt.Errorf("unable to determine IMAGE_FORMAT for prow job %s", spec.Job)
				}
			}
		}
	}
	return true, nil
}

func hasProwJob(config *prowapiv1.Config, name string) (*prowapiv1.Periodic, bool) {
	for i := range config.Periodics {
		if config.Periodics[i].Name == name {
			return &config.Periodics[i], true
		}
	}
	return nil, false
}

func prowSpecForPeriodicConfig(config *prowapiv1.Periodic, decorationConfig *prowapiv1.DecorationConfig) *prowapiv1.ProwJobSpec {
	spec := &prowapiv1.ProwJobSpec{
		Type:  prowapiv1.PeriodicJob,
		Job:   config.Name,
		Agent: prowapiv1.KubernetesAgent,

		Refs: &prowapiv1.Refs{},

		PodSpec: config.Spec.DeepCopy(),
	}

	if decorationConfig != nil {
		spec.DecorationConfig = decorationConfig.DeepCopy()
	} else {
		spec.DecorationConfig = &prowapiv1.DecorationConfig{}
	}
	isTrue := true
	spec.DecorationConfig.SkipCloning = &isTrue

	return spec
}

// oneWayEncoding can be used to encode hex to a 62-character set (0 and 1 are duplicates) for use in
// short display names that are safe for use in kubernetes as resource names.
var oneWayNameEncoding = base32.NewEncoding("bcdfghijklmnpqrstvwxyz0123456789").WithPadding(base32.NoPadding)

func namespaceSafeHash(values ...string) string {
	hash := sha512.New()

	// the inputs form a part of the hash
	for _, s := range values {
		hash.Write([]byte(s))
	}

	// Object names can't be too long so we truncate
	// the hash. This increases chances of collision
	// but we can tolerate it as our input space is
	// tiny.
	return oneWayNameEncoding.EncodeToString(hash.Sum(nil)[:])
}
