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

func (c *Controller) ensureValidationJobs(release *Release, releaseTag *imagev1.TagReference) (ValidationStatusMap, error) {
	var verifyStatus ValidationStatusMap
	if verifyStatus == nil {
		if data := releaseTag.Annotations[releaseAnnotationValidate]; len(data) > 0 {
			verifyStatus = make(ValidationStatusMap)
			if err := json.Unmarshal([]byte(data), &verifyStatus); err != nil {
				glog.Errorf("Release %s has invalid verification status, ignoring: %v", releaseTag.Name, err)
			}
		}
	}

	for name, verifyType := range release.Config.Test {
		if verifyType.Disabled {
			glog.V(2).Infof("Release additional validation step %s is disabled, ignoring", name)
			continue
		}
		switch {
		case verifyType.ProwJob != nil:
			switch verifyType.Retry.RetryStrategy {
			case RetryStrategyTillRetryCount:
				// process this, ensure minimum number of results
			default:
				glog.Errorf("Release %s has invalid test %s: unrecognized retry strategy %s", releaseTag.Name, name, verifyType.Retry.RetryStrategy)
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
			if jobNo < verifyType.Retry.RetryCount {
				jobName := fmt.Sprintf("%s-%d", name, jobNo)
				job, err := c.ensureProwJobForReleaseTag(release, jobName, verifyType, releaseTag)
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
