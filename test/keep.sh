#!/bin/bash

oc get is -n release-controller-test-release release -o yaml | grep -v " (creationTimestamp\|generation\|selfLink\|uid\|release.openshift.io/keep):" | sed 's/release.openshift.io\/name: \(.*\)/release.openshift.io\/name: \1\n      release.openshift.io\/keep: \1/' | grep -v "keep:.*stable" | sed -n '/^status:$/q;p' > keep.yaml

oc apply -n release-controller-test-release -f keep.yaml
