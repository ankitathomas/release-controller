package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/blang/semver"
	"github.com/golang/glog"
	"github.com/gorilla/mux"

	"k8s.io/apimachinery/pkg/labels"

	imagev1 "github.com/openshift/api/image/v1"
)

const candidatePageHtml = `
{{ range $stream, $list := . }}
<h1>Release Candidates for {{ nextReleaseName $list }}</h1>
<hr>
<style>
.upgrade-track-line {
	position: absolute;
	top: 0;
	bottom: -1px;
	left: 7px;
	width: 0;
	display: inline-block;
	border-left: 2px solid #000;
	display: none;
	z-index: 200;
}
.upgrade-track-dot {
	display: inline-block;
	position: absolute;
	top: 15px;
	left: 2px;
	width: 12px;
	height: 12px;
	background: #fff;
	z-index: 300;
	cursor: pointer;
}
.upgrade-track-dot {
	border: 2px solid #000;
	border-radius: 50%;
}
.upgrade-track-dot:hover {
	border-width: 6px;
}
.upgrade-track-line.start {
	top: 18px;
	height: 31px;
	display: block;
}
.upgrade-track-line.middle {
	display: block;
}
.upgrade-track-line.end {
	top: -1px;
	height: 16px;
	display: block;
}
td.upgrade-track {
	width: 16px;
	position: relative;
	padding-left: 2px;
	padding-right: 2px;
}
</style>
<div class="row">
<div class="col">
	<table class="table text-nowrap">
		<thead>
			<tr>
				<th title="Candidate tag for next release">Name</th>
				<th title="Tag(s) of release this can upgrade FROM">Upgrades</th>
				<th title="Creation time">Creation time</th>
			</tr>
		</thead>
		<tbody>
		{{ range $candidate := $list.Items }}
			<tr>
				<td> <a href="/releasestream/releasetag/{{ $candidate.FromTag }}" >{{ $candidate.FromTag }} </a></td>
				<td>{{ range $prev := $candidate.UpgradeFrom }}
					<a href="/releasestream/{{ $prev }}/release/{{ $prev }}"> {{ $prev }} </a>, 
					{{ end }}
				</td>
				<td>{{ $candidate.CreationTime }}</td>
			</tr>
		{{ end }}
		</tbody>
	</table>
</div>
</div>
{{ end }}
`

func (c *Controller) httpReleaseCandidateList(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	defer func() { glog.V(4).Infof("rendered in %s", time.Now().Sub(start)) }()
	vars := mux.Vars(req)
	releaseStreamName := vars["release"]
	releaseCandidateList, err := c.findReleaseCandidates(releaseStreamName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	switch req.URL.Query().Get("format") {
	case "json":
		data, err := json.MarshalIndent(&releaseCandidateList, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, string(data))
	default:
		fmt.Fprintf(w, htmlPageStart, "Release Status")
		page := template.Must(template.New("candidatePage").Funcs(
			template.FuncMap{
				"nextReleaseName": func(list *ReleaseCandidateList) string {
					if list == nil || list.Items == nil || len(list.Items) == 0 {
						return "next release"
					}
					return list.Items[0].Name
				},
			},
		).Parse(candidatePageHtml))

		if err := page.Execute(w, releaseCandidateList); err != nil {
			glog.Errorf("Unable to render page: %v", err)
		}
		fmt.Fprintln(w, htmlPageEnd)
	}
}

func (c *Controller) apiReleaseCandidate(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	defer func() { glog.V(4).Infof("rendered in %s", time.Now().Sub(start)) }()
	vars := mux.Vars(req)
	releaseStreamName := vars["release"]
	releaseCandidateList, err := c.findReleaseCandidates(releaseStreamName)
	if err != nil {
		if err == errStreamNotFound {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if releaseCandidateList[releaseStreamName] == nil {
		http.Error(w, errStreamNotFound.Error(), http.StatusNotFound)
		return
	}

	switch req.URL.Query().Get("format") {
	default:
		var candidate *ReleasePromoteJobParameters
		if len(releaseCandidateList[releaseStreamName].Items) != 0 {
			releaseCandidate := releaseCandidateList[releaseStreamName].Items[0]
			upgradeSuccess := make([]string, 0)
			upgrades := c.graph.UpgradesTo(releaseCandidate.FromTag)
			successPercent := 80.0
			for _, u := range upgrades {
				if u.Total == 0 {
					continue
				}
				if float64(100*u.Success)/float64(u.Total) > successPercent {
					upgradeSuccess = append(upgradeSuccess, u.From)
				}
			}
			sort.Strings(upgradeSuccess)
			releaseCandidate.UpgradeFrom = upgradeSuccess
			candidate = &(releaseCandidate.ReleasePromoteJobParameters)
		}
		data, err := json.MarshalIndent(map[string]*ReleasePromoteJobParameters{"candidate": candidate}, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		fmt.Fprintln(w)
	}
}

// z-stream and timestamp
var rePreviousReleaseName = regexp.MustCompile("(?P<STREAM>.*)-(?P<TIMESTAMP>[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6})")

func (c *Controller) findReleaseCandidates(releaseStreams ...string) (map[string]*ReleaseCandidateList, error) {
	releaseCandidates := make(map[string]*ReleaseCandidateList)
	if len(releaseStreams) == 0 {
		return releaseCandidates, nil
	}

	releaseStreamTagMap, ok := c.findReleaseByName(true, releaseStreams...)
	if !ok || len(releaseStreamTagMap) == 0 {
		return releaseCandidates, errStreamNotFound
	}

	// next release version name for each release stream
	next := make(map[string]*semver.Version)
	// creation time of nightly/image that latest stable release in release stream was promoted from
	latestPromotedTime := make(map[string]int64)

	type stableRef struct {
		from string
		name string
		time int64
	}
	stableReleases := make([]stableRef, 0)

	stable, err := c.stableReleases()
	if err != nil {
		return releaseCandidates, err
	}
	for _, r := range stable.Releases {
		for _, tag := range r.Release.Source.Spec.Tags {
			if tag.Annotations[releaseAnnotationSource] != fmt.Sprintf("%s/%s", r.Release.Source.Namespace, r.Release.Source.Name) {
				continue
			}
			if _, err := semverParseTolerant(tag.Name); err == nil {
				t, _ := time.Parse(time.RFC3339, tag.Annotations[releaseAnnotationCreationTimestamp])
				stableReleases = append(stableReleases,
					stableRef{
						from: tag.From.Name,
						name: tag.Name,
						time: t.Unix(),
					})
			}
		}
	}
	sort.Slice(stableReleases, func(i, j int) bool {
		vi, _ := semverParseTolerant(stableReleases[i].name)
		vj, _ := semverParseTolerant(stableReleases[j].name)
		return vi.GT(vj)
	})
	remaining := len(releaseStreams)
	for _, r := range stableReleases {
		if remaining == 0 {
			break
		}
		v, _ := semverParseTolerant(r.name)

		// Check if the stable version's <MAJOR>.<MINOR> matches any release stream that we are processing
		found := false
		for _, stream := range releaseStreams {
			streamVersion, _ := semverParseTolerant(stream)
			if next[stream] == nil && streamVersion.Major == v.Major && streamVersion.Minor == v.Minor {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		// Call oc adm release info to get previous nightly info for the stable release
		op, err := c.releaseInfo.ReleaseInfo(r.from)
		if err != nil {
			// releaseinfo not found, old tag
			return releaseCandidates, fmt.Errorf("Could not get release info for tag %s: %v", r.from, err)
		}

		type releaseInfoShort struct {
			Image      string               `json:"image"`
			References *imagev1.ImageStream `json:"references"`
		}

		releaseInfo := releaseInfoShort{}
		if err := json.Unmarshal([]byte(op), &releaseInfo); err != nil {
			return releaseCandidates, fmt.Errorf("Could not unmarshal release info for tag %s: %v", r.from, err)
		}
		latestPromotedFrom := releaseInfo.References.Annotations[releaseAnnotationFromRelease]
		// latestPromotedFrom has the format <registry>:<previous nightly> or <previous nightly>
		// e.g: registry.svc.ci.openshift.org/ocp/release:4.1.0-0.nightly-2019-07-18-192922
		// Removing prefix
		if idx := strings.LastIndex(latestPromotedFrom, ":"); idx != -1 {
			latestPromotedFrom = latestPromotedFrom[idx+1:]
		}

		// Find the creation time and the stream for the nightly this stable release was promoted from
		var timeString, stream string
		prevTags, _ := c.findReleaseStreamTags(false, latestPromotedFrom)
		if prevTags[latestPromotedFrom] != nil &&
			prevTags[latestPromotedFrom].Tag.Annotations[releaseAnnotationCreationTimestamp] != "" {
			// Use previous release stream tags, if available
			stream = prevTags[latestPromotedFrom].Tag.Annotations[releaseAnnotationName]
			timeString = prevTags[latestPromotedFrom].Tag.Annotations[releaseAnnotationCreationTimestamp]
		} else if rePreviousReleaseName.MatchString(latestPromotedFrom) {
			//timestamps from elsewhere
			// Try to use name format of previous nightly to find release stream and timestamp
			stream = rePreviousReleaseName.ReplaceAllString(latestPromotedFrom, "${STREAM}")
			// Use creation time of latest stable release instead
			timeString = releaseInfo.References.CreationTimestamp.Format(time.RFC3339)
		} else {
			glog.Errorf("Could not find tag %s , tag may have been deleted", latestPromotedFrom)
			continue
		}

		// Check if selected stream belongs to any we are interested in
		found = false
		for _, s := range releaseStreams {
			if stream == s {
				found = true
				break
			}
		}
		if !found {
			// The stable release belongs to a release stream we are not processing
			continue
		}

		pt, err := time.Parse(time.RFC3339, timeString)
		if err != nil {
			glog.Errorf("Unable to parse timestamp %s for %s: %v", timeString, latestPromotedFrom, err)
			continue
		}
		remaining--
		latestPromotedTime[stream] = pt.Unix()
		next[stream] = &v
	}

	for _, stream := range releaseStreams {
		nextReleaseName := ""
		if next[stream] != nil {
			nextVersion, _ := incrementSemanticVersion(*next[stream])
			nextReleaseName = nextVersion.String()
		}
		candidates := make([]*ReleaseCandidate, 0)
		releaseTags := tagsForRelease(releaseStreamTagMap[stream].Release)
		for _, tag := range releaseTags {
			if tag.Annotations != nil && tag.Annotations[releaseAnnotationPhase] == releasePhaseAccepted &&
				tag.Annotations[releaseAnnotationCreationTimestamp] != "" {
				t, _ := time.Parse(time.RFC3339, tag.Annotations[releaseAnnotationCreationTimestamp])
				ts := t.Unix()
				if ts > latestPromotedTime[stream] {
					candidates = append(candidates, &ReleaseCandidate{
						ReleasePromoteJobParameters: ReleasePromoteJobParameters{
							FromTag: tag.Name,
							Name:    nextReleaseName,
						},
						CreationTime: time.Unix(ts, 0).Format(time.RFC3339),
						Tag:          tag,
					})
				}
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].CreationTime > candidates[j].CreationTime
		})
		releaseCandidates[stream] = &ReleaseCandidateList{Items: candidates}
	}
	return releaseCandidates, nil
}

func (c *Controller) findReleaseByName(includeStableTags bool, names ...string) (map[string]*ReleaseStreamTag, bool) {
	needed := make(map[string]*ReleaseStreamTag)
	for _, name := range names {
		if len(name) == 0 {
			continue
		}
		needed[name] = nil
	}
	remaining := len(needed)

	imageStreams, err := c.imageStreamLister.ImageStreams(c.releaseNamespace).List(labels.Everything())
	if err != nil {
		return nil, false
	}

	var stable *StableReferences
	if includeStableTags {
		stable = &StableReferences{}
	}

	for _, stream := range imageStreams {
		r, ok, err := c.releaseDefinition(stream)
		if err != nil || !ok {
			continue
		}

		if includeStableTags {
			if version, err := semverParseTolerant(r.Config.Name); err == nil || r.Config.As == releaseConfigModeStable {
				stable.Releases = append(stable.Releases, StableRelease{
					Release: r,
					Version: version,
				})
			}
		}
		if includeStableTags && remaining == 0 {
			continue
		}

		matched := false
		for _, name := range names {
			if r.Config.Name == name {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		needed[r.Config.Name] = &ReleaseStreamTag{
			Release: r,
			Stable:  stable,
		}
		remaining--
		if !includeStableTags && remaining == 0 {
			return needed, true
		}
	}
	if includeStableTags {
		sort.Sort(stable.Releases)
	}
	return needed, remaining == 0
}

// TODO: Add support for returning stable releases after rally point
func (c *Controller) stableReleases() (*StableReferences, error) {
	imageStreams, err := c.imageStreamLister.ImageStreams(c.releaseNamespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	stable := &StableReferences{}

	for _, stream := range imageStreams {
		r, ok, err := c.releaseDefinition(stream)
		if err != nil || !ok {
			continue
		}

		if r.Config.As == releaseConfigModeStable {
			version, _ := semverParseTolerant(r.Source.Name)
			stable.Releases = append(stable.Releases, StableRelease{
				Release: r,
				Version: version,
			})
		}
	}

	sort.Sort(stable.Releases)
	return stable, nil
}
