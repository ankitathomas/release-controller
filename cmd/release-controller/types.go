package main

import (
	"bytes"
	"fmt"
	"github.com/blang/semver"
	"strings"
	"time"

	imagev1 "github.com/openshift/api/image/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// APITag contains information about a release tag in a stream.
type APITag struct {
	// Name is the name of the tag. This is usually a semantic version.
	Name string `json:"name"`
	// Phase is the phase of the tag.
	Phase string `json:"phase"`
	// PullSpec can be used to retrieve the release image.
	PullSpec string `json:"pullSpec"`
	// DownloadURL is a link to the web page for downloading the tools.
	DownloadURL string `json:"downloadURL"`
}

// APIRelease contains information about a release stream.
type APIRelease struct {
	// Name is the name of the release stream.
	Name string `json:"name"`
	// Tags is a list of all tags in the release sorted by semantic version, oldest to newest.
	Tags []APITag `json:"tags"`
}

// Release holds information about the release used during processing.
type Release struct {
	// Source is the image stream that the Config was loaded from and holds all
	// images that will compose the release.
	Source *imagev1.ImageStream
	// Target is the image stream that the release tag will be pushed to. It is
	// modified and updated during processing by this controller to allow multiple
	// release modifications in a single 'sync' call.
	Target *imagev1.ImageStream
	// Config holds the release configuration parsed off of Source.
	Config *ReleaseConfig
}

// ReleaseConfig is serialized in JSON as the release.openshift.io/config annotation
// on image streams that wish to have release payloads generated from them. It modifies
// how the release is calculated.
type ReleaseConfig struct {
	// Name is a required field and is used to associate release tags back to the input.
	Name string `json:"name"`

	// Message is a markdown string that is injected at the top of the release listing
	// to describe the purpose of this stream.
	Message string `json:"message"`

	// Hide indicates this release should be visually less important on the status pages.
	Hide bool `json:"hide"`

	// As defines what this image stream provides. The default value is "Integration"
	// and the images in the image stream will be used to build payloads. An optional
	// mode is "Stable" and tags are assumed to be release payloads that should be promoted
	// and published elsewhere. When choosing Stable, a user will tag a candidate release
	// image in as a new tag to this image stream and the controller will rebuild and
	// update the image with the appropriate name, metadata, and content.
	As string `json:"as"`

	// To is the image stream where release tags will be created when the As field is
	// Integration. This field is ignored when As is Stable.
	To string `json:"to"`

	// MaxUnreadyReleases blocks creating new releases if there are more than this many
	// releases in non-terminal (Failed, Accepted, Rejected) states.
	MaxUnreadyReleases int `json:"maxUnreadyReleases"`

	// MinCreationIntervalSeconds controls how quickly multiple releases can be created.
	// Releases will be created no more rapidly than this interval.
	MinCreationIntervalSeconds int `json:"minCreationIntervalSeconds"`

	// ReferenceMode describes how the release image will refer to the origin. If empty
	// or 'public' images will be copied and no source location will be preserved. If
	// `source` then the controller will attempt to keep the originating reference in place.
	ReferenceMode string `json:"referenceMode"`

	// PullSecretName is the name of a pull secret in the release job namespace to mount
	// into the pod that will create the release. The secret must contain a single file
	// config.json with a valid Docker auths array.
	PullSecretName string `json:"pullSecretName"`

	// MirrorPrefix is the prefix applied to the release mirror image stream. If unset,
	// MirrorPrefix is the name of the source image stream + the date.
	MirrorPrefix string `json:"mirrorPrefix"`

	// OverrideCLIImage may be used to override the location where the CLI image is
	// located for actions on this image stream. It is useful when a bug prevents a
	// historical image from being used with newer functionality.
	OverrideCLIImage string `json:"overrideCLIImage"`

	// Expires is the amount of time as a golang duration before Accepted release tags
	// should be expired and removed. If unset, tags are not expired.
	Expires Duration `json:"expires"`

	// CandidateTests is a map of short names to tests that are run on all terminal releases
	// marked with keep annotation.
	CandidateTests map[string]*ReleaseCandidateTest `json:"candidateTests"`

	// Verify is a map of short names to verification steps that must succeed before the
	// release is Accepted. Failures for some job types will cause the release to be
	// rejected.
	Verify map[string]ReleaseVerification `json:"verify"`

	// Publish is a map of short names to publish steps that will be performed after
	// the release is Accepted. Some publish steps are continuously maintained, others
	// may only be performed once.
	Publish map[string]ReleasePublish `json:"publish"`

	// Check is a map of short names to check routines that report additional information
	// about the health or quality of this stream to the user interface.
	Check map[string]ReleaseCheck `json:"check"`
}

type ReleaseCheck struct {
	// ConsistentImages verifies that the images in this release have not drifted
	// significantly from the referenced parent and that no significant disparities
	// exist.
	ConsistentImages *CheckConsistentImages `json:"consistentImages"`
}

type CheckConsistentImages struct {
	// Parent is the release stream to compare against.
	Parent string `json:"parent"`
}

// ReleasePublish defines one action to take when a release is Accepted.
type ReleasePublish struct {
	// Disabled will prevent this publish step from being run.
	Disabled bool `json:"disabled"`
	// TagRef updates the named tag in the release image stream to point at the release.
	TagRef *PublishTagReference `json:"tagRef"`
	// ImageStreamRef copies all images to another image stream in one transaction.
	ImageStreamRef *PublishStreamReference `json:"imageStreamRef"`
}

// PublishTagReference ensures that the release image stream has a tag that points to
// the most recent release.
type PublishTagReference struct {
	// Name is the name of the release image stream tag that will be updated to point to
	// (reference) the release tag.
	Name string `json:"name"`
}

// PublishStreamReference updates another image stream with spec tags that reference the
// images that were verified.
type PublishStreamReference struct {
	// Name is the name of the release image stream to update. Required.
	Name string `json:"name"`
	// Namespace is the namespace of the release image stream to update. If left empty
	// it will default to the same namespace as the release image stream.
	Namespace string `json:"namespace"`
	// Tags if set will limit the set of tags that are published.
	Tags []string `json:"tags"`
	// ExcludeTags if set will explicitly not publish these tags. Is applied after the
	// tags field is checked.
	ExcludeTags []string `json:"excludeTags"`
}

// ReleaseVerification is a task that must be completed before a release is marked
// as Accepted. When some tasks fail the release will be marked as Rejected.
type ReleaseVerification struct {
	// Disabled will prevent this verification from being considered as blocking
	Disabled bool `json:"disabled"`
	// Optional verifications are run, but failures will not cause the release to
	// be rejected.
	Optional bool `json:"optional"`
	// Upgrade is true if this verification should be used to verify upgrades.
	// The default UpgradeFrom for stable streams is PreviousMicro and the default
	// for other types of streams is Previous.
	Upgrade bool `json:"upgrade"`
	// UpgradeFrom, if set, describes a different default upgrade source. The supported
	// values are:
	//
	// Previous - selects the latest accepted tag from the current stream
	// PreviousMicro - selects the latest accepted patch version from the current minor
	//   version (4.2.1 will select the latest accepted 4.2.z tag).
	// PreviousMinor - selects the latest accepted patch version from the previous minor
	//   version (4.2.1 will select the latest accepted 4.1.z tag).
	// RallyPoint - selects all tags after last rally point (latest multiple of 10 patch
	//     version) from current minor version
	//
	// If no matching target exists the job will be a no-op.
	UpgradeFrom string `json:"upgradeFrom"`

	// ProwJob requires that the named ProwJob from the prow config pass before the
	// release is accepted. The job is run only one time and if it fails the release
	// is rejected.
	ProwJob *ProwJobVerification `json:"prowJob"`
	// Maximum retry attempts for the job. Defaults to 0 - do not retry on fail
	MaxRetries int `json:"maxRetries,omitempty"`
}

type ReleaseCandidateTest struct {
	ReleaseVerification
	// UpgradeTag is the tag that upgrade should be run from.
	// If empty, upgrade will run from previous accepted ImageStreamTag.
	// Ignored if Upgrade is not true
	UpgradeTag string `json:"upgradeTag"`
	// UpgradeRef is the ref that the upgrade should be run from.
	// If empty, upgrade will run from previous accepted ImageStreamTag.
	// Ignored if Upgrade is not true
	UpgradeRef    string        `json:"upgradeRef"`
	RetryStrategy RetryStrategy `json:"retryStrategy"`
}

type RetryStrategy string

const (
	// Run till MaxRetries
	RetryStrategyTillMaxRetries RetryStrategy = "TillMaxRetries"
	// Run till first success, or till MaxRetries limit is reached
	RetryStrategyFirstSuccess RetryStrategy = "FirstSuccess"
	DefaultRetryStrategy                    = RetryStrategyFirstSuccess
)

// ProwJobVerification identifies the name of a prow job that will be used to
// validate the release.
type ProwJobVerification struct {
	// Name of the prow job to verify.
	Name string `json:"name"`
}

type VerifyJobStatus struct {
	State string `json:"state"`
	URL   string `json:"url"`
}

type VerificationStatus struct {
	VerifyJobStatus
	Retries        int          `json:"retries,omitempty"`
	TransitionTime *metav1.Time `json:"transitionTime,omitempty"`
}

type CandidateTestStatus struct {
	Status         []*VerifyJobStatus
	TransitionTime *metav1.Time
}

type VerificationStatusMap map[string]*VerificationStatus

type VerificationStatusList map[string]*CandidateTestStatus

type ReleasePromoteJobParameters struct {
	// Parameters for promotion job described at
	// https://github.com/openshift/aos-cd-jobs/blob/master/jobs/build/release/Jenkinsfile#L20-L81
	// Imagestream tag which is to be promoted to the new release
	FromTag string `json:"fromTag"`
	// Name of new release to be created by the promote job
	Name string `json:"name"`
	// Optional: versions this can upgrade from
	UpgradeFrom []string `json:"upgradeFrom,omitempty"`
}

type ReleaseCandidate struct {
	ReleasePromoteJobParameters
	CreationTime string                `json:"creationTime,omitempty"`
	Tag          *imagev1.TagReference `json:"tag,omitempty"`
}

type ReleaseCandidateList struct {
	Items []*ReleaseCandidate `json:"items"`
}

func (m VerificationStatusMap) Failures() ([]string, bool) {
	var names []string
	for name, s := range m {
		if s.State == releaseVerificationStateFailed {
			names = append(names, name)
		}
	}
	return names, len(names) > 0
}

func (m VerificationStatusMap) Incomplete(required map[string]ReleaseVerification) ([]string, bool) {
	var names []string
	for name, definition := range required {
		if definition.Disabled {
			continue
		}
		if s, ok := m[name]; !ok || !stringSliceContains([]string{releaseVerificationStateSucceeded, releaseVerificationStateFailed}, s.State) {
			names = append(names, name)
		}
	}
	return names, len(names) > 0
}

func verificationJobsWithRetries(jobs map[string]ReleaseVerification, result VerificationStatusMap) ([]string, bool) {
	var names []string
	blockingJobFailure := false
	for name, definition := range jobs {
		if definition.Disabled {
			continue
		}
		s, ok := result[name]
		if !ok {
			names = append(names, name)
			continue
		}
		if !stringSliceContains([]string{releaseVerificationStateFailed}, s.State) {
			continue
		}
		if s.Retries >= definition.MaxRetries {
			if !definition.Optional {
				blockingJobFailure = true
			}
			continue
		}
		names = append(names, name)
	}
	return names, blockingJobFailure
}

func allOptional(all map[string]ReleaseVerification, names ...string) bool {
	for _, name := range names {
		if v, ok := all[name]; ok && !v.Optional {
			return false
		}
	}
	return true
}

func (m VerificationStatusList) Incomplete(required map[string]*ReleaseCandidateTest) []string {
	var names []string
	for name, definition := range required {
		if definition.Disabled {
			continue
		}

		if _, ok := m[name]; !ok {
			names = append(names, name)
			continue
		}
		testResults := make([]*CandidateTestStatus, 0)
		if test, ok := m[name]; ok {
			testResults = append(testResults, test)
		} else if definition.Upgrade && definition.UpgradeFrom == releaseUpgradeFromRallyPoint {
			// Check whether all upgrades have completed for rally point upgrades
			for testName, test := range m {
				if !strings.HasPrefix(testName, name+"-") {
					continue
				}
				if _, err := semver.ParseTolerant(testName[len(name)+1:]); err != nil {
					continue
				}
				testResults = append(testResults, test)
			}
		}

		maxRetries := definition.MaxRetries
		retryStrategy := definition.RetryStrategy
		if len(retryStrategy) == 0 {
			retryStrategy = DefaultRetryStrategy
		}

		for _, result := range testResults {
			var completed int
			for _, s := range result.Status {
				if !stringSliceContains([]string{releaseVerificationStateSucceeded, releaseVerificationStateFailed}, s.State) {
					continue
				}
				if s.State == releaseVerificationStateSucceeded && retryStrategy == RetryStrategyFirstSuccess {
					completed = maxRetries + 1
					break
				}
				completed++
			}
			if completed >= maxRetries+1 {
				continue
			}
			names = append(names, name)
			break
		}
	}
	return names
}

func (t *ReleaseCandidateTest) state(status ...*VerifyJobStatus) string {
	retryStrategy := t.RetryStrategy
	if len(retryStrategy) == 0 {
		retryStrategy = DefaultRetryStrategy
	}
	if t.RetryStrategy == RetryStrategyTillMaxRetries && len(status) < t.MaxRetries+1 {
		return releaseVerificationStatePending
	}
	success := 0
	for _, s := range status {
		switch s.State {
		case releaseVerificationStatePending:
			return s.State
		case releaseVerificationStateSucceeded:
			if t.RetryStrategy == RetryStrategyFirstSuccess {
				return s.State
			}
			success++
		}
	}
	if success >= len(status)/2 {
		// TODO: add success threshold for tests
		return releaseVerificationStateSucceeded
	}
	return releaseVerificationStateFailed
}

const (
	// releasePhasePending is assigned to release tags that are waiting for an update
	// payload image to be created and pushed.
	//
	// This phase may transition to Failed or Ready.
	releasePhasePending = "Pending"
	// releasePhaseFailed occurs when an update payload image cannot be created for
	// a given set of image mirrors.
	//
	// This phase is a terminal phase. Pending is the only input phase.
	releasePhaseFailed = "Failed"
	// releasePhaseReady represents an image tag that has a valid update payload image
	// created and pushed to the release image stream. It may not have completed all
	// possible verification.
	//
	// This phase may transition to Accepted or Rejected. Pending is the only input phase.
	releasePhaseReady = "Ready"
	// releasePhaseAccepted represents an image tag that has passed its verification
	// criteria and can safely be promoted to an external location.
	//
	// This phase is a terminal phase. Ready is the only input phase.
	releasePhaseAccepted = "Accepted"
	// releasePhaseRejected represents an image tag that has failed one or more of the
	// verification criteria.
	//
	// The controller will take no more action in this phase, but a human may set the
	// phase back to Ready to retry and the controller will attempt verification again.
	releasePhaseRejected = "Rejected"

	releaseVerificationStateSucceeded = "Succeeded"
	releaseVerificationStateFailed    = "Failed"
	releaseVerificationStatePending   = "Pending"

	releaseConfigModeStable = "Stable"

	releaseUpgradeFromPreviousMinor = "PreviousMinor"
	releaseUpgradeFromPreviousPatch = "PreviousPatch"
	releaseUpgradeFromPrevious      = "Previous"
	releaseUpgradeFromRallyPoint    = "RallyPoint"

	// releaseAnnotationConfig is the JSON serialized representation of the ReleaseConfig
	// struct. It is only accepted on image streams. An image stream with this annotation
	// is considered an input image stream for creating releases.
	releaseAnnotationConfig = "release.openshift.io/config"

	releaseAnnotationKeep              = "release.openshift.io/keep"
	releaseAnnotationGeneration        = "release.openshift.io/generation"
	releaseAnnotationSource            = "release.openshift.io/source"
	releaseAnnotationTarget            = "release.openshift.io/target"
	releaseAnnotationName              = "release.openshift.io/name"
	releaseAnnotationReleaseTag        = "release.openshift.io/releaseTag"
	releaseAnnotationImageHash         = "release.openshift.io/hash"
	releaseAnnotationPhase             = "release.openshift.io/phase"
	releaseAnnotationCreationTimestamp = "release.openshift.io/creationTimestamp"
	releaseAnnotationVerify            = "release.openshift.io/verify"
	// if true, the release controller should rewrite this release
	releaseAnnotationRewrite = "release.openshift.io/rewrite"
	// an image stream with this annotation holds release tags
	releaseAnnotationHasReleases = "release.openshift.io/hasReleases"
	// if set, when rewriting a stable tag use the images locally
	releaseAnnotationMirrorImages = "release.openshift.io/mirrorImages"
	// when set on a job, controls which queue the job is notified on
	releaseAnnotationJobPurpose = "release.openshift.io/purpose"

	releaseAnnotationReason  = "release.openshift.io/reason"
	releaseAnnotationMessage = "release.openshift.io/message"
	releaseAnnotationLog     = "release.openshift.io/log"

	releaseAnnotationFromTag = "release.openshift.io/from-tag"
	releaseAnnotationToTag   = "release.openshift.io/tag"
	// releaseAnnotationFromImageStream specifies the imagestream
	// a release was promoted from. It has the format <namespace>/<imagestream name>
	releaseAnnotationFromImageStream = "release.openshift.io/from-image-stream"

	releaseAnnotationCandidateTests = "release.openshift.io/candidate-tests"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	if len(data) == 4 && bytes.Equal(data, []byte("null")) {
		return nil
	}
	if len(data) < 2 {
		return fmt.Errorf("invalid duration")
	}
	if data[0] != '"' || data[len(data)-1] != '"' {
		return fmt.Errorf("duration must be a string")
	}
	value, err := time.ParseDuration(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// tagReferencesByAge returns the newest tag first, the oldest tag last
type tagReferencesByAge []*imagev1.TagReference

func (a tagReferencesByAge) Less(i, j int) bool {
	return a[j].Annotations[releaseAnnotationCreationTimestamp] < a[i].Annotations[releaseAnnotationCreationTimestamp]
}
func (a tagReferencesByAge) Len() int      { return len(a) }
func (a tagReferencesByAge) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func calculateBackoff(retryCount int, initialTime, currentTime *metav1.Time) time.Duration {
	var backoffDuration time.Duration
	if retryCount < 1 {
		return backoffDuration
	}
	backoff := wait.Backoff{
		Duration: time.Minute * 1,
		Factor:   2,
		Jitter:   0,
		Steps:    retryCount,
		Cap:      time.Minute * 15,
	}
	for backoff.Steps > 0 {
		backoff.Step()
	}
	backoffDuration = backoff.Duration
	if initialTime != nil {
		backoffDuration += initialTime.Sub(currentTime.Time)
	}
	if backoffDuration < 0 {
		backoffDuration = 0
	}
	return backoffDuration
}
