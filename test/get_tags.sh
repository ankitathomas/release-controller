
if [ $? -eq 0 ]; then
	exit
fi

oc get -n ocp is "$1" -o yaml | grep "annotations:" -A 8 | grep "name: \(cli\|cluster-version-operator\|insights-operator\|tests\)$" -B 6 -A 2 | grep -v "\--"
