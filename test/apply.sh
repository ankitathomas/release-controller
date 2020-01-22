#!/bin.bash
if [ -f ./release-head.txt ]; then
	j=$(cat release-head.txt)
fi

j=$(( ( $j + 1 ) % 4 ));
oc apply -n release-controller-test-release -f "release-$j.yaml";

echo $j > release-head.txt
