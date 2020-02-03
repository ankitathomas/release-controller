#!/bin/bash

state="success"
release=""

if [ $# -gt 0 ]; then
	if [ "|$1|" == "|F|" ]; then
	        state="failure"
        elif [ $# -eq 2 ] && [ "|$2|" == "|F|" ]; then
		state="failure"
	fi
	if [ "|$1|" != "|T|" ] && [ "|$1|" != "|F|" ]; then
		release=$1
		echo "1"
	elif [ $# -eq 2 ] && [ "|$2|" != "|T|" ] && [ "|$2|" != "|F|" ]; then
		release=$2
	fi
fi
if [ "|$release|" == "||" ]; then
	release=$(oc get pj -n release-controller-test-job --no-headers --sort-by=status.startTime -o custom-columns=:metadata.name,:status.state| grep "triggered" | awk '{print $1}' | sed 's/-[-a-z]*\(-[0-9]\+\.[0-9]\+\.[0-9]\+\)\?\(-[0-9]\)\?$//' | tail -n 1)
fi	       

if [ "|$release|" == "||" ]; then
	exit 0
fi

for i in $(oc get pj -n release-controller-test-job --no-headers | grep "$release"| awk '{print $1;}'); do 
	if [ $(oc get pj -n release-controller-test-job -o yaml $i | grep "state: "| grep -v "triggered" | wc -l | awk '{print $NF;}') -eq 0 ]; then
		oc get pj -n release-controller-test-job -o yaml $i | grep -v "completionTime: \|state: " | sed 's/  startTime:\(.*\)/  startTime:\1\n  completionTime:\1\n  state: '$state'/'> tmp.yaml
        	oc apply -n release-controller-test-job -f tmp.yaml 
	fi
done
