package main

import (
	"encoding/json"
	"fmt"

	"github.com/golang/glog"

	imagev1 "github.com/openshift/api/image/v1"
)

func (c *Controller) ensureVerificationJobs(release *Release, releaseTag *imagev1.TagReference) (VerificationStatusMap, error) {
	var verifyStatus VerificationStatusMap
	for name, verifyType := range release.Config.Verify {
		if verifyType.Disabled {
			glog.V(2).Infof("Release verification step %s is disabled, ignoring", name)
			continue
		}

		switch {
		case verifyType.ProwJob != nil:
			if verifyStatus == nil {
				if data := releaseTag.Annotations[releaseAnnotationVerify]; len(data) > 0 {
					verifyStatus = make(VerificationStatusMap)
					if err := json.Unmarshal([]byte(data), &verifyStatus); err != nil {
						glog.Errorf("Release %s has invalid verification status, ignoring: %v", releaseTag.Name, err)
					}
				}
			}

			if status, ok := verifyStatus[name]; ok {
				switch status.State {
				case releaseVerificationStateFailed, releaseVerificationStateSucceeded:
					// we've already processed this, continue
					continue
				case releaseVerificationStatePending:
					// we need to process this
				default:
					glog.V(2).Infof("Unrecognized verification status %q for type %s on release %s", status.State, name, releaseTag.Name)
				}
			}

			job, err := c.ensureProwJobForReleaseTag(release, name, verifyType, releaseTag)
			if err != nil {
				return nil, err
			}
			status, ok := prowJobVerificationStatus(job)
			if !ok {
				return nil, fmt.Errorf("unexpected error accessing prow job definition")
			}
			if status.State == releaseVerificationStateSucceeded {
				glog.V(2).Infof("Prow job %s for release %s succeeded, logs at %s", name, releaseTag.Name, status.URL)
			}
			if verifyStatus == nil {
				verifyStatus = make(VerificationStatusMap)
			}
			verifyStatus[name] = status

		default:
			// manual verification
		}
	}
	return verifyStatus, nil
}

func (c *Controller) ensureAdditionalTests(release *Release, releaseTag *imagev1.TagReference) (ValidationStatusMap, error) {
	var verifyStatus ValidationStatusMap
	if verifyStatus == nil {
		if data := releaseTag.Annotations[releaseAnnotationValidate]; len(data) > 0 {
			verifyStatus = make(ValidationStatusMap)
			if err := json.Unmarshal([]byte(data), &verifyStatus); err != nil {
				glog.Errorf("Release %s has invalid verification status, ignoring: %v", releaseTag.Name, err)
			}
		}
	}

	retryCount := 1
	additionalTests, err := c.upgradeJobs(release, releaseTag, retryCount)
	if err != nil {
		return verifyStatus, err
	}

	for name, additionalTest := range release.Config.AdditionalTests {
		additionalTests[name] = additionalTest
	}

	for name, testType := range additionalTests {
		if testType.Disabled {
			glog.V(2).Infof("Release additional test step %s is disabled, ignoring", name)
			continue
		}
		switch {
		case testType.ProwJob != nil:
			switch testType.Retry.RetryStrategy {
			case RetryStrategyTillRetryCount:
				// process this, ensure minimum number of results
			default:
				glog.Errorf("Release %s has invalid test %s: unrecognized retry strategy %s", releaseTag.Name, name, testType.Retry.RetryStrategy)
				continue
			}
			jobNo := 0
			if _, ok := verifyStatus[name]; ok {
				//number of times we have run this job
				jobNo = len(verifyStatus[name])

				// See if there are pending jobs. if yes, try to get their status.
				for i, status := range verifyStatus[name] {
					switch status.State {
					case releaseVerificationStateFailed, releaseVerificationStateSucceeded:
						// we've already processed this, continue
						continue
					case releaseVerificationStatePending:
						// we need to process this
						jobNo = i
						break
					default:
						glog.V(2).Infof("Unrecognized verification status %q for type %s on release %s", status.State, name, releaseTag.Name)
					}
				}
			}
			if jobNo < testType.Retry.RetryCount {
				jobName := fmt.Sprintf("%s-%d", name, jobNo)
				job, err := c.ensureProwJobForAdditionalTest(release, jobName, testType, releaseTag)
				if err != nil {
					return nil, err
				}
				status, ok := prowJobVerificationStatus(job)
				if !ok {
					return nil, fmt.Errorf("unexpected error accessing prow job definition")
				}
				if status.State == releaseVerificationStateSucceeded {
					glog.V(2).Infof("Prow job %s for release %s succeeded, logs at %s", name, releaseTag.Name, status.URL)
				}
				if verifyStatus == nil {
					verifyStatus = make(ValidationStatusMap)
				}
				if len(verifyStatus[name]) <= jobNo {
					verifyStatus[name] = append(verifyStatus[name], status)
				} else {
					verifyStatus[name][jobNo] = status
				}
			}
		default:
			// manual verification
		}
	}

	return verifyStatus, nil
}

func (c *Controller) upgradeJobs(release *Release, releaseTag *imagev1.TagReference, retryCount int) (map[string]ReleaseAdditionalTest, error) {
	upgradeTests := make(map[string]ReleaseAdditionalTest)
	if releaseTag == nil || len(releaseTag.Annotations) == 0 || len(releaseTag.Annotations[releaseAnnotationKeep]) == 0 {
		return upgradeTests, nil
	}

	releaseVersion, err := semverParseTolerant(releaseTag.Name)
	if err != nil {
		return upgradeTests, nil
	}
	upgradesFound := make(map[string]int)
	upgrades := c.graph.UpgradesTo(releaseTag.Name)
	for _, u := range upgrades {
		upgradesFound[u.From]++
	}

	// Stable releases after the last rally point
	stable, err := c.stableReleases(true)
	if err != nil {
		return upgradeTests, err
	}
	prowJobPrefix := "e2e-aws-upgrade-"

	for _, r := range stable.Releases {
		releaseSource := fmt.Sprintf("%s/%s", r.Release.Source.Namespace, r.Release.Source.Name)
		for _, stableTag := range r.Release.Source.Spec.Tags {
			if stableTag.Annotations[releaseAnnotationPhase] != releasePhaseAccepted ||
				stableTag.Annotations[releaseAnnotationSource] != releaseSource {
				continue
			}

			if len(stableTag.Name) == 0 || upgradesFound[stableTag.Name] >= retryCount {
				continue
			}

			stableVersion, err := semverParseTolerant(stableTag.Name)
			if err != nil || len(stableVersion.Pre) != 0 || len(stableVersion.Build) != 0 {
				// Only accept stable releases of the for <Major>.<Minor>.<Patch> for upgrade tests
				continue
			}
			if stableVersion.Major != releaseVersion.Major || stableVersion.Minor != releaseVersion.Minor {
				continue
			}

			fromImageStream, _ := c.findImageStreamByAnnotations(map[string]string{releaseAnnotationReleaseTag: stableTag.Name})
			if fromImageStream == nil {
				glog.Errorf("Unable to find image repository for %s", stableTag.Name)
				continue
			}
			if len(fromImageStream.Status.PublicDockerImageRepository) == 0 {
				continue
			}

			prowJobName := fmt.Sprintf("%s%d.%d", prowJobPrefix, stableVersion.Major, stableVersion.Minor)
			testName := fmt.Sprintf("%s.%d", prowJobName, stableVersion.Patch)
			upgradeTests[testName] = ReleaseAdditionalTest{
				ReleaseVerification: ReleaseVerification{
					Disabled: false,
					Optional: true,
					Upgrade:  true,
					ProwJob:  &ProwJobVerification{Name: prowJobName},
				},
				UpgradeTag: stableTag.Name,
				UpgradeRef: fromImageStream.Status.PublicDockerImageRepository,
				Retry: &RetryPolicy{
					RetryStrategy: RetryStrategyTillRetryCount,
					RetryCount:    retryCount,
				},
			}
		}
	}

	return upgradeTests, nil
}
